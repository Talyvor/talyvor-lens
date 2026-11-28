package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B17.584 — the testers sign a role person in to a test workspace on the synthetic key, never the provisioning
// secret: a synthetic workspace's Viewer gets a session that reads but cannot fund; a real workspace, an email with no
// role, another role and a wrong key are refused; POST /v1/provision/roles still signs a real workspace's role in.
func TestSyntheticRoleSession_AViewerReadsButCannotFundAndOnlyATestWorkspaceIsServed(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const testWS, realWS = "s-b17584-roles", "ws-b17584-real"
	const syntheticKey = "b17584-synthetic-key"
	const secret = "b17584-provision-secret"
	wsm := workspace.New(pool)
	if err := wsm.CreateSynthetic(ctx, testWS, "Test roles", "business"); err != nil {
		t.Fatal(err)
	}
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: realWS, Name: "Real Ltd"}); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{testWS, realWS} {
		if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)
			ON CONFLICT (workspace_id) DO NOTHING`, ws); err != nil {
			t.Fatal(err)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(workspaceID, personKey, role string, ttl time.Duration) (string, error) {
		return auth.GenerateRoleToken(workspaceID, personKey, role, key, ttl)
	}
	am := auth.NewManager("", key, auth.New(pool), tenant.NewStore(pool)).WithRoles(wsm)
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), am))
		authed.Use(workspaceIsolationMiddleware)
		mountAgentAccountRoutes(authed, economy.NewDualTokenStore(nil, pool, nil), tenant.NewStore(pool))
		mountWorkspaceRoleRoutes(authed, wsm)
	})
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: wsm, audit: pool, plans: pool, roles: wsm, roleMint: mint})
	mountRoleSessionRoute(r, secret, wsm, mint)

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
	roleSession := func(ws, body, synthetic string) (int, map[string]any) {
		t.Helper()
		return call("", http.MethodPost, "/v1/synthetic/workspaces/"+ws+"/role-session", body, syntheticKeyHeader, synthetic)
	}
	postings := func() (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1`, testWS).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	agentOf := map[string]string{}
	for _, ws := range []string{testWS, realWS} {
		owner, err := auth.GenerateToken(ws, ws, provisionScopes, key, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		code, a := call(owner, http.MethodPost, "/v1/workspaces/"+ws+"/agents", `{"name":"buyer"}`)
		if code != http.StatusCreated {
			t.Fatalf("the owner of %s creates an agent = %d %v", ws, code, a)
		}
		agentOf[ws] = a["id"].(string)
		if code, out := call(owner, http.MethodPut, "/v1/workspaces/"+ws+"/roles", `{"email":"vera@roles.test","role":"viewer"}`); code != http.StatusOK {
			t.Fatalf("the owner of %s gives vera viewer = %d %v", ws, code, out)
		}
	}

	// The test workspace's Viewer signs in on the synthetic key, reads the wallet, and its fund writes no posting.
	code, out := roleSession(testWS, `{"email":"Vera@roles.test","role":"viewer","ttl_hours":100000}`, syntheticKey)
	viewer, _ := out["token"].(string)
	if code != http.StatusOK || viewer == "" || out["role"] != "viewer" || out["workspace_id"] != testWS {
		t.Fatalf("the test viewer's synthetic role session = %d %v, want 200 with a viewer token", code, out)
	}
	if exp, err := time.Parse(time.RFC3339, out["expires_at"].(string)); err != nil || time.Until(exp) > auth.MaxTokenTTL+time.Minute {
		t.Fatalf("expires_at = %v (%v), want the TTL clamped to %s", out["expires_at"], err, auth.MaxTokenTTL)
	}
	if code, book := call(viewer, http.MethodGet, "/v1/workspaces/"+testWS+"/agents", ``); code != http.StatusOK || len(book["agents"].([]any)) != 1 {
		t.Fatalf("the viewer reads the agents = %d %v", code, book)
	}
	if code, _ := call(viewer, http.MethodPost, "/v1/workspaces/"+testWS+"/agents/"+agentOf[testWS]+"/fund", `{"amount_ulxc":1000000}`); code != http.StatusForbidden {
		t.Fatalf("the viewer funds = %d, want 403", code)
	}
	if n := postings(); n != 0 {
		t.Fatalf("agent_postings holds %d rows after the viewer's fund, want 0", n)
	}

	// Refused: a real workspace, an email with no role, a role the email was not given, and a wrong or missing key.
	if code, out := roleSession(realWS, `{"email":"vera@roles.test","role":"viewer"}`, syntheticKey); code != http.StatusNotFound || out["token"] != nil {
		t.Fatalf("a real workspace's viewer on the synthetic key = %d %v, want 404 and no token", code, out)
	}
	if code, out := roleSession(testWS, `{"email":"nobody@roles.test","role":"viewer"}`, syntheticKey); code != http.StatusNotFound || out["token"] != nil {
		t.Fatalf("an email with no role = %d %v, want 404 and no token", code, out)
	}
	if code, out := roleSession(testWS, `{"email":"vera@roles.test","role":"admin"}`, syntheticKey); code != http.StatusForbidden || out["token"] != nil {
		t.Fatalf("the viewer asking for admin = %d %v, want 403 and no token", code, out)
	}
	for _, k := range []string{"wrong-key", ""} {
		if code, out := roleSession(testWS, `{"email":"vera@roles.test","role":"viewer"}`, k); code != http.StatusUnauthorized || out["token"] != nil {
			t.Fatalf("synthetic key %q = %d %v, want 401 and no token", k, code, out)
		}
	}

	// The real route is unchanged: the real workspace's viewer signs in on the provisioning secret.
	code, out = call("", http.MethodPost, "/v1/provision/roles",
		`{"identity":"iss|vera","email":"vera@roles.test","mode":"session","account_id":"`+realWS+`"}`, provisionSecretHeader, secret)
	real, _ := out["token"].(string)
	if code != http.StatusOK || real == "" || out["role"] != "viewer" {
		t.Fatalf("the real viewer's /v1/provision/roles session = %d %v", code, out)
	}
	if code, _ := call(real, http.MethodGet, "/v1/workspaces/"+realWS+"/agents", ``); code != http.StatusOK {
		t.Fatalf("the real viewer reads the agents = %d", code)
	}
}
