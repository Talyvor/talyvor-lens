package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/opsusage"
)

// credentialAuth presents one credential per Authorization header: the admin key, the suite's operator
// read key, or a workspace's own key.
type credentialAuth struct{}

func (credentialAuth) Authenticate(r *http.Request) (*auth.AuthContext, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer admin-key":
		return &auth.AuthContext{IsAdmin: true}, nil
	case "Bearer operator-read-key":
		return &auth.AuthContext{Scopes: []string{auth.ScopeOperatorRead}}, nil
	}
	return &auth.AuthContext{WorkspaceID: "ws-op-a"}, nil
}

// B18.17 — the three operator-screen reads answer an admin credential (and the operator read key) with
// every workspace's spend, held LENS and last activity, synthetic workspaces apart, and refuse everyone
// else — a workspace's own key, and any write.
func TestOperatorWorkspaceReads_SpendHeldAndLastActivityPerWorkspace_AdminOnly(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	for _, ws := range []struct {
		id        string
		synthetic bool
	}{{"ws-op-a", false}, {"ws-op-b", false}, {"ws-op-synth", true}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`, ws.id, ws.synthetic); err != nil {
			t.Fatal(err)
		}
	}
	// Both of ws-op-a's events inside the month the database's own clock counts as current, on any day of it.
	var monthStart, lastA time.Time
	if err := pool.QueryRow(ctx, `SELECT date_trunc('month', now()), date_trunc('second', now())`).Scan(&monthStart, &lastA); err != nil {
		t.Fatal(err)
	}
	lastA = lastA.UTC()
	for _, e := range []struct {
		ws   string
		cost float64
		at   time.Time
	}{{"ws-op-a", 1.25, monthStart}, {"ws-op-a", 0.50, lastA}, {"ws-op-synth", 9, lastA}} {
		if _, err := pool.Exec(ctx, `INSERT INTO token_events (provider, model, input_tokens, output_tokens, workspace_id, created_at,
			team, feature, cost_usd, pii_detected, session_id, request_id)
			VALUES ('openai', 'gpt-4o', 10, 5, $1, $2, '', '', $3, false, '', gen_random_uuid()::text)`, e.ws, e.at, e.cost); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lens_token_balances (workspace_id, held_balance) VALUES ('ws-op-b', 822)`); err != nil {
		t.Fatal(err)
	}

	reader := opsusage.NewReader(pool)
	routes := map[string]http.HandlerFunc{
		"spend":         requireAdminOrOperatorRead(credentialAuth{}, newOperatorWorkspaceReadHandler(reader.SpendByWorkspace)),
		"held":          requireAdminOrOperatorRead(credentialAuth{}, newOperatorWorkspaceReadHandler(reader.HeldByWorkspace)),
		"last-activity": requireAdminOrOperatorRead(credentialAuth{}, newOperatorWorkspaceReadHandler(reader.ActivityByWorkspace)),
	}
	call := func(route, method, key string, into any) int {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/admin/workspaces/"+route, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		routes[route](w, req)
		if into != nil && w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code
	}

	var spend struct{ Workspaces []opsusage.WorkspaceSpend }
	var held struct{ Workspaces []opsusage.WorkspaceHeld }
	var active struct{ Workspaces []opsusage.WorkspaceActivity }
	if call("spend", http.MethodGet, "admin-key", &spend) != 200 || call("held", http.MethodGet, "admin-key", &held) != 200 ||
		call("last-activity", http.MethodGet, "admin-key", &active) != 200 {
		t.Fatal("an admin credential was refused")
	}
	if len(spend.Workspaces) != 2 || spend.Workspaces[0] != (opsusage.WorkspaceSpend{WorkspaceID: "ws-op-a", CurrentMonthUSD: 1.75, AllTimeUSD: 1.75, Requests: 2}) ||
		spend.Workspaces[1] != (opsusage.WorkspaceSpend{WorkspaceID: "ws-op-b"}) {
		t.Errorf("spend = %+v; want ws-op-a 1.75 over 2 requests and ws-op-b nothing, the synthetic workspace apart", spend.Workspaces)
	}
	if len(held.Workspaces) != 2 || held.Workspaces[0].HeldULENS != 0 || held.Workspaces[1] != (opsusage.WorkspaceHeld{WorkspaceID: "ws-op-b", HeldULENS: 822}) {
		t.Errorf("held = %+v; want ws-op-b holding 822 µLENS", held.Workspaces)
	}
	if len(active.Workspaces) != 2 || active.Workspaces[0].LastRequestAt == nil || !active.Workspaces[0].LastRequestAt.Equal(lastA) || active.Workspaces[1].LastRequestAt != nil {
		t.Errorf("last activity = %+v; want ws-op-a at %s and ws-op-b never", active.Workspaces, lastA)
	}

	for route := range routes {
		if code := call(route, http.MethodGet, "operator-read-key", nil); code != http.StatusOK {
			t.Errorf("%s: the operator read key got %d, want 200", route, code)
		}
		if code := call(route, http.MethodGet, "workspace-key", nil); code != http.StatusUnauthorized {
			t.Errorf("%s: a workspace's own key got %d, want 401", route, code)
		}
		if code := call(route, http.MethodPost, "operator-read-key", nil); code != http.StatusMethodNotAllowed {
			t.Errorf("%s: a write with the operator read key got %d, want 405", route, code)
		}
	}
}
