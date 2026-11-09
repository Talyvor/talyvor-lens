package main

import (
	"context"
	"errors"
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
)

// B19.7 — the owner pauses every agent with one call and resumes them with another. B55.23: a payee's pull under a
// mandate the paused agent granted is refused, and moves nothing, until the agents are started again.
func TestAgentRoutes_OneSwitchPausesEveryAgent(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	registry := partners.NewRegistry(nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screening.NewScreener(pool, registry))
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	call := func(who *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	var ids []string
	for _, name := range []string{"payer", "payee"} {
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.FundAgent(ctx, ws, a.ID, 5_000_000); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	pay := `{"to_agent_id":"` + ids[1] + `","amount_ulxc":1000}`

	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payerGBP, err := store.OpenCurrencyAccount(ctx, ws, ids[0], "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, "ws-agents-payee", "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	details, err := store.CurrencyAccountDetails(ctx, ws, "", payerGBP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: company.PartnerAccountRef, PartnerRef: "test_in_b5523",
		Payer: "Owner Ltd", Reference: details.PaymentReference, AmountMinor: 10000, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	mandate, err := store.GrantMandate(ctx, ws, "", economy.MandateGrant{AccountID: payerGBP.ID, PayeeWorkspaceID: "ws-agents-payee",
		MaxPerPullMinor: 1000, MaxPerMonthMinor: 5000, ExpiresAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	pull := func(key string) error {
		_, err := store.PullMandate(ctx, mandate.ID, economy.MandatePayee{WorkspaceID: "ws-agents-payee"}, 500, "", key, "")
		return err
	}
	payerBalance := func() int64 {
		t.Helper()
		b, err := store.Balance(ctx, ws, payerGBP.ID)
		if err != nil {
			t.Fatal(err)
		}
		return b.TestMinor
	}
	if err := pull("before"); err != nil || payerBalance() != 9500 {
		t.Fatalf("a pull before pause-all: %v, the payer holds %d; want paid, 9500", err, payerBalance())
	}

	if code, _ := call(proxyKey, http.MethodPost, base+"/pause-all", `{}`); code != http.StatusForbidden {
		t.Fatalf("a proxy key paused every agent: %d, want 403", code)
	}
	if code, body := call(owner, http.MethodPost, base+"/pause-all", `{"reason":"incident 42"}`); code != http.StatusOK {
		t.Fatalf("pause-all = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodPost, base+"/"+ids[0]+"/pay", pay); code != http.StatusForbidden || !strings.Contains(body, "incident 42") {
		t.Errorf("a payment while every agent is paused = %d %s, want 403 giving the reason", code, body)
	}
	if err := pull("paused"); !errors.Is(err, economy.ErrMandateWorkspacePaused) || payerBalance() != 9500 {
		t.Errorf("a pull while every agent is paused: %v, the payer holds %d; want refused, still 9500", err, payerBalance())
	}
	if _, book := call(owner, http.MethodGet, base, ""); !strings.Contains(book, `"all_paused_reason":"incident 42"`) {
		t.Errorf("the book does not show every agent paused: %s", book)
	}
	if code, body := call(owner, http.MethodPost, base+"/resume-all", ``); code != http.StatusOK {
		t.Fatalf("resume-all = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodPost, base+"/"+ids[0]+"/pay", pay); code != http.StatusOK {
		t.Errorf("a payment after resume-all = %d %s, want 200", code, body)
	}
	if err := pull("resumed"); err != nil || payerBalance() != 9000 {
		t.Errorf("a pull after resume-all: %v, the payer holds %d; want paid, 9000", err, payerBalance())
	}
}
