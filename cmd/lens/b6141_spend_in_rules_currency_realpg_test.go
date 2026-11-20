package main

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B61.41 — what an agent has spent is counted in the rules' own currency: a euro agent with a €1,000.00 daily limit
// pays €600.00, the euro moves by 5%, and €400.00 more is let through while €400.01 is refused. Each payment the rules
// let through records what it was worth in euros then, so the morning's €600.00 is still €600.00 after the rate moves.
func TestB6141_SpendSoFarIsCountedInTheRulesCurrency(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6141"
	var reg *partners.Registry
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
			reg = r
			return sandboxMoneyLayer(t, pool, store, r)
		})
	b6112TopUp(t, pool, b, 400_000)
	// A payee saved a day ago: the new-payee wait (B50.21) is not what this judges.
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '2 days' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.SetAgentRules(ctx, ws, b.acct.AgentID, economy.AgentRules{Currency: "EUR", DailyLimitMinor: 100_000}); err != nil {
		t.Fatal(err)
	}
	reg.UseFXRates(b6112Rates{big.NewRat(11, 10)})
	b.pay(t, "morning", 60_000)
	reg.UseFXRates(b6112Rates{big.NewRat(1155, 1000)}) // the euro up 5%
	_, err := b.store.PayOutside(ctx, ws, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID,
		Currency: "EUR", AmountMinor: 40_001, Reference: "Invoice over", IdempotencyKey: "over"})
	if !errors.Is(err, economy.ErrAgentRule) {
		t.Fatalf("€400.01 after €600.00 of a €1,000.00 day: %v, want refused", err)
	}
	b.pay(t, "rest", 40_000)
	// On the ledger: €1,000.00 out of the agent's account, the two payments the rules let through.
	var paid int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(-sum(p.amount_minor), 0)::bigint FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		WHERE p.account_id = $1 AND e.idempotency_key LIKE 'payment_out%'`, b.acct.ID).Scan(&paid); err != nil || paid != 100_000 {
		t.Fatalf("the agent's account paid out %d (%v), want 100000", paid, err)
	}
	t.Logf("€600.00, then €400.01 refused and €400.00 let through with the euro up 5%%: %d cents out", paid)
}
