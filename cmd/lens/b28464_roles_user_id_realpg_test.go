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

// B28.464 — the owner's roles list names who took each role up (user_id, the person key of their role session; "" until
// someone has), so Team and roles can pick the second approver. A second-approver line set to the listed user_id is
// satisfied by that person's role session approving: zero pay postings after the owner's approval, one after theirs.
func TestB28464_TheRolesListNamesWhoTookARoleUpAndThatPersonIsTheSecondApprover(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b28464", "b28464-provision-secret"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 40000000, 40000000)`, ws); err != nil {
		t.Fatal(err)
	}
	wsm := workspace.New(pool)
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: ws, Name: "Two Approvers Ltd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET account_type = 'business' WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	am := auth.NewManager("", key, auth.New(pool), tenant.NewStore(pool)).WithRoles(wsm)
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), am))
		authed.Use(workspaceIsolationMiddleware)
		mountAgentAccountRoutes(authed, store, tenant.NewStore(pool))
		mountSecondApproverRoutes(authed, store)
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
	owner, err := auth.GenerateToken(ws, ws, provisionScopes, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws

	for email, role := range map[string]string{"ada@two.test": "approver", "fin@two.test": "finance"} {
		if code, out := call(owner, http.MethodPut, base+"/roles", `{"email":"`+email+`","role":"`+role+`"}`); code != http.StatusOK {
			t.Fatalf("the owner gives %s %s = %d %v", email, role, code, out)
		}
	}
	code, out := call("", http.MethodPost, "/v1/provision/roles",
		`{"identity":"iss|ada","email":"ada@two.test","mode":"session","account_id":"`+ws+`"}`, provisionSecretHeader, secret)
	ada, _ := out["token"].(string)
	if code != http.StatusOK || ada == "" {
		t.Fatalf("ada's session = %d %v", code, out)
	}
	adaReq := httptest.NewRequest(http.MethodGet, base+"/agents", nil)
	adaReq.Header.Set("Authorization", "Bearer "+ada)
	adaCtx, err := am.Authenticate(adaReq)
	if err != nil || adaCtx.UserID == "" {
		t.Fatalf("ada's role token = %+v %v, want a UserID", adaCtx, err)
	}

	// The owner's list: ada's user_id is her role token's UserID; fin, who has not taken the role up, lists "".
	code, roles := call(owner, http.MethodGet, base+"/roles", ``)
	userIDs := map[string]any{}
	items, _ := roles["items"].([]any)
	for _, it := range items {
		row := it.(map[string]any)
		userIDs[row["email"].(string)] = row["user_id"]
	}
	if code != http.StatusOK || len(items) != 2 {
		t.Fatalf("the owner's roles = %d %v, want ada and fin", code, roles)
	}
	if userIDs["ada@two.test"] != adaCtx.UserID {
		t.Errorf("ada's user_id = %v, want her role token's UserID %q", userIDs["ada@two.test"], adaCtx.UserID)
	}
	if userIDs["fin@two.test"] != "" {
		t.Errorf("fin's user_id = %v, want \"\" (not taken up)", userIDs["fin@two.test"])
	}
	listed, _ := userIDs["ada@two.test"].(string)

	// The line is set from the listed user_id; over it the owner's approval pays nothing and ada's pays once.
	code, a := call(owner, http.MethodPost, base+"/agents", `{"name":"payer"}`)
	if code != http.StatusCreated {
		t.Fatalf("create payer = %d %v", code, a)
	}
	payer := a["id"].(string)
	code, a = call(owner, http.MethodPost, base+"/agents", `{"name":"supplier"}`)
	if code != http.StatusCreated {
		t.Fatalf("create supplier = %d %v", code, a)
	}
	supplier := a["id"].(string)
	agents := base + "/agents/"
	if code, out := call(owner, http.MethodPost, agents+payer+"/fund", `{"amount_ulxc":20000000}`); code != http.StatusOK {
		t.Fatalf("fund = %d %v", code, out)
	}
	if code, out := call(owner, http.MethodPut, base+"/second-approver", `{"user_id":"`+listed+`","above_minor":50,"currency":"USD"}`); code != http.StatusOK {
		t.Fatalf("PUT second-approver to the listed user_id = %d %v", code, out)
	}
	pay := func() (int, int) {
		t.Helper()
		code, out := call(owner, http.MethodPost, agents+payer+"/pay", `{"to_agent_id":"`+supplier+`","amount_ulxc":10000000,"memo":"over"}`)
		if code != http.StatusOK && code != http.StatusAccepted {
			t.Fatalf("pay = %d %v", code, out)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings p JOIN agent_postings q ON q.entry_id = p.entry_id
			WHERE p.workspace_id = $1 AND p.kind = 'pay' AND p.account = $2 AND p.amount_ulxc < 0
			  AND q.kind = 'pay' AND q.account = $3 AND q.amount_ulxc > 0`, ws, "agent:"+payer, "agent:"+supplier).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return code, n
	}
	if code, n := pay(); code != http.StatusAccepted || n != 0 {
		t.Fatalf("a payment over the line = %d with %d pay postings, want 202 and none", code, n)
	}
	var pending string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending'`, payer).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if code, out := call(owner, http.MethodPost, agents+"approvals/"+pending+"/approve", `{}`); code != http.StatusOK || out["status"] != "pending" {
		t.Fatalf("the owner's approval = %d %v, want 200 and still pending", code, out)
	}
	if code, n := pay(); code != http.StatusAccepted || n != 0 {
		t.Fatalf("the retry after the owner's approval = %d with %d pay postings, want 202 and none", code, n)
	}
	if code, out := call(ada, http.MethodPost, agents+"approvals/"+pending+"/approve", `{}`); code != http.StatusOK || out["status"] != "approved" {
		t.Fatalf("ada's approval as the second approver = %d %v, want 200 approved", code, out)
	}
	if code, n := pay(); code != http.StatusOK || n != 1 {
		t.Fatalf("the retry after ada's approval = %d with %d pay postings, want 200 and one", code, n)
	}
	b501ZeroSum(t, pool)
}
