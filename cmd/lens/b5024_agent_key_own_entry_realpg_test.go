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

// B50.24 — an agent's own key reads only its own forecast, alerts and book entry: never another agent's, nor the
// workspace's totals. The owner still reads every agent's.
func TestAgentForecastAlertsBook_AnAgentsKeySeesOnlyItsOwnEntry(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-own-forecast"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(who *auth.AuthContext, path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	a, err := store.CreateAgent(ctx, ws, "agent-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateAgent(ctx, ws, "agent-b", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, ag := range []string{a.ID, b.ID} {
		if _, err := store.FundAgent(ctx, ws, ag, 10_000_000); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO agent_spend_alerts (id, workspace_id, agent_id, last_hour_ulxc, usual_per_hour_ulxc, paused)
			VALUES ($1, $2, $3, 5000000, 1000, false)`, "alert-"+ag, ws, ag); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+a.ID+"/keys", strings.NewReader(`{"name":"a key"}`))
	req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue agent A's key = %d %s", w.Code, w.Body.String())
	}
	var keyA string
	if err := pool.QueryRow(ctx, `SELECT scoped_key_id FROM agent_account_keys WHERE agent_id = $1`, a.ID).Scan(&keyA); err != nil {
		t.Fatal(err)
	}
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: keyA, Scopes: []string{auth.ScopeProxy}}
	base := "/v1/workspaces/" + ws + "/agents"

	for _, path := range []string{base + "/forecast?at=2026-04-11T00:00:00Z", base + "/alerts", base} {
		body := call(agentKey, path)
		if strings.Contains(body, b.ID) || strings.Contains(body, "agent-b") {
			t.Errorf("agent A's key sees agent B on %s: %s", path, body)
		}
		if !strings.Contains(body, a.ID) {
			t.Errorf("agent A's key does not see itself on %s: %s", path, body)
		}
		if owners := call(owner, path); !strings.Contains(owners, a.ID) || !strings.Contains(owners, b.ID) {
			t.Errorf("the owner does not see both agents on %s: %s", path, owners)
		}
	}
	var book economy.AgentBook
	if err := json.Unmarshal([]byte(call(agentKey, base)), &book); err != nil {
		t.Fatal(err)
	}
	if book.WorkspaceBalanceULXC != 0 || book.AllocatedULXC != 0 || book.UnallocatedULXC != 0 {
		t.Errorf("agent A's key sees the workspace's totals: %+v", book)
	}
}
