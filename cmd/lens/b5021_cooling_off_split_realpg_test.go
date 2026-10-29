package main

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B50.21 — a new payee's cooling-off counts what was already paid to it: after £1,000 to a payee saved an hour ago, a
// second £1,000 is held with nothing on the ledger, and is paid once the payee's 24 hours have passed.
func TestNewPayeeCoolingOff_CountsEarlierPaymentsToThePayee(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5021-cooling-off-split"
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
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -1000000}, {AccountID: company.ID, AmountMinor: 1000000}}}); err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "New Supplier", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '1 hour' WHERE id = $1`, payee.ID); err != nil {
		t.Fatal(err)
	}
	paymentsOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}
	pay := func(key string) error {
		_, err := store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, AmountMinor: 100000, Currency: "GBP",
			Funding: economy.FundingTest, Reference: "INV-" + key, IdempotencyKey: key})
		return err
	}

	if err := pay("first"); err != nil {
		t.Fatalf("the first £1,000 to the new payee = %v, want paid", err)
	}
	if err := pay("second"); !errors.Is(err, economy.ErrPayeeCoolingOff) {
		t.Fatalf("a second £1,000 to the new payee = %v, want ErrPayeeCoolingOff", err)
	}
	if n, minor := paymentsOut(); n != 1 || minor != 100000 {
		t.Fatalf("after splitting £2,000 the ledger has %d payments out of %d, want 1 of 100000", n, minor)
	}

	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '24 hours 1 minute' WHERE id = $1`, payee.ID); err != nil {
		t.Fatal(err)
	}
	if err := pay("second"); err != nil {
		t.Fatalf("the second £1,000 after 24 hours = %v, want paid", err)
	}
	if n, minor := paymentsOut(); n != 2 || minor != 200000 {
		t.Fatalf("after 24 hours the ledger has %d payments out of %d, want 2 of 200000", n, minor)
	}
}
