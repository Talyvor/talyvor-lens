package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
)

type b5013Pages struct{ inv economy.Invoice }

func (p b5013Pages) InvoiceByToken(context.Context, string) (economy.Invoice, error) {
	return p.inv, nil
}
func (p b5013Pages) CurrencyAccountDetails(context.Context, string, string, string) (economy.CurrencyAccountDetails, error) {
	return economy.CurrencyAccountDetails{}, economy.ErrMoneyAccountNotFound
}

// B50.13 — while the main Stripe key is a test-mode one, every route that starts a real workspace's payment answers
// 403 "card payments are not switched on": a top-up, a plan checkout, a plan change, the BYOK add-on and an invoice
// paid by card. A synthetic workspace pays as before, and with a live key a real workspace pays as before.
func TestB5013_ARealWorkspaceCannotPayWhileTheMainKeyIsATestModeKey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const real, synthetic = "ws-b5013-real", "s-b5013-test"
	for _, ws := range []struct {
		id        string
		synthetic bool
	}{{real, false}, {synthetic, true}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`, ws.id, ws.synthetic); err != nil {
			t.Fatal(err)
		}
	}
	isTest := func(ws string) bool { return ws == synthetic }
	fake := &stripeKeyRecorder{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	// As main.go wires it with only the main key set: the one Service, and test workspaces pay through it while it is test mode.
	router := func(key string) http.Handler {
		ls := billing.NewLiveStripe(key, "", "")
		svc := billing.New(pool, economy.NewDualTokenStore(nil, pool, nil), ls, "whsec_b5013").WithPlans(ls, map[string]string{"plus": "price_plus"})
		b := newBillingRouter(svc, nil, isTest, key, "", "")
		reached := func(*billing.Service) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
		}
		r := chi.NewRouter()
		r.Post("/v1/workspaces/{wsID}/billing/checkout", newCheckoutHandler(b))
		r.Post("/v1/workspaces/{wsID}/billing/subscribe", b.onSubscriptions(true, newSubscribeHandler))
		r.Post("/v1/workspaces/{wsID}/billing/subscription/plan", b.onSubscriptions(true, reached))
		r.Post("/v1/workspaces/{wsID}/billing/subscription/byok", b.onSubscriptions(true, reached))
		return r
	}
	call := func(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	routes := []struct{ path, body string }{
		{"/billing/checkout", `{"usd_cents":2500}`},
		{"/billing/subscribe", `{"plan":"plus"}`},
		{"/billing/subscription/plan", `{"plan":"plus"}`},
		{"/billing/subscription/byok", ``},
	}

	testMode := router("sk_test_b5013")
	for _, rt := range routes {
		if w := call(testMode, http.MethodPost, "/v1/workspaces/"+real+rt.path, rt.body); w.Code != http.StatusForbidden ||
			!strings.Contains(w.Body.String(), "card payments are not switched on") {
			t.Errorf("a real workspace's %s on a test-mode key = %d %s; want 403 card payments are not switched on", rt.path, w.Code, w.Body.String())
		}
		if w := call(testMode, http.MethodPost, "/v1/workspaces/"+synthetic+rt.path, rt.body); w.Code != http.StatusOK {
			t.Errorf("a synthetic workspace's %s on a test-mode key = %d %s; want 200", rt.path, w.Code, w.Body.String())
		}
	}
	if n := fake.count("/v1/checkout/sessions"); n != 2 {
		t.Fatalf("Stripe was asked for %d Checkout Session(s); want the synthetic workspace's two only", n)
	}

	live := router("sk_live_b5013")
	if w := call(live, http.MethodPost, "/v1/workspaces/"+real+"/billing/checkout", `{"usd_cents":2500}`); w.Code != http.StatusOK ||
		fake.last(t, "/v1/checkout/sessions").key != "sk_live_b5013" {
		t.Fatalf("a real workspace's top-up on a live key = %d %s; want a live Checkout Session", w.Code, w.Body.String())
	}

	// An invoice's card is always Stripe test mode: only a synthetic workspace's invoice offers or takes one.
	for _, ws := range []string{real, synthetic} {
		r := chi.NewRouter()
		cards := &fakeInvoiceCards{}
		mountInvoicePayRoutes(r, b5013Pages{economy.Invoice{ID: "inv_b5013", WorkspaceID: ws, Status: economy.InvoiceSent, DueMinor: 12000, Currency: "GBP"}},
			cards, isTest)
		var page map[string]any
		_ = json.Unmarshal(call(r, http.MethodGet, "/v1/pay/tok_b5013", "").Body.Bytes(), &page)
		w := call(r, http.MethodPost, "/v1/pay/tok_b5013/card", "")
		if ws == real && (page["card"] != false || w.Code != http.StatusForbidden || cards.asked.InvoiceID != "") {
			t.Errorf("a real workspace's invoice: card offered=%v, card route %d %s, Stripe asked for %q; want no card, 403, none",
				page["card"], w.Code, w.Body.String(), cards.asked.InvoiceID)
		}
		if ws == synthetic && (page["card"] != true || w.Code != http.StatusCreated) {
			t.Errorf("a synthetic workspace's invoice: card offered=%v, card route %d %s; want a card and 201", page["card"], w.Code, w.Body.String())
		}
	}
}
