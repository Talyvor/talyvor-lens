package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	capabilityterms "github.com/talyvor/lens/docs/terms"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B17.354 — with the capabilities' terms published, as Lens publishes them when it starts, the payout run still pays
// sellers from Talyvor's own workspace: the terms are what a customer accepts, and Talyvor accepts none of its own. A
// seller with verified details is paid its sale under market:<week>; one whose details are pending is held, never sent.
func TestB17354_TalyvorPaysSellersOnceTheTermsArePublished(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, pending = "ws-b17354-seller", "ws-b17354-pending"
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, reg)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetAccountPartners(reg)
	bank.SetPayeeScreener(screener)
	bank.SetScreener(screener)
	bank.SetMoneyLayer(sandboxMoneyLayer(t, pool, bank, reg))
	if _, err := bank.PublishFirstTerms(ctx, economy.CapabilityTermsTexts(capabilityterms.FS)); err != nil {
		t.Fatal(err)
	}

	payable := time.Now().Add(-15 * 24 * time.Hour)
	for i, s := range []struct{ ws, holder string }{{seller, "Sam Seller"}, {pending, "TESTPENDING Pat Seller"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, s.ws); err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{economy.CapabilityCurrencyAccounts, economy.CapabilityPaymentsIn, economy.CapabilityPaymentsOut} {
			if _, err := bank.AcceptTerms(ctx, s.ws, c, 1, "owner", "127.0.0.1"); err != nil {
				t.Fatalf("%s accepting %s's terms: %v", s.ws, c, err)
			}
		}
		if _, err := bank.SavePayoutDetails(ctx, s.ws, economy.PayoutDetails{Asset: "USD", Holder: s.holder, IBAN: "GB82WEST12345698765432"}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, invoice_id,
			cleared_at, payable_at, kind) VALUES ($1, $2, 30000000, 30000000, 'in_b17354', $3, $3, $4)`,
			fmt.Sprintf("use_b17354_%d", i), s.ws, payable, market.EarningSale); err != nil {
			t.Fatal(err)
		}
	}

	store := market.NewStore(pool)
	store.SetMoneyPayouts(bank, economy.TalyvorWorkspace)
	now := time.Now()
	y, w := now.UTC().ISOWeek()
	week := fmt.Sprintf("market:%04d-W%02d", y, w)
	for _, ws := range []string{seller, pending} {
		if held, p, err := store.PayOutTestSeller(ctx, ws, now); err != nil || held || p == nil || p.Method != market.PayoutMoney {
			t.Fatalf("the run for %s made %+v (held %v): %v; want a money payout", ws, p, held, err)
		}
	}

	paid, err := bank.EarningsPayouts(ctx, economy.TalyvorWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]economy.EarningsPayout{}
	for _, e := range paid {
		got[e.RecipientWorkspaceID] = e
	}
	if s, h := got[seller], got[pending]; len(paid) != 2 || s.Source != week || s.Kind != economy.EarningsSale || s.AmountMinor != 3000 ||
		s.Status != economy.PaymentCompleted || s.PartnerRef == "" || h.Source != week || h.AmountMinor != 3000 || h.Status != economy.PayoutHeld || h.PartnerRef != "" {
		t.Fatalf("Talyvor's earnings payouts are %+v; want the seller's $30.00 sale completed and the pending seller's held, unsent, under %s", paid, week)
	}
}
