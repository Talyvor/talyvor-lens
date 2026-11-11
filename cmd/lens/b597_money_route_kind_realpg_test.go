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
)

// B59.7 on real Postgres: the gate reads each workspace's kind of money from workspaces.money_kind (B59.6), and a
// workspace with no row reads hybrid, as every one made before B59.6. A fiat workspace is refused an on-chain route and
// a stablecoin workspace a fiat one; each reaches its own side, and a hybrid workspace both.
func TestB597_TheGateReadsTheWorkspacesKind(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	for _, ws := range []string{"fiat", "stablecoin", "hybrid"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', $2)`,
			"ws-b597-"+ws, ws); err != nil {
			t.Fatal(err)
		}
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	if kind, err := bank.WorkspaceMoneyKind(ctx, "ws-b597-none"); err != nil || kind != economy.MoneyKindHybrid {
		t.Fatalf("a workspace with no row reads %q (%v), want hybrid", kind, err)
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ws := req.Header.Get("X-Test-Workspace")
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws})))
		})
	})
	r.Group(func(g chi.Router) { // as main.go's authed group: a group's middleware runs once the route is matched
		g.Use(moneyKindGate(bank))
		reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		g.Post("/v1/workspaces/{wsID}/agents/{agentID}/sweep", reached) // on-chain, the workspace in the path
		g.Get("/v1/money/banks", reached)                               // fiat, the workspace the key's
	})

	for _, c := range []struct {
		ws, method, path string
		refused          bool
	}{
		{"fiat", http.MethodPost, "/v1/workspaces/ws-b597-fiat/agents/a1/sweep", true},
		{"fiat", http.MethodGet, "/v1/money/banks", false},
		{"stablecoin", http.MethodGet, "/v1/money/banks", true},
		{"stablecoin", http.MethodPost, "/v1/workspaces/ws-b597-stablecoin/agents/a1/sweep", false},
		{"hybrid", http.MethodGet, "/v1/money/banks", false},
		{"hybrid", http.MethodPost, "/v1/workspaces/ws-b597-hybrid/agents/a1/sweep", false},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("X-Test-Workspace", "ws-b597-"+c.ws)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		switch {
		case c.refused && (rec.Code != http.StatusConflict || out["code"] != "wrong_money_kind" || out["money_kind"] != c.ws):
			t.Errorf("a %s workspace: %s %s = %d %s, want 409 wrong_money_kind naming %s", c.ws, c.method, c.path, rec.Code, rec.Body.String(), c.ws)
		case !c.refused && rec.Code != http.StatusOK:
			t.Errorf("a %s workspace: %s %s = %d %s, want it reached", c.ws, c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
