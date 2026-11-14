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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// B58.7 — a workspace with no passkey and no phone key pays nothing in live money at a person's word: a payment to a
// payee and a payout batch approved in live money are refused 403 with "Add a passkey or your phone before your first
// payment." and nothing on the ledger. The same payment in test money still posts. Once the workspace has a key, a live
// payment is asked for that key's signature, as a payment always was (B54.33).
func TestLivePayment_WaitsForTheWorkspacesFirstKey(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b587-live-needs-key"
	ctx := economy.WithUseCountry(context.Background(), "GB")
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	b587LiveMoney(t, pool, store, ws)
	for _, funding := range []string{economy.FundingTest, economy.FundingLive} {
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "top_up",
			IdempotencyKey: "top-up-" + funding, Funding: funding, Counterparty: "Payer", Postings: []economy.MoneyPosting{
				{AccountID: partnerAccount, AmountMinor: -500000}, {AccountID: company.ID, AmountMinor: 500000}}}); err != nil {
			t.Fatalf("top up %s: %v", funding, err)
		}
	}

	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	mountMoneyPayoutRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(auth.WithAuthContext(economy.WithUseCountry(req.Context(), "GB"), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	payment := func(key, funding string) string {
		return `{"payee_id":"` + payee.ID + `","account_id":"` + company.ID + `","currency":"GBP","amount_minor":2000,"funding":"` + funding +
			`","reference":"Invoice","idempotency_key":"` + key + `"}`
	}
	paidOut := func(funding string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind LIKE 'payment_out%' AND p.funding = $2 AND p.account_id = $3`, ws, funding, company.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sentence := economy.ErrKeyBeforeLivePayment.Error()

	if code, out := call("/v1/money/payments", payment("live-1", economy.FundingLive)); code != http.StatusForbidden || out["error"] != sentence {
		t.Errorf("a live payment with no key = %d %v, want 403 %q", code, out, sentence)
	}
	code, out := call("/v1/money/payouts", `{"funding":"live","idempotency_key":"payroll","rows":[{"payee_id":"`+payee.ID+
		`","amount_minor":3000,"currency":"GBP","reference":"Pay"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("upload a live batch = %d %v, want 201", code, out)
	}
	if code, out := call("/v1/money/payouts/"+out["id"].(string)+"/approve", ""); code != http.StatusForbidden || out["error"] != sentence {
		t.Errorf("approving a live payout batch with no key = %d %v, want 403 %q", code, out, sentence)
	}
	if n := paidOut(economy.FundingLive); n != 0 {
		t.Fatalf("with no key the live account shows %d payment postings, want nothing on the ledger", n)
	}

	if code, out := call("/v1/money/payments", payment("test-1", economy.FundingTest)); code != http.StatusCreated || paidOut(economy.FundingTest) == 0 {
		t.Errorf("a test payment with no key = %d %v, want 201 and posted", code, out)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation)
		VALUES ($1, $2, 'test', '\x00', '\x00')`, "dev_"+ws, ws); err != nil {
		t.Fatal(err)
	}
	if code, out := call("/v1/money/payments", payment("live-2", economy.FundingLive)); code != http.StatusForbidden ||
		!strings.Contains(jsonOf(out["error"]), "Sign this payment's challenge") {
		t.Errorf("an unsigned live payment with a key = %d %v, want 403 asking for its signature", code, out)
	}
	if n := paidOut(economy.FundingLive); n != 0 {
		t.Fatalf("the live account shows %d payment postings, want nothing on the ledger", n)
	}
}

// B58.7 — with no key, an agent's request to spend live money is not approved at a person's word, one at a time or with
// others: 403 with the same sentence, and it stays pending. A request to spend test money is approved as before, and the
// live one may still be declined, which moves nothing.
func TestLiveApproval_WaitsForTheWorkspacesFirstKey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b587-live-approval"
	store := economy.NewDualTokenStore(nil, pool, nil)
	agent, err := store.CreateAgent(ctx, ws, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, paid_from_kind, paid_from_funding)
		VALUES ('apr_live', $1, $2, 'fp-live', 3000000, 'account', 'live'), ('apr_test', $1, $2, 'fp-test', 3000000, 'account', 'test')`, ws, agent.ID); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/approvals"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	status := func(id string) (s string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	sentence := economy.ErrKeyBeforeLivePayment.Error()

	if code, out := call("/apr_live/approve", ""); code != http.StatusForbidden || out["error"] != sentence {
		t.Errorf("approving a live request with no key = %d %v, want 403 %q", code, out, sentence)
	}
	if code, out := call("/decide", `{"approval_ids":["apr_live","apr_test"],"decision":"approve"}`); code != http.StatusForbidden || out["error"] != sentence {
		t.Errorf("approving a live request with another, with no key = %d %v, want 403 %q", code, out, sentence)
	}
	if s := status("apr_live") + " " + status("apr_test"); s != "pending pending" {
		t.Fatalf("after the refusals the requests are %q, want both still pending", s)
	}
	if code, out := call("/apr_test/approve", ""); code != http.StatusOK || status("apr_test") != "approved" {
		t.Errorf("approving a test request with no key = %d %v, want 200 and approved", code, out)
	}
	if code, out := call("/apr_live/deny", ""); code != http.StatusOK || status("apr_live") != "denied" {
		t.Errorf("declining the live request = %d %v, want 200 and denied", code, out)
	}
}

// b587LiveMoney gives ws what live money needs besides a key: a plan with live money, the owner's checks, a limit for
// their level, and the clearances to hold money, take it in and pay it out in GB.
func b587LiveMoney(t *testing.T, pool *pgxpool.Pool, store *economy.DualTokenStore, ws string) {
	t.Helper()
	ctx := economy.WithUseCountry(context.Background(), "GB")
	for _, q := range []string{
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`,
		`INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id, status, livemode, last_event_at, plan, byok)
			VALUES ($1, 'sub_' || $1, 'cus_' || $1, 'price_team', 'active', false, NOW(), 'team', false)`,
		`INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, country)
			VALUES ($1, 1, 'contact', 'kyc-partner', 'completed', 'ref-1-' || $1, ''), ($1, 2, 'person', 'kyc-partner', 'completed', 'ref-2-' || $1, 'GB'),
			($1, 3, 'company', 'kyc-partner', 'completed', 'ref-3-' || $1, 'GB')`,
		`INSERT INTO verification_level_limits (level, currency, limit_minor, operator, reference) SELECT 1, 'GBP', 4611686018427387904, 'test', $1`,
	} {
		if _, err := pool.Exec(ctx, q, ws); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{economy.CapabilityCurrencyAccounts, economy.CapabilityPaymentsIn, economy.CapabilityPaymentsOut} {
		if _, err := store.ClearCapability(ctx, key, "nicolai", economy.ClearanceTerms{Reference: "B58.7 test", Licence: "EMI-900123",
			Partner: "Test Payments Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
}

// B58.7 — a mandate the owner granted with no key pulls no live money: the payee's live pull is refused 403 with the
// sentence and nothing on the ledger, while a pull of test money under the same mandate moves as before.
func TestLiveMandatePull_WaitsForThePayersFirstKey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := economy.WithUseCountry(context.Background(), "GB")
	const ws, payeeWS = "ws-b587-payer", "ws-b587-payee"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screener)
	store.SetPayeeScreener(screener)
	b587LiveMoney(t, pool, store, ws)
	agent, err := store.CreateAgent(ctx, ws, "Subscriber", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	agentGBP, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, payeeWS, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	for _, funding := range []string{economy.FundingTest, economy.FundingLive} {
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "top_up",
			IdempotencyKey: "top-up-" + funding, Funding: funding, Counterparty: "Payer", Postings: []economy.MoneyPosting{
				{AccountID: partnerAccount, AmountMinor: -10000}, {AccountID: agentGBP.ID, AmountMinor: 10000}}}); err != nil {
			t.Fatalf("top up %s: %v", funding, err)
		}
	}

	r := chi.NewRouter()
	mountMandateRoutes(r, store)
	mountMandatePullRoute(r, store, func(h http.Handler) http.Handler { return h })
	call := func(asWS, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(economy.WithUseCountry(req.Context(), "GB"), &auth.AuthContext{WorkspaceID: asWS,
			AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	pulled := func(funding string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
			WHERE e.kind = 'mandate_pull' AND p.funding = $1`, funding).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	code, m := call(ws, "/v1/money/mandates", `{"account_id":"`+agentGBP.ID+`","payee_workspace_id":"`+payeeWS+
		`","max_per_pull_minor":3000,"max_per_month_minor":5000,"expires_at":"`+time.Now().Add(30*24*time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if code != http.StatusCreated || m["status"] != "active" {
		t.Fatalf("grant with no key = %d %v, want 201 and active", code, m)
	}
	pull := "/v1/money/mandates/" + m["id"].(string) + "/pull"

	sentence := economy.ErrKeyBeforeLivePayment.Error()
	if code, out := call(payeeWS, pull, `{"amount_minor":2000,"funding":"live","idempotency_key":"inv-live"}`); code != http.StatusForbidden || out["error"] != sentence {
		t.Errorf("a live pull from a workspace with no key = %d %v, want 403 %q", code, out, sentence)
	}
	if n := pulled(economy.FundingLive); n != 0 {
		t.Fatalf("the refused live pull posted %d rows, want nothing on the ledger", n)
	}
	if code, out := call(payeeWS, pull, `{"amount_minor":2000,"idempotency_key":"inv-test"}`); code != http.StatusCreated || pulled(economy.FundingTest) != 2 {
		t.Errorf("a test pull = %d %v (%d test rows), want 201 and its two postings", code, out, pulled(economy.FundingTest))
	}
}
