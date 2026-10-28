package market

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/billing"
)

// b501Connect is Stripe Connect with a live key that every seller's account is enabled at. Like Stripe under the
// idempotency key LiveStripe sends ("market-payout-<id>"), it makes one transfer per payout however often it is asked.
type b501Connect struct {
	mu        sync.Mutex
	transfers []string
	asked     map[string]bool
}

func (*b501Connect) Livemode() bool { return true }
func (*b501Connect) CreateConnectedAccount(context.Context, string, string, string) (billing.ConnectAccount, error) {
	return billing.ConnectAccount{}, nil
}
func (*b501Connect) OnboardingLink(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (*b501Connect) ConnectedAccount(_ context.Context, id string) (billing.ConnectAccount, error) {
	return billing.ConnectAccount{ID: id, DetailsSubmitted: true, PayoutsEnabled: true}, nil
}
func (c *b501Connect) TransferToSeller(_ context.Context, _ string, _ int64, payoutID, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.asked[payoutID] {
		c.asked[payoutID] = true
		c.transfers = append(c.transfers, payoutID)
	}
	return "tr_" + payoutID, nil
}

// b501Meter accepts every use and counts them, safely from many goroutines.
type b501Meter struct {
	mu       sync.Mutex
	accepted []string
}

func (m *b501Meter) MeterMarketUse(_ context.Context, _, useID string, _ int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accepted = append(m.accepted, useID)
	return nil
}

type b501Capabilities struct{}

func (b501Capabilities) RequireBilledCapability(context.Context, string, string) error { return nil }

// b501AtOnce runs fn 20 times at once and waits for all of them.
func b501AtOnce(fn func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn()
		}()
	}
	close(start)
	wg.Wait()
}

// b501JournalBalances fails the test unless every market journal entry sums to zero per currency and funding.
func b501JournalBalances(t *testing.T, s *Store) {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT 1 FROM market_journal_postings
		GROUP BY entry_id, currency, funding HAVING sum(amount_usd_micros) <> 0) x`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d market journal entries do not sum to zero per currency and funding (%v)", n, err)
	}
}

// B50.1 — the same marketplace purchase, earnings payout run or credit take sent 20 times at once moves money once: one
// licence and one billed row, one payout and one transfer, one credit take — each with its one journal entry.
func TestB501_TwentyMarketRequestsAtOnceMoveMoneyOnce(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	s := NewStore(pool)
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("marketplace purchase", func(t *testing.T) {
		const seller, buyer = "ws-b501-seller", "ws-b501-buyer"
		for _, ws := range []string{seller, buyer} {
			if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
				t.Fatal(err)
			}
		}
		l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", Visibility: "public",
			Artifact: []byte(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
			Offers:   []Offer{{Kind: OfferBuy, Licence: LicenceCommercial, PriceUSDMicros: 9_000_000}}})
		if err != nil {
			t.Fatal(err)
		}
		meter := &b501Meter{}
		deps := LicenceDeps{Meter: meter, Capabilities: b501Capabilities{}}
		b501AtOnce(func() {
			_, _, _ = s.License(ctx, deps, buyer, "", l.ID, "buy-once", LicenceRequest{OfferID: l.Offers[0].ID})
		})
		if n := count(`SELECT count(*) FROM market_licences WHERE buyer_workspace_id = $1`, buyer); n != 1 {
			t.Fatalf("20 purchases at once under one key made %d licences, want 1", n)
		}
		if n := count(`SELECT count(*) FROM market_uses WHERE buyer_workspace_id = $1 AND charge = 'billed'`, buyer); n != 1 || len(meter.accepted) != 1 {
			t.Fatalf("20 purchases at once billed %d rows and metered %v, want one", n, meter.accepted)
		}
	})

	// Earnings of $42.50 to a live seller, cleared on a live invoice 30 days ago (past the holdback).
	earn := func(t *testing.T, seller, buyer string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, details_submitted, payouts_enabled)
			VALUES ($1, 'acct_'||$1, true, true)`, seller); err != nil {
			t.Fatal(err)
		}
		paid := time.Now().Add(-30 * 24 * time.Hour)
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at) VALUES ('use_'||$1, 'lst_b501', 1, $1, $2, 500000000, 'billed', $3, $3, $3)`, seller, buyer, paid.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ClearInvoice(ctx, buyer, "in_"+seller, paid.Add(-2*time.Hour), paid, paid, true); err != nil || n != 1 {
			t.Fatalf("clear the invoice = %d, %v", n, err)
		}
	}

	t.Run("earnings payout", func(t *testing.T) {
		const seller = "ws-b501-payout"
		earn(t, seller, "ws-b501-payout-buyer")
		api := &b501Connect{asked: map[string]bool{}}
		now := time.Now()
		b501AtOnce(func() { _, _ = s.PayOut(ctx, api, now) })
		if n := count(`SELECT count(*) FROM market_payouts WHERE workspace_id = $1`, seller); n != 1 || len(api.transfers) != 1 {
			t.Fatalf("20 payout runs at once made %d payouts and sent %v, want one payout sent once", n, api.transfers)
		}
		if n := count(`SELECT count(*) FROM market_journal_entries WHERE kind = 'payout' AND ref IN (SELECT id FROM market_payouts WHERE workspace_id = $1)`, seller); n != 1 {
			t.Fatalf("the payout has %d journal entries, want 1", n)
		}
		b501JournalBalances(t, s)
	})

	t.Run("earnings taken as credits", func(t *testing.T) {
		const seller = "ws-b501-credits"
		earn(t, seller, "ws-b501-credits-buyer")
		now := time.Now()
		b501AtOnce(func() { _, _ = s.TakeAsCredits(ctx, noCredits{}, seller, now) })
		if n := count(`SELECT count(*) FROM market_payouts WHERE workspace_id = $1`, seller); n != 1 {
			t.Fatalf("20 credit takes at once made %d payouts, want 1", n)
		}
		if n := count(`SELECT count(*) FROM market_journal_entries WHERE kind = 'credits' AND ref IN (SELECT id FROM market_payouts WHERE workspace_id = $1)`, seller); n != 1 {
			t.Fatalf("the credit take has %d journal entries, want 1", n)
		}
		b501JournalBalances(t, s)
	})
}
