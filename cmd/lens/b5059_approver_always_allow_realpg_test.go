package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B50.59 — "always allow" makes a standing rule for the agent, and an agent's rules are the owner's and Admins' to
// change. An Approver may approve or decline a held payment, but approving with always_allow (or asking for its
// challenge) is refused 403 before the decision: the approval stays pending, the agent's rules gain no allowance, and
// the next payment to that payee is held again. An Admin's always_allow still makes the rule.
func TestB5059_AnApproversAlwaysAllowIsRefusedAndMakesNoRule(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5059"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	as := func(role string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: role + "-person", Role: role}
	}
	approver, admin := as(workspace.RoleApprover), as(workspace.RoleAdmin)
	call := func(who *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	payer, err := store.CreateAgent(ctx, ws, "payer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := store.CreateAgent(ctx, ws, "supplier", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, payer.ID, 15_000_000); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/"
	// pay answers the status and how many pay entries now move LXC from the payer to the supplier.
	pay := func(amount int64) (int, int) {
		t.Helper()
		code, body := call(owner, http.MethodPost, base+payer.ID+"/pay", `{"to_agent_id":"`+supplier.ID+`","amount_ulxc":`+strconv.FormatInt(amount, 10)+`}`)
		if code != http.StatusOK && code != http.StatusAccepted {
			t.Fatalf("pay = %d %s", code, body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings p JOIN agent_postings q ON q.entry_id = p.entry_id
			WHERE p.workspace_id = $1 AND p.kind = 'pay' AND p.account = $2 AND p.amount_ulxc < 0
			  AND q.kind = 'pay' AND q.account = $3 AND q.amount_ulxc > 0`, ws, "agent:"+payer.ID, "agent:"+supplier.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return code, n
	}
	pending := func() []economy.AgentApproval {
		t.Helper()
		_, body := call(owner, http.MethodGet, base+"approvals", "")
		var out struct{ Approvals []economy.AgentApproval }
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(out.Approvals, func(a economy.AgentApproval) bool { return a.Status != "pending" })
	}
	// allowances reads the agent's rules row as saved, not the handler's answer.
	allowances := func() string {
		t.Helper()
		rules, err := store.GetAgentRules(ctx, ws, payer.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(rules.PayeeAllowances)
		return string(b)
	}

	if code, body := call(owner, http.MethodPut, base+payer.ID+"/rules", `{"approval_above_ulxc":2000000}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	if code, n := pay(3_000_000); code != http.StatusAccepted || n != 0 {
		t.Fatalf("a payment above the approval amount = %d with %d pay postings, want 202 and none", code, n)
	}
	held := pending()
	if len(held) != 1 {
		t.Fatalf("pending approvals = %+v, want one", held)
	}
	allow := `{"always_allow":{"amount_ulxc":10000000,"per":"day"}}`

	if code, body := call(approver, http.MethodPost, base+"approvals/"+held[0].ID+"/challenge", allow); code != http.StatusForbidden {
		t.Errorf("an approver's challenge with always_allow = %d %s, want 403", code, body)
	}
	if code, body := call(approver, http.MethodPost, base+"approvals/"+held[0].ID+"/approve", allow); code != http.StatusForbidden {
		t.Errorf("an approver's approve with always_allow = %d %s, want 403", code, body)
	}
	if got := allowances(); got != "{}" && got != "null" {
		t.Fatalf("after an approver's always_allow, the agent's payee allowances = %s, want none", got)
	}
	if len(pending()) != 1 {
		t.Fatalf("after an approver's refused always_allow, %d approvals pending, want the one still pending", len(pending()))
	}

	// The approver may still approve the payment once, which makes no rule.
	if code, body := call(approver, http.MethodPost, base+"approvals/"+held[0].ID+"/approve", `{}`); code != http.StatusOK || !strings.Contains(body, `"status":"approved"`) {
		t.Fatalf("an approver's plain approve = %d %s, want 200 approved", code, body)
	}
	if code, n := pay(3_000_000); code != http.StatusOK || n != 1 {
		t.Fatalf("the approved payment's retry = %d with %d pay postings, want 200 and one", code, n)
	}
	if code, n := pay(3_000_000); code != http.StatusAccepted || n != 1 {
		t.Fatalf("the next payment to that payee = %d with %d pay postings, want 202 (held) and still one", code, n)
	}

	// An Admin's always_allow is a rules change it may make.
	held = pending()
	if len(held) != 1 {
		t.Fatalf("pending approvals = %+v, want one", held)
	}
	if code, body := call(admin, http.MethodPost, base+"approvals/"+held[0].ID+"/approve", allow); code != http.StatusOK || !strings.Contains(body, `"payee_allowances":{"`+supplier.ID+`"`) {
		t.Fatalf("an admin's approve with always_allow = %d %s, want 200 with the allowance", code, body)
	}
	if got := allowances(); !strings.Contains(got, supplier.ID) {
		t.Fatalf("after an admin's always_allow, the agent's payee allowances = %s, want the supplier's", got)
	}
}
