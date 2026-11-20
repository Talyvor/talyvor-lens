package main

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B50.48 — a payment out never takes an idempotency key Lens pays its own payments under, on the migrated schema.

// Someone who may pay out pays payee B £1.00 under the key a waiting request's item is paid under; the owner then
// approves the request. The £1.00 is refused, and the approved £50.00 is paid to payee A.
func TestB5048_ACallerCannotTakeAnApprovedRequestsKey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	b := b501Payer(t, pool, "ws-b5048-preq")
	ws := b.acct.WorkspaceID
	b.store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_device_keys WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	sign := b5018Passkey(t, b.store, ws, rpID, origin)
	other, err := b.store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Bob Other", Country: "GB", SortCode: "200000", AccountNumber: "11223344"})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := b.store.PreparePaymentRequest(ctx, ws, "ada", "b5048-request", economy.PaymentRequestIn{Client: "chat", AgentID: b.acct.AgentID,
		Items: []economy.PaymentRequestItemIn{{PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: 5000, Reference: "Invoice A"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "pending_approval" {
		t.Fatalf("the request is %s, want pending_approval", r.Status)
	}
	key := "preq:" + r.ID + ":1"

	if _, err := b.store.PayOutside(ctx, ws, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: other.ID, Currency: "GBP",
		AmountMinor: 100, Reference: "first", IdempotencyKey: key}); !errors.Is(err, economy.ErrOutsidePayment) {
		t.Errorf("a payment under the request's key: %v, want ErrOutsidePayment", err)
	}

	statement, challenge, _, err := b.store.PrepareRequestApproval(ctx, ws, r.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.ApprovePaymentRequest(ctx, ws, r.ID, "ada", statement, sign(challenge)); err != nil {
		t.Fatal(err)
	}
	var payee string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT payee_id, amount_minor FROM money_payments WHERE workspace_id = $1 AND idempotency_key = $2`,
		ws, key).Scan(&payee, &amount); err != nil || payee != b.payee.ID || amount != 5000 {
		t.Errorf("the payment under the request's key = %s %d (%v), want the approved 5000 to %s", payee, amount, err, b.payee.ID)
	}
	if got := b468Postings(t, pool, ws, key); got == "" {
		t.Error("no ledger row for the approved payment")
	}
	b.balance(t, 45000)
	b501ZeroSum(t, pool)
}

// A caller's key that starts like one of Lens's own — a standing order's tick, a payout batch's row, a mandate's pull,
// a request's item and the rest — is refused before anything is held, and no payment is made under it.
func TestB5048_ACallerCannotTakeLensKeys(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	b := b501Payer(t, pool, "ws-b5048-keys")
	ws := b.acct.WorkspaceID
	for _, key := range []string{"schedule:sch_1:2026-11-13T09:00:00Z", "payout:pob_1:1", "mandate_pull:mdt_1:k", "preq:preq_1:1", "payment_in:x", "fx:x",
		"card_x", "money:x", "  schedule:sch_2:2026-11-13T09:00:00Z"} {
		if _, err := b.store.PayOutside(ctx, ws, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID, Currency: "GBP",
			AmountMinor: 100, IdempotencyKey: key}); !errors.Is(err, economy.ErrOutsidePayment) {
			t.Errorf("a payment under %q: %v, want ErrOutsidePayment", key, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1`, ws).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d payments (%v), want none", n, err)
	}
	b.balance(t, 50000)
}
