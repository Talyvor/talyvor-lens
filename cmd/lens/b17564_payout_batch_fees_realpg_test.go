package main

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B17.564 — a payout batch prices each row's fee when it is uploaded, as the payment will be charged: free while the
// plan's payments this month last (10 on Studio), then 0.20 each. The funds check counts the fee, so a row the
// account cannot pay with its fee is invalid, and an approval never moves more than it approved: a row priced free
// whose free payment another payment took before the approval is not paid, and the account falls by exactly what
// the paid rows were approved at.
func TestPayoutBatch_PricesEachRowsFeeAndNeverPaysMoreThanApproved(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b17564-payout-fees"
	confirmedAccounts(t, pool, ws)
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	// £1,000.00 of test money in to the company's account.
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -100000}, {AccountID: company.ID, AmountMinor: 100000}}}); err != nil {
		t.Fatal(err)
	}
	rows := func(n int, minor int64) []economy.PayoutRow {
		out := make([]economy.PayoutRow, n)
		for i := range out {
			out[i] = economy.PayoutRow{PayeeID: payee.ID, AmountMinor: minor, Currency: "GBP", Reference: "Pay " + jsonOf(i+1)}
		}
		return out
	}
	balance := func() int64 {
		t.Helper()
		b, err := store.Balance(ctx, ws, company.ID)
		if err != nil {
			t.Fatal(err)
		}
		return b.TestMinor
	}

	// 11 rows of £90.90 are £999.90, which the account holds; the 11th row's 0.20 fee makes it £1,000.10, which it does not.
	tight, err := store.CreatePayoutBatch(ctx, ws, economy.PayoutBatch{IdempotencyKey: "tight", Rows: rows(11, 9090)})
	if err != nil {
		t.Fatal(err)
	}
	if last := tight.Rows[10]; tight.RowsValid != 10 || last.Status != economy.PayoutRowInvalid || !strings.Contains(last.Detail, "with its fee") ||
		tight.TotalsMinor["GBP"] != 10*9090 || tight.FeesMinor["GBP"] != 0 {
		t.Errorf("the tight batch has %d valid rows, totals %v, fees %v and row 11 %+v, want 10 valid, 909.00, no fees and row 11 invalid for its fee",
			tight.RowsValid, tight.TotalsMinor, tight.FeesMinor, last)
	}

	// Ten rows of £1.00, all within the month's 10 free payments, so approved at no fee. One payment out then takes a
	// free payment before the approval: the tenth row would now cost 0.20 that nobody approved.
	free, err := store.CreatePayoutBatch(ctx, ws, economy.PayoutBatch{IdempotencyKey: "free", Rows: rows(10, 100)})
	if err != nil {
		t.Fatal(err)
	}
	if free.RowsValid != 10 || free.FeesMinor["GBP"] != 0 {
		t.Fatalf("the free batch has %d valid rows and fees %v, want 10 valid at no fee", free.RowsValid, free.FeesMinor)
	}
	if _, err := store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, AmountMinor: 100, Currency: "GBP", Funding: economy.FundingTest,
		Reference: "Before the batch", IdempotencyKey: "one-before"}); err != nil {
		t.Fatal(err)
	}
	if got := balance(); got != 100000-100 {
		t.Fatalf("after one free payment the account holds %d, want %d", got, 100000-100)
	}
	sent, err := store.ApprovePayoutBatch(ctx, ws, free.ID, "owner", nil)
	if err != nil {
		t.Fatal(err)
	}
	paid := 0
	for _, r := range sent.Rows {
		if r.PaymentID != "" {
			paid++
		}
	}
	if last := sent.Rows[9]; paid != 9 || last.PaymentID != "" || last.Status != economy.PaymentFailed || !strings.Contains(last.Detail, "approved with a fee of") {
		t.Errorf("approval paid %d rows and row 10 is %+v, want 9 paid and row 10 not paid, for a fee it was not approved with", paid, last)
	}
	// The ledger: the account falls by the one payment and the 9 rows at no fee, and not a penny more.
	var n int
	var out int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		WHERE e.workspace_id = $1 AND e.kind LIKE 'payment_out%' AND p.account_id = $2`, ws, company.ID).Scan(&n, &out); err != nil {
		t.Fatal(err)
	}
	if n != 10 || out != -1000 {
		t.Errorf("the company's account shows %d payment postings of %d pence, want 10 of -1000", n, out)
	}
	if got := balance(); got != 100000-1000 {
		t.Errorf("the account holds %d after the approval, want %d", got, 100000-1000)
	}
}
