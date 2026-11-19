package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.315 — above a second, higher amount a Business's payment needs two approvers, through the routes the approval
// sheet and Team and roles use. Under the line one approval pays, as before. Over it the owner's approval leaves the
// payment waiting and the retry writes no posting; the owner approving again is refused; the second approver's approval
// approves it and the retry writes exactly one. Two approvals neither of which is the second approver's do not pay. A
// Personal account has no second approver.
func TestB28315_OverTheSecondApproverLineAPaymentNeedsTwoApprovers(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, personal = "ws-b28315-business", "ws-b28315-personal"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type) VALUES ($1, $1, $1, 'business'), ($2, $2, $2, 'personal')`,
		ws, personal); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 40000000, 40000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountSecondApproverRoutes(r, store)
	person := func(wsID, userID, scope string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: wsID, AuthMethod: auth.MethodJWT, UserID: userID, Scopes: []string{scope}}
	}
	owner, finance, auditor := person(ws, "owner", auth.ScopeKeys), person(ws, "finance", auth.ScopeAdmin), person(ws, "auditor", auth.ScopeAdmin)
	call := func(as *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), as))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	newAgent := func(name string) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	payer, supplier := newAgent("payer"), newAgent("supplier")
	if _, err := store.FundAgent(ctx, ws, payer, 40_000_000); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/"
	// pay answers the status and how many pay entries have moved LXC from the payer to the supplier so far.
	pay := func(amount int64, memo string) (int, int) {
		t.Helper()
		code, body := call(owner, http.MethodPost, base+payer+"/pay", `{"to_agent_id":"`+supplier+`","amount_ulxc":`+strconv.FormatInt(amount, 10)+`,"memo":"`+memo+`"}`)
		if code != http.StatusOK && code != http.StatusAccepted {
			t.Fatalf("pay %s = %d %s", memo, code, body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings p JOIN agent_postings q ON q.entry_id = p.entry_id
			WHERE p.workspace_id = $1 AND p.kind = 'pay' AND p.account = $2 AND p.amount_ulxc < 0
			  AND q.kind = 'pay' AND q.account = $3 AND q.amount_ulxc > 0`, ws, "agent:"+payer, "agent:"+supplier).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return code, n
	}
	pendingID := func(memo string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND memo = $2 AND status = 'pending'`, payer, memo).Scan(&id); err != nil {
			t.Fatalf("the pending approval of %s: %v", memo, err)
		}
		return id
	}
	approve := func(as *auth.AuthContext, id string) (int, economy.AgentApproval, string) {
		t.Helper()
		code, body := call(as, http.MethodPost, base+"approvals/"+id+"/approve", `{}`)
		var a economy.AgentApproval
		_ = json.Unmarshal([]byte(body), &a)
		return code, a, body
	}

	if code, body := call(owner, http.MethodPut, base+payer+"/rules", `{"approval_above_ulxc":2000000}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	// Only the owner sets the line; above $0.50 a payment needs finance too. 1 LXC is $0.10.
	line := `{"user_id":"finance","above_minor":50,"currency":"USD"}`
	if code, body := call(finance, http.MethodPut, "/v1/workspaces/"+ws+"/second-approver", line); code != http.StatusForbidden {
		t.Errorf("PUT second-approver as an admin = %d %s, want 403", code, body)
	}
	if code, body := call(owner, http.MethodPut, "/v1/workspaces/"+ws+"/second-approver", line); code != http.StatusOK {
		t.Fatalf("PUT second-approver = %d %s", code, body)
	}
	if code, body := call(finance, http.MethodGet, "/v1/workspaces/"+ws+"/second-approver", ""); code != http.StatusOK ||
		!strings.Contains(body, `"user_id":"finance","above_minor":50,"currency":"USD"`) {
		t.Errorf("GET second-approver = %d %s, want finance above 50 USD", code, body)
	}
	if code, body := call(person(personal, "solo", auth.ScopeKeys), http.MethodPut, "/v1/workspaces/"+personal+"/second-approver", line); code != http.StatusNotFound ||
		!strings.Contains(body, `"code":"not_business"`) {
		t.Errorf("PUT second-approver on a Personal account = %d %s, want 404 not_business", code, body)
	}

	// Under the line ($0.30): one approval pays.
	if code, n := pay(3_000_000, "under"); code != http.StatusAccepted || n != 0 {
		t.Fatalf("a payment above the approval amount = %d with %d pay postings, want 202 and none", code, n)
	}
	if code, a, body := approve(owner, pendingID("under")); code != http.StatusOK || a.Status != "approved" {
		t.Fatalf("the owner approving under the line = %d %s, want 200 approved", code, body)
	}
	if code, n := pay(3_000_000, "under"); code != http.StatusOK || n != 1 {
		t.Fatalf("the retry under the line = %d with %d pay postings, want 200 and one", code, n)
	}

	// Over the line ($1.00): zero postings after the first approval, one after the second.
	if code, n := pay(10_000_000, "over"); code != http.StatusAccepted || n != 1 {
		t.Fatalf("a payment over the line = %d with %d pay postings, want 202 and still one", code, n)
	}
	over := pendingID("over")
	code, a, body := approve(owner, over)
	if code != http.StatusOK || a.Status != "pending" || !a.WaitingForSecondApprover || a.FirstApprovedBy != "user:owner" {
		t.Fatalf("the owner's approval over the line = %d %s, want 200, pending, waiting for a second approver", code, body)
	}
	if code, n := pay(10_000_000, "over"); code != http.StatusAccepted || n != 1 {
		t.Fatalf("the retry after the first approval = %d with %d pay postings, want 202 and still one", code, n)
	}
	if code, _, body := approve(owner, over); code != http.StatusConflict || !strings.Contains(body, `"code":"same_approver"`) {
		t.Errorf("the owner approving twice = %d %s, want 409 same_approver", code, body)
	}
	if code, a, body := approve(finance, over); code != http.StatusOK || a.Status != "approved" || a.SecondApprovedBy != "user:finance" {
		t.Fatalf("the second approver's approval = %d %s, want 200 approved by finance", code, body)
	}
	if code, n := pay(10_000_000, "over"); code != http.StatusOK || n != 2 {
		t.Fatalf("the retry after the second approval = %d with %d pay postings, want 200 and one more", code, n)
	}
	if code, n := pay(10_000_000, "over"); code != http.StatusAccepted || n != 2 {
		t.Errorf("paying it again = %d with %d pay postings, want a fresh approval and no more", code, n)
	}

	// Two approvals, neither of them the second approver's: the payment still waits.
	if code, n := pay(10_000_000, "pair"); code != http.StatusAccepted || n != 2 {
		t.Fatalf("another payment over the line = %d with %d pay postings", code, n)
	}
	pair := pendingID("pair")
	if code, _, body := approve(owner, pair); code != http.StatusOK {
		t.Fatalf("the owner's approval = %d %s", code, body)
	}
	if code, _, body := approve(auditor, pair); code != http.StatusForbidden || !strings.Contains(body, `"code":"second_approver_needed"`) {
		t.Errorf("an admin who is not the second approver approving second = %d %s, want 403 second_approver_needed", code, body)
	}
	if code, n := pay(10_000_000, "pair"); code != http.StatusAccepted || n != 2 {
		t.Errorf("the retry = %d with %d pay postings, want 202 and no more", code, n)
	}

	// An API key is never one of the two, admin scope or not.
	key := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-1", Scopes: []string{auth.ScopeAdmin}}
	if code, _, body := approve(key, pair); code != http.StatusForbidden || !strings.Contains(body, `"code":"approver_unknown"`) {
		t.Errorf("a workspace key approving second = %d %s, want 403 approver_unknown", code, body)
	}

	// The line asks in itself: an agent whose own rules ask nothing still waits over it, and says why.
	if code, body := call(owner, http.MethodPut, base+payer+"/rules", `{"approval_above_ulxc":0}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	if code, n := pay(3_000_000, "no ask, under"); code != http.StatusOK || n != 3 {
		t.Fatalf("under the line with no approval amount = %d with %d pay postings, want 200 and one more", code, n)
	}
	if code, n := pay(10_000_000, "no ask, over"); code != http.StatusAccepted || n != 3 {
		t.Fatalf("over the line with no approval amount = %d with %d pay postings, want 202 and no more", code, n)
	}
	code, a, body = approve(owner, pendingID("no ask, over"))
	if code != http.StatusOK || !a.OverSecondApproverLine || !a.WaitingForSecondApprover {
		t.Errorf("the owner's approval of it = %d %s, want over the second approver line and waiting", code, body)
	}
	b501ZeroSum(t, pool)
}
