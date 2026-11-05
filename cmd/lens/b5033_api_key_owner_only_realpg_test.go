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
	"github.com/talyvor/lens/internal/tenant"
)

// B50.33: an agent's own key asked POST /v1/workspaces/{wsID}/api-keys for {"admin"} and got it, and an admin-scoped
// key passes ownerOnly — so the agent could rewrite its own rules. Only the owner (or the operator) manages keys now,
// and no caller grants a scope it does not hold.
func TestB5033_AnAgentsKeyCannotMintItselfAnAdminKey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5033"
	ts := tenant.NewStore(pool)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, economy.NewDualTokenStore(nil, pool, nil), ts)
	r.Post("/v1/workspaces/{wsID}/api-keys", ownerOnly(apiKeyCreateHandler(ts)))
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

	ag, err := economy.NewDualTokenStore(nil, pool, nil).CreateAgent(ctx, ws, "agent-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if w := call(owner, "/v1/workspaces/"+ws+"/agents/"+ag.ID+"/keys", `{"name":"a key"}`); w.Code != http.StatusCreated {
		t.Fatalf("issue the agent's key = %d %s", w.Code, w.Body.String())
	}
	var agentKeyID string
	if err := pool.QueryRow(ctx, `SELECT scoped_key_id FROM agent_account_keys WHERE agent_id = $1`, ag.ID).Scan(&agentKeyID); err != nil {
		t.Fatal(err)
	}
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: agentKeyID, Scopes: []string{auth.ScopeProxy}}
	adminKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k-admin", Scopes: []string{auth.ScopeAdmin}}
	mint := "/v1/workspaces/" + ws + "/api-keys"

	before := keys()
	for _, c := range []struct {
		name string
		who  *auth.AuthContext
		body string
	}{
		{"the agent's key asking for admin", agentKey, `{"name":"x","scopes":["admin"]}`},
		{"the agent's key asking for proxy", agentKey, `{"name":"x","scopes":["proxy"]}`},
		{"an admin key asking for a scope it does not hold", adminKey, `{"name":"x","scopes":["proxy"]}`},
	} {
		if w := call(c.who, mint, c.body); w.Code != http.StatusForbidden {
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
		{"an admin key minting an admin key", adminKey, `{"name":"a2","scopes":["admin"]}`},
	} {
		if w := call(c.who, mint, c.body); w.Code != http.StatusCreated {
			t.Errorf("%s = %d %s, want 201", c.name, w.Code, w.Body.String())
		}
	}
	if n := keys(); n != before+3 {
		t.Errorf("the owner's mints made %d key(s), want 3", n-before)
	}
}

// main.go composes these routes inline, so the wrapping is read from its source: every route that mints, rotates or
// revokes a workspace key goes through ownerOnly.
func TestB5033_EveryKeyMintingOrRevokingRouteIsOwnerOnly(t *testing.T) {
	_, f := mainGoAST(t)
	want := map[string]bool{
		"POST /v1/workspaces/{wsID}/api-keys":                            false,
		"POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate":             false,
		"POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate/begin":       false,
		"POST /v1/workspaces/{wsID}/key-rotations/{rotationID}/complete": false,
		"POST /v1/workspaces/{wsID}/key-rotations/{rotationID}/abandon":  false,
		"DELETE /v1/workspaces/{wsID}/api-keys/{keyID}":                  false,
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
			if id, ok := h.Fun.(*ast.Ident); ok && id.Name == "ownerOnly" {
				want[route] = true
			}
		}
		return true
	})
	for route, gated := range want {
		if !gated {
			t.Errorf("%s is not wrapped in ownerOnly in main.go: any key of the workspace can manage its keys", route)
		}
	}
}
