package market

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// noCredits takes earnings as credits without an LXC ledger: the journal is what these tests read.
type noCredits struct{}

func (noCredits) CreditLXCTx(context.Context, pgx.Tx, string, int64, string, map[string]interface{}) (int64, error) {
	return 0, nil
}

// reconciles fails the test unless seller's journal holdback and available balances equal what SellerEarnings and
// SellerPayouts say at now, and JournalCheck finds nothing.
func reconciles(t *testing.T, s *Store, seller string, now time.Time, after string) {
	t.Helper()
	ctx := context.Background()
	e, err := s.SellerEarnings(ctx, seller, now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.SellerPayouts(ctx, nil, seller, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.journalCheck(ctx, seller, now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("after %s, %s does not reconcile: %v", after, seller, r.Mismatches)
	}
	holdback, available := journalBalance(t, s, SellerHoldback(seller)), journalBalance(t, s, SellerAvailable(seller))
	due := r.PendingReleaseUSDMicros // past the holdback, not yet moved by the release job
	if -holdback-due != e.InHoldbackUSDMicros || -holdback-due != p.InHoldbackUSDMicros ||
		-available+due != e.AvailableUSDMicros-e.OwedUSDMicros || -available+due != p.AvailableUSDMicros-p.OwedUSDMicros {
		t.Fatalf("after %s, %s's journal holds %d in holdback and %d available (%d due); SellerEarnings says %d, %d (owed %d); SellerPayouts %d, %d (owed %d)",
			after, seller, -holdback, -available, due, e.InHoldbackUSDMicros, e.AvailableUSDMicros, e.OwedUSDMicros,
			p.InHoldbackUSDMicros, p.AvailableUSDMicros, p.OwedUSDMicros)
	}
}

// B32.17 — 200 random clears, refunds, releases, payouts and credit takes across four seeded sellers, over ten
// months: after every one, each seller's journal holdback and available balances equal SellerEarnings and
// SellerPayouts, and JournalCheck finds nothing.
func TestEscrow_TheJournalReconcilesThroughRandomSalesRefundsReleasesAndPayouts(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	sellers := []string{"ws-b3217-s1", "ws-b3217-s2", "ws-b3217-s3", "ws-b3217-s4"}
	buyers := []string{"ws-b3217-b1", "ws-b3217-b2", "ws-b3217-b3"}
	for i, ws := range sellers {
		if _, err := pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, details_submitted, payouts_enabled)
			VALUES ($1, $2, true, true)`, ws, fmt.Sprintf("acct_b3217_%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(3217))
	now := time.Now().Add(-300 * 24 * time.Hour).UTC().Truncate(time.Microsecond)
	type sale struct{ use, invoice, seller string }
	var cleared []sale
	stripe := &connectLive{}
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		now = now.Add(time.Duration(rng.Intn(36)+1) * time.Hour)
		var op string
		switch k := rng.Intn(10); {
		case k < 4:
			op = "clear"
			seller, buyer := sellers[rng.Intn(len(sellers))], buyers[rng.Intn(len(buyers))]
			id, invoice := fmt.Sprintf("use_b3217_%d", i), fmt.Sprintf("in_b3217_%d", i)
			billedUse(t, pool, id, seller, buyer, int64(rng.Intn(1_000)+1)*1_000_000, now.Add(-time.Minute)) // 1 to 1,000 LXC
			if n, err := s.ClearInvoice(ctx, buyer, invoice, now.Add(-2*time.Minute), now, now, true); err != nil || n != 1 {
				t.Fatalf("op %d: clear = %d, %v", i, n, err)
			}
			cleared = append(cleared, sale{id, invoice, seller})
		case k < 6 && len(cleared) > 0:
			op = "refund"
			x := cleared[rng.Intn(len(cleared))]
			if _, _, err := s.ReverseInvoice(ctx, x.invoice, "buyer_refund", "re_"+x.use, 0, true); err != nil {
				t.Fatalf("op %d: refund %s: %v", i, x.use, err)
			}
		case k < 8:
			op = "release"
			if _, err := s.ReleaseDue(ctx, now); err != nil {
				t.Fatalf("op %d: release: %v", i, err)
			}
		case k < 9:
			op = "payout"
			if _, err := s.PayOut(ctx, stripe, now); err != nil {
				t.Fatalf("op %d: payout: %v", i, err)
			}
		default:
			op = "credits"
			if _, err := s.TakeAsCredits(ctx, noCredits{}, sellers[rng.Intn(len(sellers))], now); err != nil && !errors.Is(err, ErrNothingAvailable) {
				t.Fatalf("op %d: take as credits: %v", i, err)
			}
		}
		counts[op]++
		for _, ws := range sellers {
			reconciles(t, s, ws, now, fmt.Sprintf("op %d (%s)", i, op))
		}
	}
	if _, err := s.ReleaseDue(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, ws := range sellers {
		reconciles(t, s, ws, now, "the last release")
	}
	var payouts, credits, owed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'payout'), count(*) FILTER (WHERE kind = 'credits'),
		(SELECT count(*) FROM market_journal_balances WHERE account LIKE 'seller:%:available' AND balance_usd_micros > 0)
		FROM market_journal_entries`).Scan(&payouts, &credits, &owed); err != nil {
		t.Fatal(err)
	}
	if payouts == 0 || credits == 0 || counts["refund"] == 0 || counts["release"] == 0 {
		t.Fatalf("the run journalled %d payouts and %d credit takes from %v; want every kind of operation exercised", payouts, credits, counts)
	}
	t.Logf("ops %v: %d payouts and %d credit takes journalled; %d sellers left owing", counts, payouts, credits, owed)
}

// B32.17 — the holdback is the escrow: an earning with an open hold is still in holdback 30 days after it cleared,
// while one cleared beside it was released; it is released the moment its hold is, and a hold can no longer be put
// on it. Taken as credits and then refunded, it leaves the seller's available below zero: owed.
func TestEscrow_AHeldEarningStaysInHoldbackUntilItsHoldIsReleased(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const seller, buyer = "ws-b3217-held", "ws-b3217-buyer"
	now := time.Now().UTC().Truncate(time.Microsecond)
	paid := now.Add(-30 * 24 * time.Hour)
	billedUse(t, pool, "use_held", seller, buyer, 100_000_000, paid.Add(-2*time.Hour)) // $10: $8.50 to the seller
	billedUse(t, pool, "use_free", seller, buyer, 20_000_000, paid.Add(-time.Hour))    // $2: $1.70
	if _, err := s.OpenHold(ctx, "use_held", HoldDispute, buyer, paid.Add(-time.Minute)); err != nil {
		t.Fatalf("a hold on a use not yet cleared: %v", err)
	}
	if n, err := s.ClearInvoice(ctx, buyer, "in_held", paid.Add(-3*time.Hour), paid, paid, true); err != nil || n != 2 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	if n, err := s.ReleaseDue(ctx, now); err != nil || n != 1 {
		t.Fatalf("30 days on, ReleaseDue released %d (%v); want only the unheld earning", n, err)
	}
	e, err := s.SellerEarnings(ctx, seller, now)
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]string{}
	for _, x := range e.Earnings {
		held[x.UseID] = x.HeldFor
	}
	if e.InHoldbackUSDMicros != 8_500_000 || e.AvailableUSDMicros != 1_700_000 || held["use_held"] != HoldDispute || held["use_free"] != "" ||
		journalBalance(t, s, SellerHoldback(seller)) != -8_500_000 || journalBalance(t, s, SellerAvailable(seller)) != -1_700_000 {
		t.Fatalf("30 days on: in holdback %d, available %d, held %v; journal %d and %d; want the held $8.50 still in holdback and $1.70 available",
			e.InHoldbackUSDMicros, e.AvailableUSDMicros, held, journalBalance(t, s, SellerHoldback(seller)), journalBalance(t, s, SellerAvailable(seller)))
	}
	reconciles(t, s, seller, now, "30 days with a hold")

	var holdID string
	if err := pool.QueryRow(ctx, `SELECT id FROM market_holds WHERE use_id = 'use_held'`).Scan(&holdID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseHold(ctx, holdID, "the seller delivered", now); err != nil {
		t.Fatal(err)
	}
	j, err := s.JournalFor(ctx, "use_held")
	if err != nil {
		t.Fatal(err)
	}
	if len(j) != 2 || j[1].Kind != JournalRelease || !j[1].CreatedAt.Equal(now) {
		t.Fatalf("the held use's journal after its hold was released = %+v; want its clear, then its release at %v", j, now)
	}
	if got := journalBalance(t, s, SellerAvailable(seller)); got != -10_200_000 || journalBalance(t, s, SellerHoldback(seller)) != 0 {
		t.Fatalf("after the release, available reads %d and holdback %d; want -10200000 and 0", got, journalBalance(t, s, SellerHoldback(seller)))
	}
	if _, err := s.OpenHold(ctx, "use_held", HoldIPClaim, "operator", now); !errors.Is(err, ErrOutOfHoldback) {
		t.Fatalf("a hold on a released earning = %v; want ErrOutOfHoldback", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM market_holds`); err == nil {
		t.Fatal("deleting a hold was allowed; the escrow's record must stay")
	}

	if _, err := s.TakeAsCredits(ctx, noCredits{}, seller, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReverseInvoice(ctx, "in_held", "chargeback", "dp_held", 10_000_000, false); err != nil {
		t.Fatal(err)
	}
	if got := journalBalance(t, s, SellerAvailable(seller)); got != 8_500_000 {
		t.Fatalf("a chargeback after the earnings were taken as credits leaves available at %d; want +8500000, owed", got)
	}
	reconciles(t, s, seller, now, "a chargeback after the payout")
}

// B32.17 — migration 0200 journals the history: earnings already past their holdback are released, and payouts and
// credit takes already made leave available, so a seller reconciles the moment it has run.
func TestEscrow_TheMigrationJournalsReleasesAndPayoutsAlreadyMade(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	// A second database migrated up to 0198, given history, then migrated to the end: 0199 journals the clears and
	// the refund, 0200 the releases and the payouts.
	conn, err := pgx.Connect(ctx, pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := "lens_b3217_backfill_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Database = name
	mc, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close(ctx)
	before := fstest.MapFS{}
	names, _ := fs.Glob(migrations.FS, "*.sql")
	for _, n := range names {
		if n < "0199" {
			b, _ := fs.ReadFile(migrations.FS, n)
			before[n] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := dbmigrate.Run(ctx, mc, before); err != nil {
		t.Fatal(err)
	}
	old, recent := time.Now().Add(-40*24*time.Hour), time.Now().Add(-2*24*time.Hour)
	for _, e := range []struct {
		use   string
		share int64
		paid  time.Time
	}{{"use_past", 30_000_000, old}, {"use_live", 20_000_000, old}, {"use_refunded", 5_000_000, old}, {"use_recent", 7_000_000, recent}} {
		if _, err := mc.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, fee_usd_micros, invoice_id,
			cleared_at, payable_at, livemode) VALUES ($1, 'ws-old', $2, $2, 0, 'in_old', $3, $4, $5)`,
			e.use, e.share, e.paid, e.paid.Add(Holdback), e.use == "use_live"); err != nil {
			t.Fatal(err)
		}
	}
	// The refund, a $30 Stripe payout and $10 taken as credits, half of it test money as its ledger row says.
	for _, q := range []string{
		`INSERT INTO market_refunds (use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc, gross_usd_micros,
			reversed_share_usd_micros, reason, cause) VALUES ('use_refunded', 'lst_old', 'ws-buyer', 'ws-old', 50000000, 5000000, 5000000, 'r', 'buyer_refund')`,
		`INSERT INTO market_payouts (id, workspace_id, method, month, gross_usd_micros, account_fee_usd_micros, payout_fee_usd_micros,
			net_usd_micros, stripe_account_id, created_at, livemode) VALUES ('mpo_old_stripe', 'ws-old', 'stripe', '2026-08', 30000000, 2000000, 320000, 27680000,
			'acct_old', now() - interval '20 days', false)`,
		`INSERT INTO market_payouts (id, workspace_id, method, month, gross_usd_micros, net_usd_micros, credits_ulxc, paid_at, created_at)
			VALUES ('mpo_old_credits', 'ws-old', 'credits', '2026-09', 10000000, 10000000, 100000000, now() - interval '10 days', now() - interval '10 days')`,
		`INSERT INTO lxc_ledger (workspace_id, amount, balance_after, type, metadata)
			VALUES ('ws-old', 100, 100, 'market_earnings', '{"market_payout_id": "mpo_old_credits", "test_funded_ulxc": 50000000}')`,
	} {
		if _, err := mc.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		t.Fatalf("migrating a database with history: %v", err)
	}
	p2, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	s := NewStore(p2)
	reconciles(t, s, "ws-old", time.Now(), "migration 0200")
	// $30 + $20 released, less the $30 payout and the $10 of credits: $10 available; the recent $7 still in holdback.
	if a, h := journalBalance(t, s, SellerAvailable("ws-old")), journalBalance(t, s, SellerHoldback("ws-old")); a != -10_000_000 || h != -7_000_000 {
		t.Fatalf("after the backfill, available reads %d and holdback %d; want -10000000 and -7000000", a, h)
	}
	var creditsTest, fees int64
	if err := p2.QueryRow(ctx, `SELECT
		(SELECT balance_usd_micros FROM market_journal_balances WHERE account = 'credits:issued' AND funding = 'test'),
		(SELECT balance_usd_micros FROM market_journal_balances WHERE account = 'stripe:connect_fees')`).Scan(&creditsTest, &fees); err != nil {
		t.Fatal(err)
	}
	if creditsTest != -5_000_000 || fees != -2_320_000 {
		t.Fatalf("credits:issued (test) reads %d and stripe:connect_fees %d; want -5000000 (the ledger's test half) and -2320000", creditsTest, fees)
	}
}
