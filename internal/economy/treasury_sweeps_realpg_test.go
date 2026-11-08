package economy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/partners"
)

// B30.59 — a company account's idle money above its target, swept each day into a money-market fund.

// A company's USD account holds $10,000.00 with a target of $2,500.00: the day's sweep moves the $7,500.00 above it
// into the fund — one entry for treasury_sweep, the account -750000 and the fund +750000 — and running again that day
// moves nothing. A withdrawal of $1,000.00 comes back at once, the fund -100000 and the account +100000; more than the
// fund holds moves nothing. The sweep shows the fund's balance and a return labelled simulated; a live sweep is
// refused while treasury_sweep is uncleared, and another workspace cannot set one on the account.
func TestTreasurySweep_SweepsTheExcessAndComesBackOnWithdrawal(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	reg := partners.NewRegistry(nil)
	s.SetAccountPartners(reg)
	s.SetTreasuryPartners(reg)
	defer func(bps int64) { SimTreasuryRateBPS = bps }(SimTreasuryRateBPS)
	SimTreasuryRateBPS = 365 // 3.65% a year: a cent a day on each $100.00
	const ws = "ws-b3059-treasury"
	acct, err := s.OpenCurrencyAccount(ctx, ws, "", CurrencyUSD, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTestMoney(ctx, ws, acct.ID, 10_000_00, "b3059-in"); err != nil {
		t.Fatal(err)
	}

	sw, err := s.SetTreasurySweep(ctx, ws, acct.ID, 2_500_00, FundingTest)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Partner != "test" || sw.PartnerAccountRef == "" || sw.TargetMinor != 2_500_00 || sw.FundMinor != 0 {
		t.Fatalf("set %+v; want an empty fund at the test broker with a $2,500.00 target", sw)
	}
	if _, err := s.SetTreasurySweep(ctx, ws, acct.ID, 1_00, FundingLive); !errors.Is(err, ErrCapabilityNotCleared) {
		t.Fatalf("a live sweep while treasury_sweep is uncleared: %v; want ErrCapabilityNotCleared", err)
	}
	if _, err := s.SetTreasurySweep(ctx, "ws-b3059-other", acct.ID, 1_00, FundingTest); !errors.Is(err, ErrMoneyAccountNotFound) {
		t.Fatalf("another workspace sets a sweep on the account: %v; want ErrMoneyAccountNotFound", err)
	}

	day := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	if n, err := s.SweepTreasuries(ctx, day); err != nil || n != 1 {
		t.Fatalf("the day's sweep moved %d (%v); want 1", n, err)
	}
	type row struct {
		capability, kind, account string
		amount                    int64
	}
	postings := func(kind string) []row {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT e.capability, e.kind, p.account_id, p.amount_minor FROM money_entries e
			JOIN money_postings p ON p.entry_id = e.id WHERE e.workspace_id = $1 AND e.kind = $2 ORDER BY e.created_at, p.line`, ws, kind)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.capability, &r.kind, &r.account, &r.amount); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		return got
	}
	want := []row{
		{CapabilityTreasurySweep, "treasury_sweep", acct.ID, -7_500_00},
		{CapabilityTreasurySweep, "treasury_sweep", sw.FundAccountID, 7_500_00},
	}
	if got := postings("treasury_sweep"); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the sweep posted %+v; want %+v", got, want)
	}
	if n, err := s.SweepTreasuries(ctx, day.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("a second sweep that day moved %d (%v); want 0", n, err)
	}

	if _, err := s.WithdrawTreasury(ctx, ws, acct.ID, 1_000_00, "b3059-out"); err != nil {
		t.Fatal(err)
	}
	if replay, err := s.WithdrawTreasury(ctx, ws, acct.ID, 1_000_00, "b3059-out"); err != nil || replay.Kind != "treasury_withdrawal" {
		t.Fatalf("the same withdrawal again: %+v (%v); want the first entry back", replay, err)
	}
	want = []row{
		{CapabilityTreasurySweep, "treasury_withdrawal", sw.FundAccountID, -1_000_00},
		{CapabilityTreasurySweep, "treasury_withdrawal", acct.ID, 1_000_00},
	}
	if got := postings("treasury_withdrawal"); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the withdrawal posted %+v; want %+v", got, want)
	}
	if _, err := s.WithdrawTreasury(ctx, ws, acct.ID, 7_000_00, "b3059-over"); !errors.Is(err, ErrTreasuryFunds) {
		t.Fatalf("withdrawing $7,000.00 of $6,500.00: %v; want ErrTreasuryFunds", err)
	}

	seen, err := s.TreasurySweep(ctx, ws, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seen.FundMinor != 6_500_00 || seen.RateBPS != 365 || seen.SimulatedDailyReturnMinor != 65 || seen.Return != "simulated" {
		t.Fatalf("the sweep shows %+v; want $6,500.00 in the fund and a simulated return of 65 cents a day", seen)
	}
	if b := moneyBalance(t, s, ws, acct.ID); b.AmountMinor != 3_500_00 {
		t.Fatalf("the account holds %d; want 350000", b.AmountMinor)
	}
}
