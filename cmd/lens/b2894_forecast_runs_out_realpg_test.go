package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.94 — /forecast gives each agent what it holds and when that runs out at its pace so far this month, written in
// the agent's own time zone; an agent that has spent nothing this month does not run out.
func TestAgentForecast_GivesBalanceAndRunsOutAtInTheAgentsTimeZone(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-forecast-runs-out"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	spender, err := store.CreateAgent(ctx, ws, "spender", "owner")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "payee", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, spender.ID, 5_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, spender.ID, payee.ID, 4_000_000, "run it down"); err != nil {
		t.Fatal(err)
	}
	// Spent 4 LXC over the month's first 10 days; 1 LXC left lasts 2.5 days more: 13 April 12:00 UTC, 22:00 in Sydney.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`ALTER TABLE agent_postings DISABLE TRIGGER audit_no_mutation`,
		`UPDATE agent_postings SET created_at = '2026-04-05T00:00:00Z' WHERE workspace_id = '` + ws + `'`,
		`ALTER TABLE agent_postings ENABLE TRIGGER audit_no_mutation`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_rules (agent_id, workspace_id, timezone) VALUES ($1, $2, 'Australia/Sydney')`, spender.ID, ws); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/forecast?at=2026-04-11T00:00:00Z", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET forecast = %d %s", w.Code, w.Body.String())
	}
	var f struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		`"` + spender.ID + `"`: {`1000000`, `"2026-04-13T22:00:00+10:00"`},
		`"` + payee.ID + `"`:   {`4000000`, `null`},
	}
	for _, a := range f.Agents {
		id := string(a["agent_id"])
		got, ok := want[id]
		if !ok {
			continue
		}
		if string(a["balance_ulxc"]) != got[0] || string(a["runs_out_at"]) != got[1] {
			t.Errorf("agent %s: balance_ulxc %q, runs_out_at %q; want %s and %s", id, a["balance_ulxc"], a["runs_out_at"], got[0], got[1])
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("forecast does not list %v: %s", want, w.Body.String())
	}
}
