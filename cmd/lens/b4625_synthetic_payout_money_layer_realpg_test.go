package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B46.25 — the synthetic payout run pays a test seller who saved verified payout details on the money layer there, as
// the weekly run does: one market_payouts row of method money, and earnings_payout rows under market:<week> — what its
// lineage royalties earned as a royalty, the rest as a sale. A test seller without details is still paid by
// tr_synthetic_. The run asked again pays nothing twice.
func TestB4625_TheSyntheticPayoutRunPaysATestSellerOnTheMoneyLayer(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const marketplace, seller, plain = "ws-b4625-marketplace", "ws-b4625-seller", "ws-b4625-plain"
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, reg)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetAccountPartners(reg)
	bank.SetPayeeScreener(screener)
	bank.SetScreener(screener)
	bank.SetMoneyLayer(sandboxMoneyLayer(t, pool, bank, reg))

	for _, ws := range []string{seller, plain} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	// B17.343 — the marketplace starts with no dollar account: the run opens it and brings in what the bill collected.
	if _, err := bank.SavePayoutDetails(ctx, seller, economy.PayoutDetails{Asset: "USD", Holder: "Sam Seller", IBAN: "GB82WEST12345698765432"}); err != nil {
		t.Fatal(err)
	}

	// Past their holdback: the seller's $30 sale and $10 lineage royalty; the plain seller's $30 sale.
	payable := time.Now().Add(-15 * 24 * time.Hour)
	for _, e := range []struct {
		use, ws, kind string
		micros        int64
	}{{"use_b4625_1", seller, market.EarningSale, 30_000_000}, {"use_b4625_2", seller, market.EarningLineage, 10_000_000},
		{"use_b4625_3", plain, market.EarningSale, 30_000_000}} {
		lineage := e.kind == market.EarningLineage
		if _, err := pool.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, invoice_id,
			cleared_at, payable_at, kind, source_ref, depth) VALUES ($1, $2, CASE WHEN $6 THEN 0 ELSE $3 END, $3, 'in_b4625', $4, $4, $5,
			CASE WHEN $6 THEN 'mle_b4625' ELSE '' END, CASE WHEN $6 THEN 1 ELSE 0 END)`, e.use, e.ws, e.micros, payable, e.kind, lineage); err != nil {
			t.Fatal(err)
		}
	}

	store := market.NewStore(pool)
	store.SetMoneyPayouts(bank, marketplace)
	now := time.Now()
	y, w := now.UTC().ISOWeek()
	week := fmt.Sprintf("market:%04d-W%02d", y, w)

	held, p, err := store.PayOutTestSeller(ctx, seller, now)
	if err != nil || held || p == nil || p.Method != market.PayoutMoney || p.Asset != "USD" || p.StripeTransferID != "" || p.PaidAt == nil ||
		p.NetUSDMicros != 40_000_000 || p.AccountFeeUSDMicros+p.PayoutFeeUSDMicros != 0 {
		t.Fatalf("the synthetic run pays the seller %+v (held %v, %v); want a paid USD money payout of $40.00 without Stripe fees", p, held, err)
	}
	if _, again, err := store.PayOutTestSeller(ctx, seller, now); err != nil || again != nil {
		t.Fatalf("the run asked again made %+v (%v), want nothing", again, err)
	}

	paid, err := bank.EarningsPayouts(ctx, marketplace)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]economy.EarningsPayout{}
	for _, e := range paid {
		got[e.RecipientWorkspaceID+" "+e.Kind] = e
		if e.Source != week {
			t.Errorf("payout %s's source is %q, want %s", e.ID, e.Source, week)
		}
	}
	if len(paid) != 2 || got[seller+" sale"].AmountMinor != 3000 || got[seller+" royalty"].AmountMinor != 1000 ||
		got[seller+" sale"].Status != economy.PaymentCompleted || got[seller+" royalty"].Status != economy.PaymentCompleted {
		t.Fatalf("the money layer paid %+v, want the seller's completed $30.00 sale and $10.00 royalty, once each", paid)
	}
	var acct string
	var collected int64
	if err := pool.QueryRow(ctx, `SELECT a.id, COALESCE((SELECT sum(p.amount_minor) FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		WHERE p.account_id = a.id AND e.kind = 'earnings_collected'), 0)::bigint FROM money_accounts a
		WHERE a.workspace_id = $1 AND a.purpose = 'company' AND a.currency = 'USD'`, marketplace).Scan(&acct, &collected); err != nil {
		t.Fatalf("the run opened no USD account for the marketplace: %v", err)
	}
	if b, err := bank.Balance(ctx, marketplace, acct); err != nil || collected != 4000 || b.TestMinor != 0 {
		t.Errorf("the marketplace's USD account collected %d and holds %+v (%v), want $40.00 collected and all of it paid out", collected, b, err)
	}

	// A test seller without payout details is still paid by a synthetic Stripe transfer, never on the money layer.
	if _, q, err := store.PayOutTestSeller(ctx, plain, now); err != nil || q == nil || q.Method != market.PayoutStripe ||
		!strings.HasPrefix(q.StripeTransferID, "tr_synthetic_") {
		t.Fatalf("the plain seller was paid %+v (%v), want a tr_synthetic_ Stripe payout", q, err)
	}
	if after, err := bank.EarningsPayouts(ctx, marketplace); err != nil || len(after) != 2 {
		t.Errorf("the money layer paid %d payouts after the plain seller's run (%v), want still 2", len(after), err)
	}
}
