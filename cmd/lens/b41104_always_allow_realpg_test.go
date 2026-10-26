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
)

// B41.104 — "Approve and always allow this", through the routes the approval card uses: the simulator previews the
// rule before it is saved, approving with always_allow saves it, and the next payment to that payee within it is not
// held for approval — a pay posting, no approval filed. Past the allowance a payment waits for approval again, another
// payee's never stopped, and the payee's monthly cap refuses what would pass it.
func TestB41104_ApprovingWithAlwaysAllowLetsTheNextPaymentToThatPayeeThroughWithoutApproval(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-always-allow"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
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
	payer, supplier, other := newAgent("payer"), newAgent("supplier"), newAgent("other")
	if _, err := store.FundAgent(ctx, ws, payer, 15_000_000); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/"
	// pay answers the status and how many pay entries now move LXC from the payer to `to`.
	pay := func(to string, amount int64) (int, int) {
		t.Helper()
		code, body := call(http.MethodPost, base+payer+"/pay", `{"to_agent_id":"`+to+`","amount_ulxc":`+strconv.FormatInt(amount, 10)+`}`)
		if code != http.StatusOK && code != http.StatusForbidden {
			t.Fatalf("pay %s = %d %s", to, code, body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings p JOIN agent_postings q ON q.entry_id = p.entry_id
			WHERE p.workspace_id = $1 AND p.kind = 'pay' AND p.account = $2 AND p.amount_ulxc < 0
			  AND q.kind = 'pay' AND q.account = $3 AND q.amount_ulxc > 0`, ws, "agent:"+payer, "agent:"+to).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return code, n
	}
	pending := func() []economy.AgentApproval {
		t.Helper()
		_, body := call(http.MethodGet, base+"approvals", "")
		var out struct{ Approvals []economy.AgentApproval }
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(out.Approvals, func(a economy.AgentApproval) bool { return a.Status != "pending" })
	}
	simulate := func(body string) string {
		t.Helper()
		code, got := call(http.MethodPost, base+payer+"/rules/simulate", body)
		if code != http.StatusOK {
			t.Fatalf("simulate %s = %d %s", body, code, got)
		}
		var sim economy.RuleSimulation
		if err := json.Unmarshal([]byte(got), &sim); err != nil {
			t.Fatal(err)
		}
		return sim.Verdict
	}

	if code, body := call(http.MethodPut, base+payer+"/rules", `{"approval_above_ulxc":2000000}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	if code, n := pay(supplier, 3_000_000); code != http.StatusForbidden || n != 0 {
		t.Fatalf("a payment above the approval amount = %d with %d pay postings, want 403 and none", code, n)
	}
	held := pending()
	if len(held) != 1 || held[0].Payee == nil || held[0].Payee.ID != supplier {
		t.Fatalf("pending approvals = %+v, want one for the supplier", held)
	}

	// The preview: the next 4 LXC to the supplier waits for approval today, and would not with the rule.
	payment := `"amount_ulxc":4000000,"payee":{"kind":"agent","id":"` + supplier + `"}`
	if v := simulate(`{` + payment + `}`); v != "approval_required" {
		t.Errorf("simulated next payment without the rule = %s, want approval_required", v)
	}
	if v := simulate(`{` + payment + `,"always_allow":{"amount_ulxc":10000000,"per":"month"}}`); v != "allowed" {
		t.Errorf("simulated next payment with always_allow = %s, want allowed", v)
	}
	if _, got := call(http.MethodGet, base+payer+"/rules", ""); !strings.Contains(got, `"payee_allowances":{}`) {
		t.Errorf("after the preview, rules = %s, want no allowance saved", got)
	}

	for _, bad := range []string{`{"always_allow":{"amount_ulxc":0,"per":"month"}}`, `{"always_allow":{"amount_ulxc":5,"per":"week"}}`} {
		if code, body := call(http.MethodPost, base+"approvals/"+held[0].ID+"/approve", bad); code != http.StatusBadRequest {
			t.Errorf("approve %s = %d %s, want 400", bad, code, body)
		}
	}
	code, body := call(http.MethodPost, base+"approvals/"+held[0].ID+"/approve", `{"always_allow":{"amount_ulxc":10000000,"per":"month"}}`)
	want := `"payee_allowances":{"` + supplier + `":{"amount_ulxc":10000000,"per":"month"}}`
	if code != http.StatusOK || !strings.Contains(body, `"status":"approved"`) || !strings.Contains(body, want) {
		t.Fatalf("approve with always_allow = %d %s, want approved and %s", code, body, want)
	}

	if code, n := pay(supplier, 3_000_000); code != http.StatusOK || n != 1 {
		t.Fatalf("the approved payment's retry = %d with %d pay postings, want 200 and one", code, n)
	}
	// The DONE line: the next payment to that payee within the allowance is not held — 3 + 4 of 10 LXC this month.
	if code, n := pay(supplier, 4_000_000); code != http.StatusOK || n != 2 || len(pending()) != 0 {
		t.Fatalf("the next payment within the allowance = %d with %d pay postings and %d pending approvals, want 200, two and none",
			code, n, len(pending()))
	}
	if code, n := pay(supplier, 4_000_000); code != http.StatusForbidden || n != 2 || len(pending()) != 1 {
		t.Errorf("a payment past the allowance = %d with %d pay postings and %d pending approvals, want 403, still two, and one",
			code, n, len(pending()))
	}
	if code, n := pay(other, 3_000_000); code != http.StatusForbidden || n != 0 {
		t.Errorf("a payment to another payee above the approval amount = %d with %d pay postings, want 403 and none", code, n)
	}

	// Rules saved without them keep the allowances; the payee's monthly cap counts the month's 7 LXC.
	if code, body := call(http.MethodPut, base+payer+"/rules", `{"approval_above_ulxc":2000000,"payee_monthly_limits_ulxc":{"`+supplier+`":8000000}}`); code != http.StatusOK || !strings.Contains(body, want) {
		t.Fatalf("PUT rules without allowances = %d %s, want %s kept", code, body, want)
	}
	if code, n := pay(supplier, 1_000_000); code != http.StatusOK || n != 3 {
		t.Errorf("a payment that reaches the monthly cap exactly = %d with %d pay postings, want 200 and three", code, n)
	}
	code, n := pay(supplier, 1_000_000)
	_, refusal := call(http.MethodPost, base+payer+"/rules/simulate", `{"amount_ulxc":1000000,"payee":{"kind":"agent","id":"`+supplier+`"}}`)
	if code != http.StatusForbidden || n != 3 || !strings.Contains(refusal, "monthly limit") {
		t.Errorf("a payment past the monthly cap = %d with %d pay postings (%s), want 403, still three, the monthly limit named", code, n, refusal)
	}
}
