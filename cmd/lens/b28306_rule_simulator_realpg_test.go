package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.306 — "would this request pass my rules?", asked through the route the console uses: the simulator says
// refused for an over-cap request and the postings count is unchanged, says allowed under the cap and
// approval_required above the approval amount without filing one, counts what the agent really spent, and agrees
// with a real payment of the same amount.
func TestB28306_TheSimulatorSaysRefusedForAnOverCapRequestAndThePostingsCountIsUnchanged(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-rule-simulator"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	member := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), member))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	newAgent := func(name string) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	agent, supplier := newAgent("buyer"), newAgent("supplier")
	if _, err := store.FundAgent(ctx, ws, agent, 10_000_000); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + agent
	if code, body := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":2000000,"approval_above_ulxc":1500000}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	// written counts every row a movement or its judgement writes: postings, the payee ledger, approvals, alerts.
	written := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_postings WHERE workspace_id = $1)
			+ (SELECT count(*) FROM agent_payee_payments WHERE workspace_id = $1)
			+ (SELECT count(*) FROM agent_approvals WHERE workspace_id = $1)
			+ (SELECT count(*) FROM agent_spend_alerts WHERE workspace_id = $1)`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	simulate := func(body string) economy.RuleSimulation {
		t.Helper()
		before := written()
		code, got := call(http.MethodPost, base+"/rules/simulate", body)
		if code != http.StatusOK {
			t.Fatalf("simulate %s = %d %s, want 200", body, code, got)
		}
		if after := written(); after != before {
			t.Fatalf("simulate %s wrote %d rows, want none", body, after-before)
		}
		var sim economy.RuleSimulation
		if err := json.Unmarshal([]byte(got), &sim); err != nil {
			t.Fatal(err)
		}
		return sim
	}
	payTo := `"payee":{"kind":"agent","id":"` + supplier + `"}`

	if sim := simulate(`{"amount_ulxc":3000000,"model":"gpt-4o"}`); sim.Verdict != "refused" || !strings.Contains(sim.Reason, "daily limit of $0.20 (2 LXC)") {
		t.Fatalf("a request over the daily cap = %+v, want refused naming the daily limit", sim)
	}
	if sim := simulate(`{"amount_ulxc":1000000,` + payTo + `}`); sim.Verdict != "allowed" || sim.BalanceULXC != 10_000_000 {
		t.Errorf("a payment under the cap = %+v, want allowed with the agent's balance of 10 LXC", sim)
	}
	if sim := simulate(`{"amount_ulxc":1800000,` + payTo + `}`); sim.Verdict != "approval_required" {
		t.Errorf("a payment above the approval amount = %+v, want approval_required", sim)
	}
	if sim := simulate(`{"amount_ulxc":3000000,` + payTo + `}`); sim.Verdict != "refused" {
		t.Fatalf("a payment over the cap = %+v, want refused", sim)
	}
	// The simulator agrees with the real thing: the same payment is refused and posts nothing.
	before := written()
	if code, body := call(http.MethodPost, base+"/pay", `{"to_agent_id":"`+supplier+`","amount_ulxc":3000000}`); code != http.StatusForbidden {
		t.Fatalf("the real payment over the cap = %d %s, want 403", code, body)
	}
	if written() != before {
		t.Fatal("the refused real payment wrote rows")
	}
	// It counts what the agent really spent: after paying 1 LXC, another 1.5 would pass the daily cap of 2.
	if code, body := call(http.MethodPost, base+"/pay", `{"to_agent_id":"`+supplier+`","amount_ulxc":1000000}`); code != http.StatusOK {
		t.Fatalf("a real payment under the cap = %d %s", code, body)
	}
	if sim := simulate(`{"amount_ulxc":1500000,` + payTo + `}`); sim.Verdict != "refused" || !strings.Contains(sim.Reason, "spent $0.10 (1 LXC)") {
		t.Errorf("after 1 LXC spent, a payment of 1.5 = %+v, want refused counting the 1 LXC spent", sim)
	}

	for body, want := range map[string]int{
		`{"amount_ulxc":-1}`: http.StatusBadRequest,
		`{"amount_ulxc":1,"payee":{"kind":"friend","id":"x"}}`: http.StatusBadRequest,
		`{"amount_ulxc":1,"daily_limit_ulxc":5}`:               http.StatusBadRequest,
	} {
		if code, got := call(http.MethodPost, base+"/rules/simulate", body); code != want {
			t.Errorf("simulate %s = %d %s, want %d", body, code, got, want)
		}
	}
	if code, got := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/agt_nobody/rules/simulate", `{"amount_ulxc":1}`); code != http.StatusNotFound {
		t.Errorf("simulate for no such agent = %d %s, want 404", code, got)
	}
}
