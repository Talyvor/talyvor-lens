package economy

import (
	"context"
	"testing"
	"time"
)

// B30.23 — a currency account's statement says where each line's money stands: a payment out's hold reads the
// payment's status, and money in, booked, reads completed.
func TestAccountStatement_EachLineSaysWhereItsMoneyStands(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b3023-state"
	sandboxConfirmed(t, pool, ws)
	company, partner, hold := openMoney(t, s, ws, CurrencyGBP, MoneyCompany), openMoney(t, s, ws, CurrencyGBP, MoneyPartner),
		openMoney(t, s, ws, CurrencyGBP, MoneyHold)
	post := func(kind, key, who, from, to string, amount int64) MoneyEntry {
		t.Helper()
		e, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Kind: kind, IdempotencyKey: key, Counterparty: who,
			Funding: FundingTest, Postings: []MoneyPosting{{AccountID: from, AmountMinor: -amount}, {AccountID: to, AmountMinor: amount}}})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		return e
	}
	first := post("payment_in", "b3023-in", "Acme Ltd", partner.ID, company.ID, 80_00)
	last := post("payment_out_hold", "payment_out_hold:b3023-pay", "", company.ID, hold.ID, 12_50)
	if _, err := pool.Exec(ctx, `INSERT INTO money_payments (id, workspace_id, idempotency_key, account_id, hold_account_id, payee_id,
		payee_name, amount_minor, currency, funding, status) VALUES ('mpay_b3023', $1, 'b3023-pay', $2, $3, 'pye_b3023', 'Supplier Ltd',
		1250, 'GBP', 'test', 'sent')`, ws, company.ID, hold.ID); err != nil {
		t.Fatal(err)
	}

	st, err := s.AccountStatement(ctx, ws, "", company.ID, first.CreatedAt, last.CreatedAt.Add(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Lines) != 2 || st.Lines[0].State != "completed" || st.Lines[1].State != "sent" {
		t.Fatalf("lines %+v; want the payment in completed and the payment out's hold sent", st.Lines)
	}
}
