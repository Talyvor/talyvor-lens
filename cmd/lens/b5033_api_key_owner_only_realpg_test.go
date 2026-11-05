package main

import (
	"context"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
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
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, economy.NewDualTokenStore(nil, pool, nil), ts)
	r.Post("/v1/workspaces/{wsID}/api-keys", keyOwnerOnly(apiKeyCreateHandler(ts)))
	r.Post("/v1/api/keys", apiKeysRetired)
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
		{"an analytics key minting a key", analyticsKey, mint, `{"name":"x","scopes":["proxy"]}`},
	} {
		if w := call(c.who, c.path, c.body); w.Code != http.StatusForbidden {
			t.Errorf("%s = %d %s, want 403", c.name, w.Code, w.Body.String())
		}
	}
	if n := keys(); n != before {
		t.Errorf("refused mints still made %d key(s)", n-before)
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
	// The legacy route minted scopeless keys, which no longer pass any scope gate: it is gone, for the owner too, and
	// names the route that replaced it.
	for _, who := range []*auth.AuthContext{owner, operator, analyticsKey} {
		if w := call(who, "/v1/api/keys", `{"name":"l"}`); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "/v1/workspaces/{wsID}/api-keys") {
			t.Errorf("POST /v1/api/keys = %d %s, want 410 naming the workspace route", w.Code, w.Body.String())
		}
	}
	if n := keys(); n != before+3 {
		t.Errorf("the retired route made %d key(s)", n-before-3)
	}
}

// An admin key passed ownerOnly, so in a workspace with no passkey or device key yet it could register the first
// approval key and then approve what its agents ask. Registering one is the owner's signed-in session's alone.
func TestB5033_OnlyTheOwnersSessionRegistersAnApprovalKey(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b5033-approval-keys"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: "app.talyvor.com", Origins: []string{"https://app.talyvor.com"}}, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountDeviceKeyRoutes(r, store)
	call := func(who *auth.AuthContext, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	adminKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k-admin", Scopes: []string{auth.ScopeAdmin}}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	passkeys, devices := "/v1/workspaces/"+ws+"/agents/passkeys", "/v1/money/devices"
	for _, path := range []string{passkeys + "/challenge", passkeys, devices + "/challenge", devices} {
		if w := call(adminKey, path, `{}`); w.Code != http.StatusForbidden {
			t.Errorf("an admin key on POST %s = %d %s, want 403", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{passkeys + "/challenge", devices + "/challenge"} {
		if w := call(owner, path, `{}`); w.Code != http.StatusOK {
			t.Errorf("the owner on POST %s = %d %s, want 200", path, w.Code, w.Body.String())
		}
	}
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
// revokes a workspace key goes through keyOwnerOnly — the legacy DELETE /v1/api/keys included — and the legacy mint is
// the retired handler.
func TestB5033_EveryKeyMintingOrRevokingRouteIsOwnerOnly(t *testing.T) {
	_, f := mainGoAST(t)
	retired := false
	want := map[string]bool{
		"POST /v1/workspaces/{wsID}/api-keys":                            false,
		"POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate":             false,
		"POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate/begin":       false,
		"POST /v1/workspaces/{wsID}/key-rotations/{rotationID}/complete": false,
		"POST /v1/workspaces/{wsID}/key-rotations/{rotationID}/abandon":  false,
		"DELETE /v1/workspaces/{wsID}/api-keys/{keyID}":                  false,
		"DELETE /v1/api/keys/{keyID}":                                    false,
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
		if route == "POST /v1/api/keys" {
			id, ok := call.Args[1].(*ast.Ident)
			retired = ok && id.Name == "apiKeysRetired"
		}
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
	if !retired {
		t.Error("POST /v1/api/keys is not mounted as apiKeysRetired in main.go: it can still mint a scopeless key")
	}
}
