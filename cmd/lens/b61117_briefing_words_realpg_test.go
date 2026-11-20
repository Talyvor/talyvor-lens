package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// B61.117 — the briefing's items and line, and the simulator's reasons, name each amount in its currency and never
// say LXC: the parked credit is written as the dollars it is worth, so a day that spent credit and dollars reads as
// one dollar figure, not "$0.30 and $5.00".
func TestB61117_BriefingAndSimulatorSayTheCurrencyNotLXC(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	const ws = "ws-b61117"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, ws); err != nil {
		t.Fatal(err)
	}
	buyer, err := store.CreateAgent(ctx, ws, "buyer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.CreateAgent(ctx, ws, "writer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, buyer.ID, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer.ID, economy.AgentRules{ApprovalAboveULXC: 10_000_000, MaxPerRequestULXC: 50_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, buyer.ID, writer.ID, 3_000_000, "a logo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestPaymentApproval(ctx, ws, buyer.ID, writer.ID, 30_000_000, "", "a large order"); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO money_accounts (id, workspace_id, currency, purpose) VALUES ('macc_usd', 'ws-b61117', 'USD', 'company'),
			('macc_usd_hold', 'ws-b61117', 'USD', 'hold')`,
		`INSERT INTO money_payments (id, workspace_id, idempotency_key, account_id, hold_account_id, payee_id, payee_name,
			amount_minor, currency, funding, reference, status) VALUES
			('mpay_acme', 'ws-b61117', 'k1', 'macc_usd', 'macc_usd_hold', 'payee_acme', 'Acme', 500, 'USD', 'test', 'hosting', 'completed')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PauseAgent(ctx, ws, writer.ID, "its owner paused it"); err != nil {
		t.Fatal(err)
	}

	b, err := store.DailyBriefing(ctx, ws, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, it := range b.Items {
		said = append(said, it.Text, it.Reason)
	}
	said = append(said, b.Line)
	if len(b.Items) != 4 {
		t.Fatalf("the briefing lists %+v, want the approval, the paused agent and yesterday's two payments", b.Items)
	}
	if want := "1 approval waiting, 1 thing blocked, 2 payments yesterday, spent yesterday $5.30."; b.Line != want {
		t.Errorf("line = %q, want %q: the credit is dollars, so the dollars spent are one figure", b.Line, want)
	}

	for _, in := range []economy.SimulatedRequest{
		{AmountULXC: 30_000_000},                  // above the approval amount
		{AmountULXC: 60_000_000},                  // above the most for one request
		{AmountMinor: 600, Currency: "USD"},       // a payment from a dollar account, above the approval amount
		{AmountMinor: 9_000_000, Currency: "USD"}, // and above the most for one request
	} {
		sim, err := store.SimulateAgentRules(ctx, ws, buyer.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		if sim.Verdict == "allowed" || sim.Reason == "" {
			t.Errorf("simulating %+v = %+v, want a reason it is not allowed", in, sim)
		}
		said = append(said, sim.Reason)
	}
	for _, s := range said {
		if strings.Contains(s, "LXC") {
			t.Errorf("a sentence says LXC: %q", s)
		}
	}
}
