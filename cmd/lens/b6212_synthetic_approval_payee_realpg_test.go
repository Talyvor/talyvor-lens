package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B62.12, red first — the testers change an approved payment's payee on a test workspace, so the nightly reaches B62.1's
// stop: an agent's €150.00 to Ada, approved, is re-pointed on the synthetic key at the workspace's other payee, and the
// agent's same payment to that payee is stopped 403 with nothing moved, the approval still approved and stopped because
// the payee changed. Without the key, on a real workspace, on a pending approval or at a payee of another workspace it
// is refused and the approval is unchanged. On main the route is not there.
func TestB6212_TheTestersRepointAnApprovedPaymentAndItIsStopped(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, realWS = "s-b6212-payee", "ws-b6212-real"
	const syntheticKey = "b6212-synthetic-key"
	wsm := workspace.New(pool)
	if err := wsm.CreateSynthetic(ctx, ws, "Test payee", "business"); err != nil {
		t.Fatal(err)
	}
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: realWS, Name: "Real Ltd"}); err != nil {
		t.Fatal(err)
	}
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	agent, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "EUR", "test")
	if err != nil {
		t.Fatal(err)
	}
	acct, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "EUR", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee := func(in, name, iban string) string {
		t.Helper()
		p, err := store.CreateOutsidePayee(ctx, in, economy.OutsidePayee{Name: name, Country: "DE", IBAN: iban, BIC: "COBADEFFXXX"})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	approvedTo, changedTo := payee(ws, "Ada Lovelace", "DE89370400440532013000"), payee(ws, "Grace Hopper", "DE02120300000000202051")
	elsewhere := payee(realWS, "Real Payee", "DE89370400440532013000")
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000_000}); err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'EUR'`, ws).
		Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -50_000}, {AccountID: acct.ID, AmountMinor: 50_000}}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: wsm, audit: pool, due: syntheticDueDeps{db: pool, approvals: store}})
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		if !strings.HasPrefix(path, "/v1/synthetic/") {
			req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// The agent's €150.00, under one key and one reference, as the nightly sends it to either payee.
	pay := func(to string) (int, map[string]any) {
		return call(http.MethodPost, "/v1/money/payments", `{"payee_id":"`+to+`","account_id":"`+acct.ID+
			`","currency":"EUR","amount_minor":15000,"reference":"Invoice 6212","idempotency_key":"k-6212"}`)
	}
	repoint := func(in, approval, to, key string) (int, map[string]any) {
		return call(http.MethodPost, "/v1/synthetic/workspaces/"+in+"/agents/approvals/"+approval+"/payee", `{"payee_id":"`+to+`"}`,
			syntheticKeyHeader, key)
	}
	nothingMoved := func(when string) {
		t.Helper()
		for _, a := range []struct{ id, name string }{{acct.ID, "the agent's account"}, {company.ID, "the company's account"}} {
			want := int64(0)
			if a.id == acct.ID {
				want = 50_000
			}
			if b, err := store.Balance(ctx, ws, a.id); err != nil || b.TestMinor != want {
				t.Fatalf("%s: %s holds %+v (%v), want %d", when, a.name, b, err, want)
			}
		}
		var payments int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1`, ws).Scan(&payments); err != nil || payments != 0 {
			t.Fatalf("%s: %d payments out (%v), want none", when, payments, err)
		}
	}
	fingerprint := func(approval string) string {
		t.Helper()
		var fp string
		if err := pool.QueryRow(ctx, `SELECT fingerprint FROM agent_approvals WHERE id = $1`, approval).Scan(&fp); err != nil {
			t.Fatal(err)
		}
		return fp
	}

	if code, out := pay(approvedTo); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "Approval apr_") {
		t.Fatalf("pay €150.00 to Ada = %d %v, want 403 naming the approval", code, out)
	}
	var approval string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending'`, agent.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if code, _ := repoint(ws, approval, changedTo, syntheticKey); code != http.StatusConflict {
		t.Fatalf("re-point a pending approval = %d, want 409", code)
	}
	if _, err := store.DecideAgentApproval(ctx, ws, approval, true); err != nil {
		t.Fatal(err)
	}
	approvedFP := fingerprint(approval)
	for _, c := range []struct {
		name, in, to, key string
		want              int
	}{
		{"without the synthetic key", ws, changedTo, "", http.StatusUnauthorized},
		{"with a wrong key", ws, changedTo, "wrong", http.StatusUnauthorized},
		{"on a real workspace", realWS, changedTo, syntheticKey, http.StatusNotFound},
		{"at another workspace's payee", ws, elsewhere, syntheticKey, http.StatusNotFound},
		{"of an approval that is not there", ws + "-other", changedTo, syntheticKey, http.StatusNotFound},
	} {
		if code, out := repoint(c.in, approval, c.to, c.key); code != c.want {
			t.Fatalf("re-point %s = %d %v, want %d", c.name, code, out, c.want)
		}
		if fingerprint(approval) != approvedFP {
			t.Fatalf("re-point %s changed the approval's fingerprint", c.name)
		}
	}

	if code, out := repoint(ws, approval, changedTo, syntheticKey); code != http.StatusOK || out["approval_id"] != approval || out["payee_id"] != changedTo {
		t.Fatalf("re-point the approved €150.00 at Grace = %d %v, want 200 naming the approval and the new payee", code, out)
	}
	var shownPayee, shownName string
	if err := pool.QueryRow(ctx, `SELECT payee_id, payee_name FROM agent_approvals WHERE id = $1`, approval).Scan(&shownPayee, &shownName); err != nil ||
		shownPayee != approvedTo || shownName != "Ada Lovelace" {
		t.Fatalf("what the owner was shown is now %s %q (%v), want Ada's, kept", shownPayee, shownName, err)
	}

	// The agent's same payment, now to Grace: found by the approval, and stopped because it is not the one approved.
	if code, out := pay(changedTo); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "not the one you approved") {
		t.Fatalf("pay the approved €150.00 to Grace = %d %v, want 403 as not the payment approved", code, out)
	}
	nothingMoved("after the changed payment")
	var read struct {
		Status  string `json:"status"`
		Stopped *struct {
			Changed string    `json:"changed"`
			At      time.Time `json:"at"`
		} `json:"stopped"`
	}
	code, out := call(http.MethodGet, "/v1/workspaces/"+ws+"/agents/approvals/"+approval, "")
	raw, _ := json.Marshal(out)
	_ = json.Unmarshal(raw, &read)
	if code != http.StatusOK || read.Status != "approved" || read.Stopped == nil || read.Stopped.Changed != economy.StoppedPayee || read.Stopped.At.IsZero() {
		t.Fatalf("the approval reads %d %v, want still approved and stopped because the payee changed", code, out)
	}
}
