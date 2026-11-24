package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/support"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B28.342 — the owner gives people a role in the workspace (Viewer, Finance, Approver, Admin), each signs in to it
// through POST /v1/provision/roles, and Lens enforces the role on wallet actions: B28.70's DONE line, "a Viewer's
// attempt to fund writes no posting", asserted on agent_postings, through the real auth middleware.
func TestWorkspaceRoles_AViewersFundWritesNoPostingAndFinancesDoes(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-roles", "b28342-provision-secret"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	wsm := workspace.New(pool)
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: ws, Name: "Roles Ltd"}); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	am := auth.NewManager("", key, auth.New(pool), tenant.NewStore(pool)).WithRoles(wsm)
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), am))
		authed.Use(workspaceIsolationMiddleware)
		mountAgentAccountRoutes(authed, economy.NewDualTokenStore(nil, pool, nil), tenant.NewStore(pool))
		mountWorkspaceRoleRoutes(authed, wsm)
	})
	mountRoleSessionRoute(r, secret, wsm, func(workspaceID, personKey, role string, ttl time.Duration) (string, error) {
		return auth.GenerateRoleToken(workspaceID, personKey, role, key, ttl)
	})

	call := func(token, method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	postings := func() (n, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings`).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	owner, err := auth.GenerateToken(ws, ws, provisionScopes, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws

	code, a := call(owner, http.MethodPost, base+"/agents", `{"name":"buyer"}`)
	if code != http.StatusCreated {
		t.Fatalf("the owner's create = %d %v", code, a)
	}
	fund := base + "/agents/" + a["id"].(string) + "/fund"

	for email, role := range map[string]string{"vera@roles.test": "viewer", "fin@roles.test": "finance", "ada@roles.test": "approver"} {
		if code, out := call(owner, http.MethodPut, base+"/roles", `{"email":"`+email+`","role":"`+role+`"}`); code != http.StatusOK || out["role"] != role {
			t.Fatalf("the owner gives %s %s = %d %v", email, role, code, out)
		}
	}
	if code, _ := call(owner, http.MethodPut, base+"/roles", `{"email":"x@roles.test","role":"owner"}`); code != http.StatusBadRequest {
		t.Fatalf("a role that is not one of the four = %d, want 400", code)
	}
	session := func(identity, email string) (int, string) {
		t.Helper()
		code, out := call("", http.MethodPost, "/v1/provision/roles",
			`{"identity":"`+identity+`","email":"`+email+`","mode":"session","account_id":"`+ws+`"}`, provisionSecretHeader, secret)
		tok, _ := out["token"].(string)
		return code, tok
	}
	code, list := call("", http.MethodPost, "/v1/provision/roles", `{"identity":"iss|vera","email":"Vera@roles.test","mode":"list"}`,
		provisionSecretHeader, secret)
	if accts, _ := list["accounts"].([]any); code != http.StatusOK || len(accts) != 1 || accts[0].(map[string]any)["role"] != "viewer" {
		t.Fatalf("the viewer's accounts = %d %v, want the one workspace, as viewer", code, list)
	}
	_, viewer := session("iss|vera", "vera@roles.test")
	_, finance := session("iss|fin", "fin@roles.test")
	_, approver := session("iss|ada", "ada@roles.test")
	if viewer == "" || finance == "" || approver == "" {
		t.Fatal("a role was given no session")
	}
	if code, _ := session("iss|someone-else", "vera@roles.test"); code != http.StatusConflict {
		t.Fatalf("another person signing in with the viewer's email = %d, want 409", code)
	}

	// The Viewer reads the wallet, and its attempt to fund writes no posting.
	if code, book := call(viewer, http.MethodGet, base+"/agents", ``); code != http.StatusOK || len(book["agents"].([]any)) != 1 {
		t.Fatalf("the viewer reads the agents = %d %v", code, book)
	}
	if code, _ := call(viewer, http.MethodPost, fund, `{"amount_ulxc":1000000}`); code != http.StatusForbidden {
		t.Fatalf("the viewer funds = %d, want 403", code)
	}
	if code, _ := call(approver, http.MethodPost, fund, `{"amount_ulxc":1000000}`); code != http.StatusForbidden {
		t.Fatalf("the approver funds = %d, want 403", code)
	}
	if code, _ := call(viewer, http.MethodPut, base+"/roles", `{"email":"vera@roles.test","role":"admin"}`); code != http.StatusForbidden {
		t.Fatalf("the viewer makes itself admin = %d, want 403", code)
	}
	if n, _ := postings(); n != 0 {
		t.Fatalf("agent_postings holds %d rows after a viewer's and an approver's fund, want 0", n)
	}

	// Finance funds: one balanced entry.
	if code, out := call(finance, http.MethodPost, fund, `{"amount_ulxc":1000000}`); code != http.StatusOK || out["balance_ulxc"].(float64) != 1_000_000 {
		t.Fatalf("finance funds = %d %v", code, out)
	}
	if n, sum := postings(); n != 2 || sum != 0 {
		t.Fatalf("agent_postings: %d rows summing to %d after finance's fund, want 2 summing to 0", n, sum)
	}

	// Changed to Viewer, Finance's very next fund is refused; and once removed, its session is over.
	if code, _ := call(owner, http.MethodPut, base+"/roles", `{"email":"fin@roles.test","role":"viewer"}`); code != http.StatusOK {
		t.Fatalf("the owner changes finance's role = %d", code)
	}
	if code, _ := call(finance, http.MethodPost, fund, `{"amount_ulxc":1000000}`); code != http.StatusForbidden {
		t.Fatalf("a finance session made viewer funds = %d, want 403", code)
	}
	if code, _ := call(owner, http.MethodDelete, base+"/roles?email=fin@roles.test", ``); code != http.StatusNoContent {
		t.Fatalf("the owner removes the role = %d", code)
	}
	if code, _ := call(finance, http.MethodGet, base+"/agents", ``); code != http.StatusUnauthorized {
		t.Fatalf("a removed role's session reads = %d, want 401", code)
	}
	if n, _ := postings(); n != 2 {
		t.Fatalf("agent_postings holds %d rows, want the 2 of finance's one fund", n)
	}
	code, roles := call(owner, http.MethodGet, base+"/roles", ``)
	if items, _ := roles["items"].([]any); code != http.StatusOK || len(items) != 2 {
		t.Fatalf("the owner's roles = %d %v, want the viewer and the approver", code, roles)
	}
}

// What each role may do beyond reading, and the owner's parts no role reaches.
func TestWorkspaceRoles_WhatEachRoleMayDo(t *testing.T) {
	for _, c := range []struct {
		role, method, path string
		want               bool
	}{
		{"viewer", http.MethodGet, "/v1/workspaces/w/agents", true},
		{"viewer", http.MethodPost, "/v1/workspaces/w/agents/a/fund", false},
		{"finance", http.MethodPost, "/v1/workspaces/w/agents/a/fund", true},
		{"finance", http.MethodPost, "/v1/money/payments", true},
		{"finance", http.MethodPut, "/v1/workspaces/w/agents/a/rules", false},
		{"finance", http.MethodPost, "/v1/workspaces/w/agents/approvals/p/approve", false},
		{"approver", http.MethodPost, "/v1/workspaces/w/agents/approvals/p/approve", true},
		{"approver", http.MethodPost, "/v1/money/payouts/p/approve", true},
		{"approver", http.MethodPost, "/v1/workspaces/w/agents/a/fund", false},
		{"admin", http.MethodPut, "/v1/workspaces/w/agents/a/rules", true},
		{"admin", http.MethodPost, "/v1/workspaces/w/agents/a/fund", true},
		{"admin", http.MethodPost, "/v1/workspaces/w/agents/approvals/p/approve", true},
		{"admin", http.MethodPost, "/v1/workspaces/w/agents/a/freeze", false},
		{"admin", http.MethodPost, "/v1/workspaces/w/verification/identity", false},
		{"admin", http.MethodPost, "/v1/workspaces/w/requests", false},
		{"viewer", http.MethodGet, "/v1/workspaces/w/requests/1/export", false},
		{"viewer", http.MethodGet, "/v1/workspaces/w/verification", false},
		{"finance", http.MethodGet, "/v1/workspaces/w/business-details", false},
		{"admin", http.MethodPost, "/v1/workspaces/w/agents/a/keys", false},
		{"admin", http.MethodPut, "/v1/workspaces/w/roles", false},
		{"admin", http.MethodGet, "/v1/workspaces/w/agents/passkeys", false},
		{"admin", http.MethodPost, "/v1/workspaces/w//agents/a/fund", false},
		{"owner", http.MethodGet, "/v1/workspaces/w/agents", false},
	} {
		if got := auth.RoleAllows(c.role, c.method, c.path); got != c.want {
			t.Errorf("%s %s %s = %v, want %v", c.role, c.method, c.path, got, c.want)
		}
	}
}

// B28.342, v7 review: a role session is never the owner. No role reads the owner's data export or identity
// verification, starts an identity check or files a request — through the real auth middleware, so both gates are
// exercised: Authenticate's (auth.RoleAllows) and the handler's (storedanswers.OwnerOrAdmin refuses every role
// session; a handler admits one only where it names the role). Finance and Approver still do what their lists name.
func TestWorkspaceRoles_NoRoleReachesTheOwnersExportVerificationOrRequests(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-roles-owner", "b28342-provision-secret"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	wsm := workspace.New(pool)
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: ws, Name: "Owner Ltd"}); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	am := auth.NewManager("", key, auth.New(pool), tenant.NewStore(pool)).WithRoles(wsm)
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), am))
		authed.Use(workspaceIsolationMiddleware)
		mountAgentAccountRoutes(authed, bank, tenant.NewStore(pool))
		mountWorkspaceRoleRoutes(authed, wsm)
		mountVerificationRoutes(authed, bank, partners.OneVerifier(&partners.TestKYCProvider{}))
		mountSupportRequestRoutes(authed, support.NewStore(pool))
	})
	mountRoleSessionRoute(r, secret, wsm, func(workspaceID, personKey, role string, ttl time.Duration) (string, error) {
		return auth.GenerateRoleToken(workspaceID, personKey, role, key, ttl)
	})
	call := func(token, method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	owner, err := auth.GenerateToken(ws, ws, provisionScopes, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws
	code, a := call(owner, http.MethodPost, base+"/agents", `{"name":"buyer"}`)
	if code != http.StatusCreated {
		t.Fatalf("the owner's create = %d %v", code, a)
	}
	agent := base + "/agents/" + a["id"].(string)
	code, filed := call(owner, http.MethodPost, base+"/requests", `{"kind":"data_export","message":""}`)
	if code != http.StatusCreated {
		t.Fatalf("the owner asks for the data export = %d %v", code, filed)
	}
	export := fmt.Sprintf("%s/requests/%d/export", base, int64(filed["id"].(float64)))
	if code, out := call(owner, http.MethodGet, export, ``); code != http.StatusConflict {
		t.Fatalf("the owner's export before Talyvor answered = %d %v, want 409 not_ready", code, out)
	}
	if code, out := call(owner, http.MethodGet, base+"/verification", ``); code != http.StatusOK {
		t.Fatalf("the owner reads the verification = %d %v", code, out)
	}

	sessions := map[string]string{}
	for _, role := range workspace.Roles {
		email := role + "@owner.test"
		if code, out := call(owner, http.MethodPut, base+"/roles", `{"email":"`+email+`","role":"`+role+`"}`); code != http.StatusOK {
			t.Fatalf("the owner gives %s %s = %d %v", email, role, code, out)
		}
		code, out := call("", http.MethodPost, "/v1/provision/roles",
			`{"identity":"iss|`+role+`","email":"`+email+`","mode":"session","account_id":"`+ws+`"}`, provisionSecretHeader, secret)
		tok, _ := out["token"].(string)
		if code != http.StatusOK || tok == "" {
			t.Fatalf("%s's session = %d %v", role, code, out)
		}
		sessions[role] = tok
	}
	for role, tok := range sessions {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, export, ``},
			{http.MethodGet, base + "/verification", ``},
			{http.MethodPost, base + "/verification/identity", `{}`},
			{http.MethodPost, base + "/requests", `{"kind":"support","message":"hello"}`},
			{http.MethodPost, base + "/requests", `{"kind":"account_closure","message":""}`},
			{http.MethodGet, agent + "/spend-permission", ``}, // past RoleAllows: the handler's OwnerOrAdmin refuses it
		} {
			if code, out := call(tok, c.method, c.path, c.body); code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d %v, want 403", role, c.method, c.path, code, out)
			}
		}
		if code, out := call(tok, http.MethodGet, agent+"/summary", ``); code != http.StatusOK {
			t.Errorf("%s reads the agent's summary = %d %v, want 200", role, code, out)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_requests WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("workspace_requests holds %d rows, want the owner's one", n)
	}

	// Finance and Admin fund; Approver and Admin reach the decision (404: no such approval, past every gate).
	for _, role := range []string{"finance", "admin"} {
		if code, out := call(sessions[role], http.MethodPost, agent+"/fund", `{"amount_ulxc":1000000}`); code != http.StatusOK {
			t.Errorf("%s funds = %d %v, want 200", role, code, out)
		}
	}
	for _, role := range []string{"approver", "admin"} {
		if code, out := call(sessions[role], http.MethodPost, base+"/agents/approvals/no-such/approve", `{}`); code != http.StatusNotFound {
			t.Errorf("%s decides = %d %v, want 404 past the gates", role, code, out)
		}
	}
	for _, role := range []string{"viewer", "finance"} {
		if code, _ := call(sessions[role], http.MethodPost, base+"/agents/approvals/no-such/approve", `{}`); code != http.StatusForbidden {
			t.Errorf("%s decides = %d, want 403", role, code)
		}
	}
	var postings, sum int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings`).Scan(&postings, &sum); err != nil {
		t.Fatal(err)
	}
	if postings != 4 || sum != 0 {
		t.Fatalf("agent_postings: %d rows summing to %d, want finance's and admin's two funds, 4 rows summing to 0", postings, sum)
	}
}
