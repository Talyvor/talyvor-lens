package economy

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/partners"
)

// B30.11 — a clean day reports zero breaks in every currency, and a ledger payment the partner's statement does not
// have shows as a missing break with its amount, in the run and in the safeguarding view.
func TestReconciliation_ACleanDayHasNoBreaksAndAMissingStatementLineIsOne(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	p := &partners.TestAccountPartner{}
	const ws = "ws-b3011-reconcile"
	sandboxConfirmed(t, pool, ws) // B61.52: they move money, so they have confirmed who they are

	at, err := p.OpenAccount(ctx, partners.AccountRequest{ID: "acct-gbp", Holder: "Acme Ltd", Currency: CurrencyGBP})
	if err != nil {
		t.Fatal(err)
	}
	company := openMoney(t, s, ws, CurrencyGBP, MoneyCompany)
	partner, err := s.OpenMoneyAccount(ctx, MoneyAccount{WorkspaceID: ws, Currency: CurrencyGBP, Purpose: MoneyPartner, PartnerAccountRef: at.Ref})
	if err != nil {
		t.Fatal(err)
	}
	post := func(key, ref string, in int64) {
		t.Helper()
		if _, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd", Kind: "payment",
			IdempotencyKey: key, PartnerRef: ref, Funding: FundingTest, Postings: []MoneyPosting{
				{AccountID: company.ID, AmountMinor: in}, {AccountID: partner.ID, AmountMinor: -in}}}); err != nil {
			t.Fatal(err)
		}
	}
	in, err := p.PayByBank(ctx, partners.PayByBankRequest{ID: "in-1", AccountRef: at.Ref, Amount: partners.Money{Minor: 25_00, Currency: CurrencyGBP}, Payer: "Acme Ltd"})
	if err != nil {
		t.Fatal(err)
	}
	post("in-1", in.Ref, 25_00)
	out, err := p.SendPayment(ctx, partners.PaymentRequest{ID: "out-1", AccountRef: at.Ref, Amount: partners.Money{Minor: 10_00, Currency: CurrencyGBP},
		Payee: partners.Payee{Name: "Acme Ltd"}})
	if err != nil {
		t.Fatal(err)
	}
	post("out-1", out.Ref, -10_00)

	runs, err := s.ReconcileDay(ctx, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != len(MoneyCurrencies) {
		t.Fatalf("%d runs; want one per currency, %d", len(runs), len(MoneyCurrencies))
	}
	for _, r := range runs {
		if r.BreakCount != 0 || len(r.Breaks) != 0 || r.Funding != FundingTest {
			t.Fatalf("a clean day's %s run is %+v; want zero breaks, test money", r.Currency, r)
		}
	}
	gbp := runFor(t, runs, CurrencyGBP)
	if gbp.CustomersHoldMinor != 15_00 || gbp.PartnerHoldsMinor != 15_00 || gbp.ShortfallMinor != 0 {
		t.Fatalf("the GBP run is %+v; want £15.00 held by customers and by the partner", gbp)
	}

	// The ledger moves £7.00 in that the partner's statement never shows.
	post("in-2", "test_pbb_never_arrived", 7_00)
	runs, err = s.ReconcileDay(ctx, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	gbp = runFor(t, runs, CurrencyGBP)
	want := ReconciliationBreak{Kind: BreakMissing, WorkspaceID: ws, AccountID: partner.ID, PaymentRef: "test_pbb_never_arrived",
		LedgerMinor: 7_00, AmountMinor: -7_00}
	if gbp.BreakCount != 1 || len(gbp.Breaks) != 1 || gbp.Breaks[0] != want {
		t.Fatalf("the GBP run's breaks are %d %+v; want only %+v", gbp.BreakCount, gbp.Breaks, want)
	}
	if gbp.CustomersHoldMinor != 22_00 || gbp.PartnerHoldsMinor != 15_00 || gbp.ShortfallMinor != 7_00 {
		t.Fatalf("the GBP run is %+v; want £22.00 held by customers against the partner's £15.00, £7.00 short", gbp)
	}
	view, err := s.Safeguarding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v := runFor(t, view, CurrencyGBP); v.ID != gbp.ID || v.ShortfallMinor != 7_00 || len(v.Breaks) != 1 || v.Breaks[0] != want {
		t.Fatalf("the safeguarding view shows %+v for GBP; want the latest run %s with its £7.00 break", v, gbp.ID)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM reconciliation_runs WHERE break_count > 0`); n != 1 {
		t.Fatalf("%d runs recorded breaks; want the one GBP run", n)
	}
}

func runFor(t *testing.T, runs []ReconciliationRun, currency string) ReconciliationRun {
	t.Helper()
	for _, r := range runs {
		if r.Currency == currency {
			return r
		}
	}
	t.Fatalf("no %s run in %+v", currency, runs)
	return ReconciliationRun{}
}
