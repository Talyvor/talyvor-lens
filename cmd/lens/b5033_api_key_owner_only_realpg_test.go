package main

import (
	"context"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B50.33: an agent's own key asked POST /v1/workspaces/{wsID}/api-keys for {"admin"} and got it, and an admin-scoped
// key passes ownerOnly — so the agent could rewrite its own rules. Only the owner's signed-in session (or the operator)
// creates, rotates or revokes keys now; no key does, an admin-scoped one included.
func TestB5033_OnlyTheOwnersSessionMintsKeys(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5033"
	ts := tenant.NewStore(pool)
	legacy := &countingKeyGen{}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, economy.NewDualTokenStore(nil, pool, nil), ts)
	r.Post("/v1/workspaces/{wsID}/api-keys", keyOwnerOnly(apiKeyCreateHandler(ts)))
	r.Post("/v1/api/keys", keyOwnerOnly(newCreateAPIKeyHandler(legacy)))
	call := func(who *auth.AuthContext, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	keys := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_api_keys WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeAnalytics, auth.ScopeKeys}}
	operator := &auth.AuthContext{AuthMethod: auth.MethodGlobalKey, IsAdmin: true}

	ag, err := economy.NewDualTokenStore(nil, pool, nil).CreateAgent(ctx, ws, "agent-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if w := call(owner, "/v1/workspaces/"+ws+"/agents/"+ag.ID+"/keys", `{"name":"a key"}`); w.Code != http.StatusCreated {
		t.Fatalf("the owner issuing the agent's key = %d %s", w.Code, w.Body.String())
	}
	var agentKeyID string
	if err := pool.QueryRow(ctx, `SELECT scoped_key_id FROM agent_account_keys WHERE agent_id = $1`, ag.ID).Scan(&agentKeyID); err != nil {
		t.Fatal(err)
	}
	wsKey := func(id string, scopes ...string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: id, Scopes: scopes}
	}
	agentKey, adminKey, analyticsKey := wsKey(agentKeyID, auth.ScopeProxy), wsKey("k-admin", auth.ScopeAdmin), wsKey("k-an", auth.ScopeAnalytics)
	mint := "/v1/workspaces/" + ws + "/api-keys"

	before := keys()
	for _, c := range []struct {
		name string
		who  *auth.AuthContext
		path string
		body string
	}{
		{"the agent's key asking for admin", agentKey, mint, `{"name":"x","scopes":["admin"]}`},
		{"the agent's key asking for proxy", agentKey, mint, `{"name":"x","scopes":["proxy"]}`},
		{"an admin key minting an admin key", adminKey, mint, `{"name":"x","scopes":["admin"]}`},
		{"an admin key issuing an agent's key", adminKey, "/v1/workspaces/" + ws + "/agents/" + ag.ID + "/keys", `{"name":"x"}`},
		{"a key that claims the keys scope", wsKey("k-forged", auth.ScopeKeys), mint, `{"name":"x","scopes":["proxy"]}`},
		{"the browser chat's session key", &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}, mint, `{"name":"x","scopes":["proxy"]}`},
		{"an analytics key minting a legacy (scopeless) key", analyticsKey, "/v1/api/keys", `{"name":"x"}`},
		{"an admin key minting a legacy key", adminKey, "/v1/api/keys", `{"name":"x"}`},
	} {
		if w := call(c.who, c.path, c.body); w.Code != http.StatusForbidden {
			t.Errorf("%s = %d %s, want 403", c.name, w.Code, w.Body.String())
		}
	}
	if n := keys(); n != before || legacy.n != 0 {
		t.Errorf("refused mints still made %d workspace key(s) and %d legacy key(s)", n-before, legacy.n)
	}

	for _, c := range []struct {
		name string
		who  *auth.AuthContext
		body string
	}{
		{"the owner minting a proxy key", owner, `{"name":"p","scopes":["proxy"]}`},
		{"the owner minting an admin key", owner, `{"name":"a","scopes":["admin"]}`},
		{"the operator minting an admin key", operator, `{"name":"a2","scopes":["admin"]}`},
	} {
		if w := call(c.who, mint, c.body); w.Code != http.StatusCreated {
			t.Errorf("%s = %d %s, want 201", c.name, w.Code, w.Body.String())
		}
	}
	if n := keys(); n != before+3 {
		t.Errorf("the owner's mints made %d key(s), want 3", n-before)
	}
	if w := call(owner, "/v1/api/keys", `{"name":"l"}`); w.Code != http.StatusCreated || legacy.n != 1 {
		t.Errorf("the owner minting a legacy key = %d (%d minted), want 201 and 1", w.Code, legacy.n)
	}
}

// countingKeyGen stands in for the legacy auth.KeyStore and counts the keys it was asked for.
type countingKeyGen struct{ n int }

func (g *countingKeyGen) GenerateKey(_ context.Context, ws, _, _ string, _ *time.Time) (string, *auth.APIKey, error) {
	g.n++
	return "tlv_legacy", &auth.APIKey{ID: "legacy-1", WorkspaceID: ws}, nil
}

// The admin keys minted before B50.33 cannot be told apart from the owner's: the sweep lists them all, and --revoke
// revokes each, leaving its revocation on the workspace's audit chain, and leaves every other key alone.
func TestB5033_TheSweepRevokesEveryAdminKeyOnTheAuditChain(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5033-sweep"
	ts := tenant.NewStore(pool)
	_, admin, err := ts.CreateAPIKey(ctx, ws, "minted by an agent", []string{"admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, proxy, err := ts.CreateAPIKey(ctx, ws, "an agent's key", []string{"proxy"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := func(id string) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_api_keys WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	var out strings.Builder
	if err := adminKeySweep(ctx, pool, nil, "operator-cli:test", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), admin.ID) || strings.Contains(out.String(), proxy.ID) || !live(admin.ID) {
		t.Fatalf("the listing must name the admin key, not the proxy key, and revoke nothing:\n%s", out.String())
	}

	out.Reset()
	if err := adminKeySweep(ctx, pool, []string{"--revoke"}, "operator-cli:test", &out); err != nil {
		t.Fatal(err)
	}
	if live(admin.ID) || !live(proxy.ID) {
		t.Fatalf("after --revoke: admin key live = %v (want false), proxy key live = %v (want true)", live(admin.ID), live(proxy.ID))
	}
	var kind, body string
	if err := pool.QueryRow(ctx, `SELECT kind, body FROM audit_chain WHERE workspace_id = $1 AND subject = $2`,
		ws, "api_key:"+admin.ID).Scan(&kind, &body); err != nil {
		t.Fatalf("no audit-chain entry for the revoked key: %v", err)
	}
	if kind != "api_key.revoked" || !strings.Contains(body, "operator-cli:test") {
		t.Errorf("audit entry = %s %s, want api_key.revoked by operator-cli:test", kind, body)
	}
}

// main.go composes these routes inline, so the wrapping is read from its source: every route that mints, rotates or
// revokes a workspace key goes through keyOwnerOnly — the legacy /v1/api/keys pair included.
func TestB5033_EveryKeyMintingOrRevokingRouteIsOwnerOnly(t *testing.T) {
	_, f := mainGoAST(t)
	want := map[string]bool{
		"POST /v1/workspaces/{wsID}/api-keys":                            false,
		"POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate":             false,
		"POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate/begin":       false,
		"POST /v1/workspaces/{wsID}/key-rotations/{rotationID}/complete": false,
		"POST /v1/workspaces/{wsID}/key-rotations/{rotationID}/abandon":  false,
		"DELETE /v1/workspaces/{wsID}/api-keys/{keyID}":                  false,
		"POST /v1/api/keys":           false,
		"DELETE /v1/api/keys/{keyID}": false,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		lit, ok2 := call.Args[0].(*ast.BasicLit)
		if !ok || !ok2 {
			return true
		}
		route := strings.ToUpper(sel.Sel.Name) + " " + strings.Trim(lit.Value, `"`)
		if _, tracked := want[route]; !tracked {
			return true
		}
		if h, ok := call.Args[1].(*ast.CallExpr); ok {
			if id, ok := h.Fun.(*ast.Ident); ok && id.Name == "keyOwnerOnly" {
				want[route] = true
			}
		}
		return true
	})
	for route, gated := range want {
		if !gated {
			t.Errorf("%s is not wrapped in keyOwnerOnly in main.go: a key of the workspace can manage its keys", route)
		}
	}
}
