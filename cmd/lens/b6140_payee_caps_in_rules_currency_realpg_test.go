package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// b6140Payer is a euro account's agent paying an outside payee saved two days ago (the new-payee wait, B50.21, is not
// what these judge), with €4,500.00 in its account and the euro worth $1.155 — the test rate moved by 5%.
func b6140Payer(t *testing.T, ws string) (b468Payer, *pgxpool.Pool) {
	t.Helper()
	pool := agentRoutesDB(t)
	var reg *partners.Registry
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
			reg = r
			return sandboxMoneyLayer(t, pool, store, r)
		})
	b6112TopUp(t, pool, b, 400_000)
	if _, err := pool.Exec(context.Background(), `UPDATE money_payees SET created_at = now() - interval '2 days' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	reg.UseFXRates(b6112Rates{big.NewRat(1155, 1000)})
	return b, pool
}

// b6140Field is one field of v as a client reads it, in JSON.
func b6140Field(t *testing.T, v any, path ...string) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range path {
		m, _ := out.(map[string]any)
		out = m[k]
	}
	return out
}

// b6140Rules saves the agent's rules as a client sends them, in JSON.
func b6140Rules(t *testing.T, b b468Payer, body string) economy.AgentRules {
	t.Helper()
	var r economy.AgentRules
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	saved, err := b.store.SetAgentRules(context.Background(), b.acct.WorkspaceID, b.acct.AgentID, r)
	if err != nil {
		t.Fatalf("save %s: %v", body, err)
	}
	return saved
}

func (b b468Payer) try(key string, minor int64) error {
	_, err := b.store.PayOutside(context.Background(), b.acct.WorkspaceID, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID,
		Currency: "EUR", AmountMinor: minor, Reference: "Invoice " + key, IdempotencyKey: key})
	return err
}

// b6140PaidOut is what the agent's account has paid out, off the ledger.
func b6140PaidOut(t *testing.T, pool *pgxpool.Pool, b b468Payer) int64 {
	t.Helper()
	var paid int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(-sum(p.amount_minor), 0)::bigint FROM money_postings p
		JOIN money_entries e ON e.id = p.entry_id WHERE p.account_id = $1 AND e.idempotency_key LIKE 'payment_out%'`, b.acct.ID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	return paid
}

// B61.40 — a euro agent's "€50.00 a day to Grace Hopper" holds in euros with the euro moved by 5%: €50.01 is refused,
// €50.00 is paid, and then not a cent more that day. On main the cap is µLXC and the euro amount is judged at the rate.
func TestB6140_APayeeCapHoldsInTheRulesCurrency(t *testing.T) {
	b, pool := b6140Payer(t, "ws-b6140-cap")
	rules := b6140Rules(t, b, `{"currency":"EUR","payee_daily_limits_minor":{"`+b.payee.ID+`":5000}}`)
	if got := b6140Field(t, rules, "payee_daily_limits_minor", b.payee.ID); got != 5000.0 {
		t.Fatalf("the cap reads back as %v, want €50.00", got)
	}
	for key, minor := range map[string]int64{"over": 5001, "then-a-cent": 1} {
		if key == "then-a-cent" {
			b.pay(t, "at", 5000)
		}
		err := b.try(key, minor)
		if !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "€50.00") {
			t.Fatalf("pay %d cents (%s): %v, want refused by the €50.00 daily limit for Grace Hopper", minor, key, err)
		}
	}
	if paid := b6140PaidOut(t, pool, b); paid != 5000 {
		t.Fatalf("the agent's account paid out %d, want 5000", paid)
	}
}

// B61.40 — "always allow Grace Hopper up to €20.00 a day", saved by the always-allow route in euros, lets €20.00
// through without asking above €10.00, with the euro moved by 5%, and asks at €20.01.
func TestB6140_AlwaysAllowHoldsInTheRulesCurrency(t *testing.T) {
	b, pool := b6140Payer(t, "ws-b6140-allow")
	ctx := context.Background()
	b6140Rules(t, b, `{"currency":"EUR","approval_above_minor":1000}`)
	var need *economy.ApprovalNeededError
	if err := b.try("asks", 2001); !errors.As(err, &need) {
		t.Fatalf("€20.01 above €10.00: %v, want it to ask", err)
	}
	if _, err := b.store.DecideAgentApproval(ctx, b.acct.WorkspaceID, need.ApprovalID, true); err != nil {
		t.Fatal(err)
	}
	var allow economy.PayeeAllowance
	if err := json.Unmarshal([]byte(`{"amount_minor":2000,"per":"day"}`), &allow); err != nil {
		t.Fatal(err)
	}
	rules, err := b.store.AllowPayee(ctx, b.acct.WorkspaceID, need.ApprovalID, allow)
	if err != nil {
		t.Fatalf("always allow €20.00 a day: %v", err)
	}
	if a := b6140Field(t, rules, "payee_allowances", b.payee.ID); b6140Field(t, a, "amount_minor") != 2000.0 || b6140Field(t, a, "per") != "day" {
		t.Fatalf("the allowance reads back as %v, want €20.00 a day", a)
	}
	if err := b.try("asks-again", 2001); !errors.As(err, &need) {
		t.Fatalf("€20.01 with €20.00 a day always allowed: %v, want it to ask", err)
	}
	b.pay(t, "allowed", 2000)
	if paid := b6140PaidOut(t, pool, b); paid != 2000 {
		t.Fatalf("the agent's account paid out %d, want 2000", paid)
	}
}

// B61.40 — a euro agent's daily conversion maximum of €50.00 converts €50.00 to dollars with the euro moved by 5%, and
// refuses €50.01; read off the ledger.
func TestB6140_TheDailyConversionMaximumHoldsInTheRulesCurrency(t *testing.T) {
	b, pool := b6140Payer(t, "ws-b6140-fx")
	ctx := context.Background()
	ws, agent := b.acct.WorkspaceID, b.acct.AgentID
	b.store.SetFXMargin(func(context.Context, economy.FeeQuerier, string) (int64, string, error) { return 0, "team", nil })
	for _, owner := range []string{"", agent} {
		if _, err := b.store.OpenCurrencyAccount(ctx, ws, owner, "USD", "test"); err != nil {
			t.Fatal(err)
		}
	}
	b6140Rules(t, b, `{"currency":"EUR","fx_daily_limit_minor":5000}`)
	convert := func(key string, minor int64) error {
		q, err := b.store.QuoteConversion(ctx, ws, agent, economy.Conversion{SellCurrency: "EUR", BuyCurrency: "USD", SellMinor: minor})
		if err != nil {
			t.Fatalf("quote %d: %v", minor, err)
		}
		_, err = b.store.Convert(ctx, ws, agent, q.ID, key)
		return err
	}
	if err := convert("over", 5001); !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "€50.00") {
		t.Fatalf("convert €50.01: %v, want refused by the €50.00 daily conversion maximum", err)
	}
	if err := convert("at", 5000); err != nil {
		t.Fatalf("convert €50.00: %v", err)
	}
	var sold int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(-sum(p.amount_minor), 0)::bigint FROM money_postings p
		JOIN money_entries e ON e.id = p.entry_id WHERE p.account_id = $1 AND e.kind = 'fx_conversion'`, b.acct.ID).Scan(&sold); err != nil || sold != 5000 {
		t.Fatalf("the agent's euro account sold %d (%v), want 5000", sold, err)
	}
}
