package main

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B46.19 — a pending payment out passes the money layer's gate only for its own payee and paying account: the same key
// and amount to another payee, or from another account, is judged by the agent's rules and refused above its limits.
func TestB4619_AHeldPaymentPassesTheGateOnlyForItsOwnPayeeAndAccount(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4619"
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
	p := b.pay(t, "held", 2000)
	// The partner has not answered yet: the payment is pending and its money held.
	if _, err := pool.Exec(ctx, `UPDATE money_payments SET status = 'pending' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	var from string
	if err := pool.QueryRow(ctx, `SELECT c.partner_account_ref FROM money_accounts a
		JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id) WHERE a.id = $1`, b.acct.ID).Scan(&from); err != nil || from == "" {
		t.Fatalf("the partner reference the agent pays from = %q, %v", from, err)
	}
	// From now on the agent may move at most £1.00 at a time.
	if _, err := b.store.SetAgentRules(ctx, ws, p.AgentID, economy.AgentRules{MaxPerRequestULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	own := money.Payee{Name: b.payee.Name, SortCode: b.payee.SortCode, AccountNumber: b.payee.AccountNumber} // as PayOutside sends it
	op := func(ref string, to money.Payee) money.Op {
		return money.Op{Route: money.Route{Workspace: ws, Agent: p.AgentID, Country: "GB", Capability: economy.CapabilityPaymentsOut},
			Partner: money.SandboxName, Capability: money.CapTransfers, Method: "outbound", Key: p.ID, Ref: ref,
			Amount: money.Amount{Minor: 2000, Asset: "GBP"}, Payee: to.Name, To: to}
	}
	limits := b.store.MoneyLimits()
	if err := limits.Allow(ctx, op(from, own)); err != nil {
		t.Fatalf("the held payment itself: %v, want it through the gate", err)
	}
	for name, o := range map[string]money.Op{
		"another payee's account": op(from, money.Payee{Name: own.Name, SortCode: own.SortCode, AccountNumber: "11223344"}),
		"another payee's name":    op(from, money.Payee{Name: "Mallory", SortCode: own.SortCode, AccountNumber: own.AccountNumber}),
		"another paying account":  op(from+"-other", own),
	} {
		if err := limits.Allow(ctx, o); !errors.Is(err, money.ErrLimit) {
			t.Errorf("%s under the held payment's key: %v, want refused by the agent's £1.00 limit", name, err)
		}
	}
}
