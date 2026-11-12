package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/support"
	"github.com/talyvor/lens/internal/tenant"
)

// B59.27 — support and complaints, closing the account, and the workspace's data, on two workspaces: each kind files
// and lists for its own workspace only; a complaint has its reference at once; closing an account that holds money is
// refused naming the balance; the operator's inbox sees both and the operator's answer is audited; the export is
// released only once answered done and holds this workspace's rows alone.
func TestB5927_RequestsFileAndListPerWorkspaceAndTheExportHoldsOnlyItsRows(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000")
	wsA, wsB := "ws-b5927-a-"+stamp, "ws-b5927-b-"+stamp
	const secret = "whsec_b5927"
	for _, ws := range []string{wsA, wsB} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'fiat')`, ws); err != nil {
			t.Fatal(err)
		}
	}
	bank, layer := b596Bank(t, pool, secret)
	requests := support.NewStore(pool)

	var caller *auth.AuthContext
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	r.Group(func(authed chi.Router) {
		authed.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), caller)))
			})
		})
		authed.Use(workspaceIsolationMiddleware)
		mountAgentAccountRoutes(authed, bank, tenant.NewStore(pool))
		mountSupportRequestRoutes(authed, requests)
	})
	owner := func(ws string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
	}
	member := &auth.AuthContext{WorkspaceID: wsA, AuthMethod: auth.MethodJWT, UserID: "member-a"}
	agentKey := &auth.AuthContext{WorkspaceID: wsA, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k-agent", Scopes: []string{"proxy"}}
	call := func(as *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		caller = as
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	file := func(as *auth.AuthContext, ws, kind, message string) (int, map[string]any) {
		t.Helper()
		code, body := call(as, http.MethodPost, "/v1/workspaces/"+ws+"/requests", fmt.Sprintf(`{"kind":%q,"message":%q}`, kind, message))
		out := map[string]any{}
		_ = json.Unmarshal([]byte(body), &out)
		return code, out
	}
	list := func(as *auth.AuthContext, ws string) []support.Request {
		t.Helper()
		code, body := call(as, http.MethodGet, "/v1/workspaces/"+ws+"/requests", "")
		var out struct{ Requests []support.Request }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("list %s = %d %s", ws, code, body)
		}
		return out.Requests
	}

	// Each workspace has an agent, so each has accounts in the ledger; ws-a's agent is paid EUR 25.00.
	agents := map[string]string{}
	for _, ws := range []string{wsA, wsB} {
		code, body := call(owner(ws), http.MethodPost, "/v1/workspaces/"+ws+"/agents", `{"name":"Payroll"}`)
		var out struct{ ID string }
		if code != http.StatusCreated || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("agent in %s = %d %s", ws, code, body)
		}
		agents[ws] = out.ID
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE agent_id = $1 AND currency = 'EUR' AND purpose = 'agent'
		AND status = 'open'`, agents[wsA]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf(`{"id":"evt_b5927_%s","to":%q,"amount":{"amount_minor":2500,"asset":"EUR"},"partner_ref":"test_in_b5927_%s","payer":"Acme"}`,
		stamp, ref, stamp))
	req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
	req.Header.Set(money.SandboxSignatureHeader, money.SignSandbox(secret, body, time.Now()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("deposit = %d %s", w.Code, w.Body.String())
	}
	var held int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(b.balance_minor), 0)::bigint FROM money_account_balances b JOIN money_accounts a ON a.id = b.account_id
		WHERE a.agent_id = $1 AND a.currency = 'EUR'`, agents[wsA]).Scan(&held); err != nil || held != 2500 {
		t.Fatalf("ws-a's agent holds %d EUR in the ledger (%v), want 2500", held, err)
	}

	// Closing ws-a while it holds EUR 25.00 is refused, naming the balance; nothing is filed. ws-b holds nothing and
	// files its closure.
	code, out := file(owner(wsA), wsA, "account_closure", "")
	if bal, _ := out["balances"].(map[string]any); code != http.StatusConflict || out["code"] != "balances_not_empty" ||
		out["field"] != "balances" || bal["EUR"] != float64(2500) {
		t.Fatalf("closing ws-a holding EUR 2500 = %d %v; want 409 balances_not_empty naming EUR 2500", code, out)
	}
	if code, out := file(owner(wsB), wsB, "account_closure", ""); code != http.StatusCreated || out["state"] != "received" {
		t.Fatalf("closing ws-b holding nothing = %d %v; want 201 received", code, out)
	}

	// Every kind files for ws-a, with its state, time and the person who filed it; a complaint has its reference at
	// once, nothing else has one.
	for _, kind := range []string{"support", "complaint", "data_export", "data_deletion"} {
		code, out := file(owner(wsA), wsA, kind, "about "+kind)
		if code != http.StatusCreated || out["kind"] != kind || out["state"] != "received" || out["workspace_id"] != wsA ||
			out["filed_by"] != "user:owner-"+wsA || out["filed_at"] == nil {
			t.Fatalf("ws-a files %s = %d %v", kind, code, out)
		}
		reference, _ := out["reference"].(string)
		if (kind == "complaint") != regexp.MustCompile(`^C-[A-Z2-9]{4}-[A-Z2-9]{4}$`).MatchString(reference) {
			t.Fatalf("ws-a's %s has reference %q; a complaint has one at once, nothing else does", kind, reference)
		}
	}
	if code, out := file(owner(wsA), wsA, "complaint", "  "); code != http.StatusBadRequest {
		t.Fatalf("a complaint saying nothing = %d %v; want 400", code, out)
	}

	// A member signed in to ws-a files support and sees only what they filed; the account and its data are the
	// owner's. No agent key files anything, and nobody files or reads for another workspace.
	if code, out := file(member, wsA, "support", "a card was declined"); code != http.StatusCreated || out["filed_by"] != "user:member-a" {
		t.Fatalf("member files support = %d %v", code, out)
	}
	for _, kind := range []string{"account_closure", "data_export", "data_deletion"} {
		if code, out := file(member, wsA, kind, ""); code != http.StatusForbidden {
			t.Fatalf("member files %s = %d %v; want 403", kind, code, out)
		}
	}
	if got := list(member, wsA); len(got) != 1 || got[0].FiledBy != "user:member-a" {
		t.Fatalf("the member lists %+v; want only their own request", got)
	}
	if code, out := file(agentKey, wsA, "support", "hello"); code != http.StatusForbidden {
		t.Fatalf("an agent key files support = %d %v; want 403", code, out)
	}
	if code, out := file(owner(wsB), wsA, "complaint", "from b"); code < 400 || code == http.StatusCreated {
		t.Fatalf("ws-b's owner files into ws-a = %d %v; want refused", code, out)
	}
	if code, body := call(owner(wsB), http.MethodGet, "/v1/workspaces/"+wsA+"/requests", ""); code < 400 {
		t.Fatalf("ws-b's owner lists ws-a = %d %s; want refused", code, body)
	}
	gotA, gotB := list(owner(wsA), wsA), list(owner(wsB), wsB)
	if len(gotA) != 5 || len(gotB) != 1 || gotB[0].Kind != support.AccountClosure {
		t.Fatalf("ws-a lists %d requests, ws-b %d (%+v); want 5 and ws-b's one closure", len(gotA), len(gotB), gotB)
	}
	for _, rq := range gotA {
		if rq.WorkspaceID != wsA {
			t.Fatalf("ws-a's list holds %+v", rq)
		}
	}

	// Both reach the operator's inbox. A customer's admin key cannot answer; the operator can, and it is audited.
	inbox := func(as *auth.AuthContext, query string) (int, []support.Request) {
		t.Helper()
		admin := chi.NewRouter()
		admin.Get("/v1/admin/requests", requireAdminOrOperatorRead(fixedAuthenticator{as}, newRequestInboxHandler(requests)))
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/requests"+query, nil))
		var out struct{ Requests []support.Request }
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Requests
	}
	answer := func(as *auth.AuthContext, id int64, body string) (int, string) {
		t.Helper()
		admin := chi.NewRouter()
		admin.Put("/v1/admin/requests/{id}", requireAdmin(fixedAuthenticator{as}, newRequestAnswerHandler(requests)))
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/v1/admin/requests/%d", id), strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	operator := &auth.AuthContext{IsAdmin: true, AuthMethod: auth.MethodGlobalKey}
	customerAdmin := &auth.AuthContext{WorkspaceID: wsA, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k-admin", Scopes: []string{auth.ScopeAdmin, auth.ScopeKeys}}
	if code, _ := inbox(customerAdmin, ""); code != http.StatusUnauthorized {
		t.Fatalf("a customer's admin key reads the inbox = %d; want 401", code)
	}
	if code, got := inbox(operator, "?state=received&workspace="+wsA); code != http.StatusOK || len(got) != 5 {
		t.Fatalf("the operator's inbox for ws-a = %d, %d requests; want 5", code, len(got))
	}
	if code, got := inbox(operator, "?workspace="+wsB); code != http.StatusOK || len(got) != 1 {
		t.Fatalf("the operator's inbox for ws-b = %d, %d requests; want 1", code, len(got))
	}
	var export support.Request
	for _, rq := range gotA {
		if rq.Kind == support.DataExport {
			export = rq
		}
	}
	exportPath := fmt.Sprintf("/v1/workspaces/%s/requests/%d/export", wsA, export.ID)
	if code, body := call(owner(wsA), http.MethodGet, exportPath, ""); code != http.StatusConflict || !strings.Contains(body, `"not_ready"`) {
		t.Fatalf("the export before Talyvor answered = %d %s; want 409 not_ready", code, body)
	}
	if code, body := answer(customerAdmin, export.ID, `{"state":"done","actor":"ws-a"}`); code != http.StatusUnauthorized {
		t.Fatalf("a customer's admin key answers = %d %s; want 401", code, body)
	}
	if code, body := answer(operator, export.ID, `{"state":"done","answer":"Your export is ready.","actor":"mihai@talyvor.com"}`); code != http.StatusOK ||
		!strings.Contains(body, `"state":"done"`) {
		t.Fatalf("the operator answers = %d %s", code, body)
	}
	var actor, detail string
	if err := pool.QueryRow(ctx, `SELECT actor, detail FROM operator_audit WHERE action = $1 AND target = $2`, support.AuditAction,
		fmt.Sprintf("%s/%d", wsA, export.ID)).Scan(&actor, &detail); err != nil || actor != "mihai@talyvor.com" || detail != "data_export done" {
		t.Fatalf("the answer in the operator audit trail = %q %q (%v)", actor, detail, err)
	}

	// The export: the owner's alone, and every row in it is ws-a's.
	if code, body := call(member, http.MethodGet, exportPath, ""); code != http.StatusForbidden {
		t.Fatalf("a member downloads the export = %d %s; want 403", code, body)
	}
	if code, _ := call(owner(wsB), http.MethodGet, exportPath, ""); code < 400 {
		t.Fatalf("ws-b's owner downloads ws-a's export = %d; want refused", code)
	}
	code, raw := call(owner(wsA), http.MethodGet, exportPath, "")
	var dump struct {
		WorkspaceID string                      `json:"workspace_id"`
		Tables      map[string][]map[string]any `json:"tables"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &dump) != nil || dump.WorkspaceID != wsA {
		t.Fatalf("ws-a's export = %d %.300s", code, raw)
	}
	if strings.Contains(raw, wsB) {
		t.Fatalf("ws-a's export names ws-b")
	}
	for _, want := range []string{"workspaces", "agent_accounts", "money_accounts", "workspace_requests"} {
		if len(dump.Tables[want]) == 0 {
			t.Fatalf("ws-a's export has no %s rows; tables: %d", want, len(dump.Tables))
		}
	}
	if n := len(dump.Tables["workspace_requests"]); n != 5 {
		t.Fatalf("ws-a's export holds %d requests; want its 5", n)
	}
	for table, rows := range dump.Tables {
		for _, row := range rows {
			mine := false
			for col, v := range row {
				if strings.Contains(col, "hash") || strings.Contains(col, "secret") {
					t.Fatalf("%s's export row carries %s", table, col)
				}
				if (strings.Contains(col, "workspace") || (col == "id" && (table == "workspaces" || table == "workspace_configs"))) && v == wsA {
					mine = true
				}
			}
			if !mine {
				t.Fatalf("ws-a's export holds a %s row that is not ws-a's: %v", table, row)
			}
		}
	}
}
