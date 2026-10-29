package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B46.22 — the marketplace's weekly payout run pays a seller who saved payout details on the money layer through the
// sandbox instead of Stripe: their available balance once, what their lineage royalties earned as a royalty and the rest
// as a sale; a creator earning only royalties is paid them as a royalty; a seller whose details are not yet verified is
// held, not paid. The run again that week pays nothing more.
func TestB4622_TheWeeklyRunPaysSellersAndRoyaltiesOnTheMoneyLayerOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const marketplace, seller, creator, pending = "ws-b4622-marketplace", "ws-b4622-seller", "ws-b4622-creator", "ws-b4622-pending"
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, reg)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetAccountPartners(reg)
	bank.SetPayeeScreener(screener)
	bank.SetScreener(screener)
	bank.SetMoneyLayer(sandboxMoneyLayer(t, pool, bank, reg))

	// The marketplace's own accounts hold $100.00 and £100.00 to pay sellers from.
	accounts := map[string]string{}
	for currency, minor := range map[string]int64{"USD": 10_000, "GBP": 10_000} {
		acct, err := bank.OpenCurrencyAccount(ctx, marketplace, "", currency, "test")
		if err != nil {
			t.Fatal(err)
		}
		accounts[currency] = acct.ID
		var partnerAccount string
		if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = $2`, marketplace, currency).
			Scan(&partnerAccount); err != nil {
			t.Fatal(err)
		}
		if _, err := bank.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: marketplace, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
			IdempotencyKey: "top-up-" + currency, Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
				{AccountID: partnerAccount, AmountMinor: -minor}, {AccountID: acct.ID, AmountMinor: minor}}}); err != nil {
			t.Fatal(err)
		}
	}
	for ws, d := range map[string]economy.PayoutDetails{
		seller:  {Asset: "USD", Holder: "Sam Seller", IBAN: "GB82 WEST 1234 5698 7654 32"},
		creator: {Asset: "GBP", Holder: "Cora Creator", IBAN: "GB82WEST12345698765432"},
		pending: {Asset: "USD", Holder: "TESTPENDING Pat", IBAN: "GB82WEST12345698765432"},
	} {
		if _, err := bank.SavePayoutDetails(ctx, ws, d); err != nil {
			t.Fatalf("%s saves its payout details: %v", ws, err)
		}
	}

	// Earnings past their holdback: the seller's $30 sale and $10 lineage royalty, the creator's $26.55 of royalties
	// and the pending seller's $40 sale. The creator is paid in pounds at yesterday's ECB rates: 0.85 ÷ 1.25 = £0.68 a dollar.
	if _, err := pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES
		(CURRENT_DATE - 1, 'USD', 1.2500), (CURRENT_DATE - 1, 'GBP', 0.8500)`); err != nil {
		t.Fatal(err)
	}
	payable := time.Now().Add(-15 * 24 * time.Hour)
	for _, e := range []struct {
		use, ws, kind string
		micros        int64
	}{{"use_b4622_1", seller, market.EarningSale, 30_000_000}, {"use_b4622_2", seller, market.EarningLineage, 10_000_000},
		{"use_b4622_3", creator, market.EarningLineage, 26_550_000}, {"use_b4622_4", pending, market.EarningSale, 40_000_000}} {
		lineage := e.kind == market.EarningLineage // a royalty: the edge that paid it, a generation up, of no gross of its own
		if _, err := pool.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, invoice_id,
			cleared_at, payable_at, kind, source_ref, depth) VALUES ($1, $2, CASE WHEN $6 THEN 0 ELSE $3 END, $3, 'in_b4622', $4, $4, $5,
			CASE WHEN $6 THEN 'mle_b4622' ELSE '' END, CASE WHEN $6 THEN 1 ELSE 0 END)`, e.use, e.ws, e.micros, payable, e.kind, lineage); err != nil {
			t.Fatal(err)
		}
	}

	store := market.NewStore(pool)
	store.SetMoneyPayouts(bank, marketplace)
	store.SetDisplayRates(ecbrate.New(pool, ""))
	now := time.Now()
	y, w := now.UTC().ISOWeek()
	week := fmt.Sprintf("market:%04d-W%02d", y, w)
	for run := range 2 {
		if _, err := store.PayOut(ctx, nil, now); err != nil {
			t.Fatalf("payout run %d: %v", run+1, err)
		}
	}

	paid, err := bank.EarningsPayouts(ctx, marketplace)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]economy.EarningsPayout{}
	for _, p := range paid {
		got[p.RecipientWorkspaceID+" "+p.Kind] = p
		if p.Source != week {
			t.Errorf("payout %s's source is %q, want the payout week, %s", p.ID, p.Source, week)
		}
	}
	if len(paid) != 4 {
		t.Fatalf("the marketplace paid %d payouts after two runs, want 4: %+v", len(paid), paid)
	}
	for _, w := range []struct {
		key, status, currency string
		minor                 int64
		postings              string
	}{
		{seller + " sale", economy.PaymentCompleted, "USD", 3000,
			"earnings_payout_hold company -3000; earnings_payout_hold hold 3000; earnings_payout hold -3000; earnings_payout partner 3000"},
		{seller + " royalty", economy.PaymentCompleted, "USD", 1000,
			"earnings_payout_hold company -1000; earnings_payout_hold hold 1000; earnings_payout hold -1000; earnings_payout partner 1000"},
		{creator + " royalty", economy.PaymentCompleted, "GBP", 1805, // £18.054, rounded where it is paid
			"earnings_payout_hold company -1805; earnings_payout_hold hold 1805; earnings_payout hold -1805; earnings_payout partner 1805"},
		{pending + " sale", economy.PayoutHeld, "USD", 4000, "earnings_payout_hold company -4000; earnings_payout_hold hold 4000"},
	} {
		p, ok := got[w.key]
		if !ok || p.Status != w.status || p.Currency != w.currency || p.AmountMinor != w.minor {
			t.Errorf("%s = %+v, want %s %d %s", w.key, p, w.status, w.minor, w.currency)
			continue
		}
		if pp := b4611Postings(t, pool, p.ID); pp != w.postings {
			t.Errorf("%s posted %q, want %q", w.key, pp, w.postings)
		}
	}
	if p := got[seller+" sale"]; p.Partner != money.SandboxName || p.PartnerRef == "" {
		t.Errorf("the seller's sale was paid by %q (%q), want the sandbox", p.Partner, p.PartnerRef)
	}
	for currency, want := range map[string]int64{"USD": 10_000 - 3000 - 1000 - 4000, "GBP": 10_000 - 1805} {
		if b, err := bank.Balance(ctx, marketplace, accounts[currency]); err != nil || b.TestMinor != want {
			t.Errorf("the marketplace's %s account holds %+v (%v), want %d: each payout taken once", currency, b, err, want)
		}
	}

	// Each seller has one money payout this week, taken from their available balance on the journal.
	for ws, want := range map[string]int64{seller: 40_000_000, creator: 26_550_000, pending: 40_000_000} {
		var n int
		var gross, cleared int64
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(gross_usd_micros), 0)::bigint,
			COALESCE((SELECT -sum(p.amount_usd_micros) FROM market_journal_postings p JOIN market_journal_entries e ON e.id = p.entry_id
				WHERE e.kind = 'payout' AND e.ref IN (SELECT id FROM market_payouts WHERE workspace_id = $1) AND p.account = 'money:clearing'), 0)::bigint
			FROM market_payouts WHERE workspace_id = $1 AND method = 'money' AND paid_at IS NOT NULL`, ws).Scan(&n, &gross, &cleared); err != nil {
			t.Fatal(err)
		}
		if n != 1 || gross != want || cleared != want {
			t.Errorf("%s has %d paid money payouts of %d µUSD, %d to money:clearing; want one of %d", ws, n, gross, cleared, want)
		}
	}
	if pg, err := store.SellerPayouts(ctx, nil, seller, now); err != nil || pg.AvailableUSDMicros != 0 || !pg.PaidThisWeek ||
		len(pg.Payouts) != 1 || pg.Payouts[0].Method != market.PayoutMoney || pg.Payouts[0].Asset != "USD" {
		t.Errorf("the seller's payouts page = %+v, %v; want nothing left, paid this week in USD on the money layer", pg, err)
	}
}
