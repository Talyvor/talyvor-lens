package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// B30.1 — on the migrated schema, every new money capability takes test-funded money and refuses live money
// naming its class, with no ledger row for the refusal; a clearance lets live money through only for an owner verified
// in a country it lists and only until it expires.
func TestB30Capabilities_TestMoneyOnlyUntilClearedForTheCountry(t *testing.T) {
	pool := supplyPool(t)
	gb := WithUseCountry(context.Background(), "GB")
	s := NewDualTokenStore(nil, pool, nil)
	const lxc = int64(1_000_000)
	const useType = "b30_capability_use"

	workspace := func(id, funding string) {
		t.Helper()
		if _, err := pool.Exec(gb, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, id); err != nil {
			t.Fatal(err)
		}
		sandboxConfirmed(t, pool, id) // B61.52: it moves money, so it has confirmed who it is
		if _, err := s.CreditLXC(gb, id, 100*lxc, "stripe top-up", map[string]interface{}{"funding": funding}); err != nil {
			t.Fatal(err)
		}
	}
	// use moves 10 LXC of ws's credits on the capability the way every gated path does: the capability is asked
	// first, then the ledger row is written, in one transaction.
	use := func(ctx context.Context, ws, key string) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := spendForCapability(ctx, tx, ws, key, 10*lxc); err != nil {
			return err
		}
		bal, minted, spent, err := readLXCBalance(ctx, tx, ws)
		if err != nil {
			t.Fatal(err)
		}
		if err := insertLXCLedger(ctx, tx, ws, -10*lxc, bal-10*lxc, useType, key, map[string]interface{}{"capability": key}); err != nil {
			t.Fatal(err)
		}
		if err := writeLXCBalance(ctx, tx, ws, bal-10*lxc, minted, spent); err != nil {
			t.Fatal(err)
		}
		return tx.Commit(ctx)
	}
	ledger := func(ws string) (n int, sum int64) {
		t.Helper()
		if err := pool.QueryRow(gb, `SELECT count(*), COALESCE(sum(amount), 0) FROM lxc_ledger WHERE workspace_id = $1 AND type = $2`,
			ws, useType).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	refused := func(err error, c Capability) bool {
		return errors.Is(err, ErrCapabilityNotCleared) && strings.Contains(err.Error(), "class "+string(c.Class))
	}

	keys := []string{CapabilityCurrencyAccounts, CapabilityAccountDetails, CapabilityPaymentsIn, CapabilityPaymentsOut,
		CapabilityPayByBank, CapabilityFX, CapabilityStablecoins, CapabilityX402, CapabilityMerchantAcceptance,
		CapabilityB2BCredit, CapabilitySellerAdvances, CapabilityLendingMarketplace, CapabilityTradeEquities,
		CapabilityTradeCrypto, CapabilityTradePrediction, CapabilityTreasurySweep, CapabilityPriceLock, CapabilityCover,
		CapabilityPayoutsToPeople}
	amber := map[string]bool{CapabilityPayByBank: true, CapabilityB2BCredit: true, CapabilitySellerAdvances: true,
		CapabilityLendingMarketplace: true, CapabilityPriceLock: true}
	for _, key := range keys {
		c, ok := CapabilityByKey(key)
		if want := map[bool]CapabilityClass{true: ClassAmber, false: ClassRed}[amber[key]]; !ok || c.Class != want {
			t.Fatalf("%s is registered %v as %+v, want class %s", key, ok, c, want)
		}
		testWS, liveWS := "ws-b301-test-"+key, "ws-b301-live-"+key
		workspace(testWS, FundingTest)
		workspace(liveWS, FundingLive)
		if err := use(gb, testWS, key); err != nil {
			t.Fatalf("%s on test-funded credits = %v, want accepted", key, err)
		}
		if n, sum := ledger(testWS); n != 1 || sum != -10*lxc {
			t.Fatalf("%s on test-funded credits wrote %d rows summing %d, want one of −10 LXC", key, n, sum)
		}
		if err := use(gb, liveWS, key); !refused(err, c) {
			t.Fatalf("%s on live credits = %v, want refused naming class %s", key, err, c.Class)
		}
		if n, _ := ledger(liveWS); n != 0 {
			t.Fatalf("%s refused live money but wrote %d ledger rows", key, n)
		}
	}

	// A clearance for Ireland: live money by an owner verified in Ireland goes through; by one verified in Great
	// Britain it is refused naming class RED and the country, as though there were no clearance, and so it is by one
	// verified in no country (B30.10: the owner's country is the one verification confirmed, whatever the caller says).
	payments, _ := CapabilityByKey(CapabilityPaymentsOut)
	const liveWS, gbWS, nowhereWS = "ws-b301-live-" + CapabilityPaymentsOut, "ws-b3010-gb", "ws-b3010-nowhere"
	workspace(gbWS, FundingLive)
	workspace(nowhereWS, FundingLive)
	for ws, country := range map[string]string{liveWS: "IE", gbWS: "GB", nowhereWS: ""} {
		planGatesOnPlan(t, pool, ws, "team", false) // B32.12: a plan with live money, which a cleared capability needs
		liveVerifiedIn(t, pool, ws, country)        // B30.4: and a verification level
	}
	if _, err := s.ClearCapability(gb, CapabilityPaymentsOut, "nicolai", ClearanceTerms{Reference: "partner agreement PA-7",
		Licence: "EMI-900123", Partner: "Test Payments Ltd", Countries: []string{"ie"}, ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := use(gb, liveWS, CapabilityPaymentsOut); err != nil {
		t.Fatalf("live money by an owner verified in a country the clearance lists = %v, want accepted", err)
	}
	if err := use(WithUseCountry(context.Background(), "IE"), gbWS, CapabilityPaymentsOut); !refused(err, payments) ||
		!strings.Contains(err.Error(), "only for owners verified in IE; this workspace is verified in GB") {
		t.Fatalf("live money by an owner verified in a country the clearance does not list = %v, want refused naming class RED and GB", err)
	}
	if err := use(gb, nowhereWS, CapabilityPaymentsOut); !refused(err, payments) || !strings.Contains(err.Error(), "no verified country") {
		t.Fatalf("live money by an owner verified in no country = %v, want refused naming class RED", err)
	}
	if n, sum := ledger(liveWS); n != 1 || sum != -10*lxc {
		t.Fatalf("after the cleared use: %d rows summing %d, want one of −10 LXC", n, sum)
	}
	if n, _ := ledger(gbWS); n != 0 {
		t.Fatalf("the refusal for Great Britain wrote %d ledger rows", n)
	}

	// A clearance for Great Britain that passed its expiry an hour ago refuses live money by an owner verified there.
	if _, err := pool.Exec(gb, `INSERT INTO wallet_clearances (capability, action, operator, reference, countries, partner,
		licence_reference, expires_at) VALUES ($1, 'clear', 'nicolai', 'partner agreement PA-8', '{GB}', 'Test Payments Ltd',
		'EMI-900123', now() - interval '1 hour')`, CapabilityPaymentsOut); err != nil {
		t.Fatal(err)
	}
	if err := use(gb, gbWS, CapabilityPaymentsOut); !refused(err, payments) {
		t.Fatalf("live money under an expired clearance = %v, want refused naming class RED", err)
	}
	if n, _ := ledger(gbWS); n != 0 {
		t.Fatalf("the refusal under an expired clearance wrote a ledger row: %d rows", n)
	}
	caps, err := s.WalletCapabilities(gb)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.Key == CapabilityPaymentsOut && (c.RealMoney || c.Clearance != nil) {
			t.Fatalf("an expired clearance shows as %+v, want test money only", c)
		}
	}

	// A clear that does not name a licence, a partner, real countries and a future expiry is not recorded.
	for name, terms := range map[string]ClearanceTerms{
		"no licence":      {Reference: "r", Partner: "P", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)},
		"no partner":      {Reference: "r", Licence: "L", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)},
		"no countries":    {Reference: "r", Licence: "L", Partner: "P", ExpiresAt: time.Now().Add(time.Hour)},
		"not a country":   {Reference: "r", Licence: "L", Partner: "P", Countries: []string{"XX"}, ExpiresAt: time.Now().Add(time.Hour)},
		"already expired": {Reference: "r", Licence: "L", Partner: "P", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(-time.Hour)},
	} {
		if _, err := s.ClearCapability(gb, CapabilityFX, "nicolai", terms); err == nil {
			t.Errorf("a clearance with %s was recorded", name)
		}
	}
}
