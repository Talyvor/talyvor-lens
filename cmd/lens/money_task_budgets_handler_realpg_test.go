package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B28.310 — the owner gives an agent a £300.00 budget to book a venue, for an hour. The company's GBP account pays it
// in; the agent spends £120.00 of it and is also paid £50.00 that is not the budget's. At the hour, one entry returns
// the £180.00 left — not the agent's whole £230.00 — from the agent's account to the company's, and its postings sum to
// zero. Before the hour nothing moves, and running it again, or the owner ending it after, moves nothing more.
func TestMoneyTaskBudgets_AtExpiryOneBalancedReturnOfTheUnspentAmount(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28310-task-budgets"
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screening.NewScreener(pool, registry))
	agent, err := store.CreateAgent(ctx, ws, "Planner", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentAcct, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, company.ID, 100000, "seed"); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyTaskBudgetRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		_ = json.Unmarshal(w.Body.Bytes(), out)
		return w.Code
	}
	ends := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var b economy.MoneyTaskBudget
	if code := call(http.MethodPost, "/v1/money/task-budgets", `{"agent_id":"`+agent.ID+`","task":"Book the venue","currency":"GBP","amount_minor":30000,"expires_at":"`+
		ends.Format(time.RFC3339)+`","idempotency_key":"venue"}`, &b); code != http.StatusOK || b.Status != "active" || b.FundEntryID == "" {
		t.Fatalf("set task budget: %d %+v", code, b)
	}
	var again economy.MoneyTaskBudget
	if code := call(http.MethodPost, "/v1/money/task-budgets", `{"agent_id":"`+agent.ID+`","task":"Book the venue","currency":"GBP","amount_minor":30000,"expires_at":"`+
		ends.Format(time.RFC3339)+`","idempotency_key":"venue"}`, &again); code != http.StatusOK || again.ID != b.ID {
		t.Fatalf("the same budget asked for again: %d %+v; want %s", code, again, b.ID)
	}

	var partner string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsOut, Kind: "test_spend",
		IdempotencyKey: "venue-deposit", Funding: economy.FundingTest, Counterparty: "Test venue", Postings: []economy.MoneyPosting{
			{AccountID: agentAcct.ID, AmountMinor: -12000}, {AccountID: partner, AmountMinor: 12000}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityCurrencyAccounts, Kind: "test_top_up",
		IdempotencyKey: "not-the-budget", Funding: economy.FundingTest, Postings: []economy.MoneyPosting{
			{AccountID: company.ID, AmountMinor: -5000}, {AccountID: agentAcct.ID, AmountMinor: 5000}}}); err != nil {
		t.Fatal(err)
	}

	if returned, refused, err := store.RunMoneyTaskBudgets(ctx, ends.Add(-time.Second)); err != nil || returned+refused != 0 {
		t.Fatalf("before the hour: returned %d refused %d, %v; want nothing", returned, refused, err)
	}
	if returned, _, err := store.RunMoneyTaskBudgets(ctx, ends); err != nil || returned != 1 {
		t.Fatalf("at the hour: returned %d, %v; want 1", returned, err)
	}
	if returned, refused, err := store.RunMoneyTaskBudgets(ctx, ends.Add(time.Minute)); err != nil || returned+refused != 0 {
		t.Fatalf("again: returned %d refused %d, %v; want nothing", returned, refused, err)
	}
	var ended economy.MoneyTaskBudget
	if code := call(http.MethodPost, "/v1/money/task-budgets/"+b.ID+"/end", "", &ended); code != http.StatusOK || ended.ReturnEntryID == "" {
		t.Fatalf("ended after it returned: %d %+v", code, ended)
	}

	// The ledger: one task_budget_return entry, on the two accounts, summing to zero.
	rows, err := pool.Query(ctx, `SELECT e.id, p.account_id, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		WHERE e.workspace_id = $1 AND e.kind = 'task_budget_return' ORDER BY p.line`, ws)
	if err != nil {
		t.Fatal(err)
	}
	entries, postings, sum := map[string]bool{}, map[string]int64{}, int64(0)
	for rows.Next() {
		var id, account string
		var amount int64
		if err := rows.Scan(&id, &account, &amount); err != nil {
			t.Fatal(err)
		}
		entries[id], postings[account], sum = true, postings[account]+amount, sum+amount
	}
	rows.Close()
	if len(entries) != 1 || sum != 0 || postings[agentAcct.ID] != -18000 || postings[company.ID] != 18000 || !entries[ended.ReturnEntryID] {
		t.Errorf("return entries %v, postings %v summing to %d; want one entry, agent -18000 and company +18000, summing to 0", entries, postings, sum)
	}
	for account, want := range map[string]int64{company.ID: 83000, agentAcct.ID: 5000} {
		if got, err := store.Balance(ctx, ws, account); err != nil || got.TestMinor != want {
			t.Errorf("%s holds %d test pence (%v); want %d", account, got.TestMinor, err, want)
		}
	}

	var list struct {
		TaskBudgets []economy.MoneyTaskBudget `json:"task_budgets"`
	}
	if code := call(http.MethodGet, "/v1/money/task-budgets?agent_id="+agent.ID, "", &list); code != http.StatusOK || len(list.TaskBudgets) != 1 {
		t.Fatalf("list: %d %+v", code, list)
	}
	if got := list.TaskBudgets[0]; got.Status != "returned" || got.SpentMinor != 12000 || got.ReturnedMinor != 18000 || got.Task != "Book the venue" {
		t.Errorf("the budget as listed: %+v; want returned, 12000 spent, 18000 back", got)
	}
}
