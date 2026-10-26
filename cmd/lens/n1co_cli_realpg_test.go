package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B45.33 — `lens n1co` creates the N1CO workspace with its agent wallet on test money and a monthly budget once (a
// second run changes nothing), and a payment from that wallet to a vendor no rule lets through goes to approval with
// nothing paid.
func TestN1CO_WalletOnceAndNewVendorGoesToApproval(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	args := []string{"-issuer", "https://auth.talyvor.com", "-subject", "n1co", "-monthly-budget", "200", "-test-money", "50"}
	ws := deriveWorkspaceID("https://auth.talyvor.com\x00n1co")

	var first, second bytes.Buffer
	if err := n1coCommand(ctx, pool, store, 0, args, &first); err != nil {
		t.Fatal(err)
	}
	if err := n1coCommand(ctx, pool, store, 0, args, &second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "its key, shown this once: ") || !strings.Contains(second.String(), "nothing changed") {
		t.Fatalf("first run said %q, second %q", first.String(), second.String())
	}

	var workspaces, agents, starters, keys int
	var name, agentID, funding string
	var money int64
	if err := pool.QueryRow(ctx, `SELECT count(*), max(name) FROM workspaces WHERE id = $1`, ws).Scan(&workspaces, &name); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*), max(id) FROM agent_accounts WHERE workspace_id = $1 AND kind = 'agent' AND name = $2`,
		ws, N1COAgentName).Scan(&agents, &agentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0), max(metadata->>'funding') FROM lxc_ledger WHERE workspace_id = $1 AND type = $2`,
		ws, economy.LXCTypeStarterTest).Scan(&starters, &money, &funding); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_account_keys WHERE agent_id = $1`, agentID).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if workspaces != 1 || name != "N1CO" || agents != 1 || keys != 1 || starters != 1 || money != 50_000_000 || funding != economy.FundingTest {
		t.Fatalf("after two runs: %d workspace %q, %d agent, %d key, %d test-money rows of %d µLXC (%q); want one of each, 50 LXC of test money",
			workspaces, name, agents, keys, starters, money, funding)
	}
	book, err := store.AgentBook(ctx, ws)
	if err != nil || len(book.Agents) != 1 || book.Agents[0].BalanceULXC != 50_000_000 {
		t.Fatalf("N1CO's wallet = %+v (%v), want it holding the 50 LXC", book.Agents, err)
	}
	rules, err := store.GetAgentRules(ctx, ws, agentID)
	if err != nil || rules.MonthlyLimitULXC != 200_000_000 || rules.ApprovalAboveULXC == 0 {
		t.Fatalf("rules = %+v (%v), want a 200 LXC monthly budget and an approval amount", rules, err)
	}

	// N1CO pays a vendor it has never paid, for credits: no signed mandate lets it through, so it waits for approval.
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_vendor', $1, 'Model Credits Ltd', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	var keyID string
	if err := pool.QueryRow(ctx, `SELECT scoped_key_id FROM agent_account_keys WHERE agent_id = $1`, agentID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	req, _, err := store.PreparePaymentRequest(ctx, ws, "key:"+keyID, "n1co-credits-1", economy.PaymentRequestIn{
		Client: "key:" + keyID, AgentID: agentID,
		Items: []economy.PaymentRequestItemIn{{PayeeID: "payee_vendor", Currency: "GBP", AmountMinor: 2000, Reference: "credits"}}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending_approval" || len(req.Items) != 1 || req.Items[0].Decision != economy.ItemNeedsApproval {
		t.Fatalf("the vendor payment = %s %+v, want it pending approval", req.Status, req.Items)
	}
	var paid, postings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1`, ws).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if paid != 0 || postings != 1 {
		t.Fatalf("before approval: %d payments and %d ledger rows, want none paid and only the test money", paid, postings)
	}
}
