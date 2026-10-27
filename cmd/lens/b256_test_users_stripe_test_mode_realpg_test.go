package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/tenant"
)

// b256Call is one request Lens made to Stripe: its method and path, the secret key it carried, and what it sent.
type b256Call struct {
	method, path, key string
	form              url.Values
}

// b256Stripe is Stripe's API for the marketplace bill, Connect and Issuing, recording the key of every call.
type b256Stripe struct {
	mu    sync.Mutex
	calls []b256Call
}

func (s *b256Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	if r.Method == http.MethodGet {
		form = r.URL.Query()
	}
	s.mu.Lock()
	s.calls = append(s.calls, b256Call{r.Method, r.URL.Path, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), form})
	n := len(s.calls)
	s.mu.Unlock()
	reply := func(v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch p := r.URL.Path; {
	case p == "/v1/customers":
		reply(map[string]any{"id": fmt.Sprintf("cus_b256_%d", n), "object": "customer"})
	case p == "/v1/subscriptions":
		reply(map[string]any{"id": "sub_" + form.Get("metadata[market_workspace_id]"), "object": "subscription"})
	case p == "/v1/billing/meter_events":
		reply(map[string]any{"object": "billing.meter_event", "identifier": form.Get("identifier")})
	case p == "/v1/invoices": // B26.4: a credit line invoice
		reply(map[string]any{"id": "in_" + form.Get("metadata[credit_line_workspace_id]"), "object": "invoice"})
	case strings.HasPrefix(p, "/v1/invoices/") && strings.HasSuffix(p, "/finalize"):
		reply(map[string]any{"id": strings.TrimSuffix(strings.TrimPrefix(p, "/v1/invoices/"), "/finalize"), "object": "invoice",
			"due_date": time.Now().AddDate(0, 0, 14).Unix()})
	case p == "/v1/invoiceitems":
		reply(map[string]any{"id": fmt.Sprintf("ii_b256_%d", n), "object": "invoiceitem"})
	case p == "/v2/core/accounts": // B17.23: Accounts v2, JSON
		var in struct{ Metadata map[string]string }
		_ = json.Unmarshal(body, &in)
		reply(map[string]any{"id": "acct_" + in.Metadata["market_workspace_id"], "object": "v2.core.account", "identity": map[string]any{"country": "gb"}})
	case p == "/v2/core/account_links":
		var in struct{ Account string }
		_ = json.Unmarshal(body, &in)
		reply(map[string]any{"object": "v2.core.account_link", "url": "https://connect.stripe.com/setup/e/" + in.Account})
	case strings.HasPrefix(p, "/v1/accounts/"): // onboarding finished
		reply(map[string]any{"id": strings.TrimPrefix(p, "/v1/accounts/"), "object": "account", "country": "GB",
			"details_submitted": true, "payouts_enabled": true})
	case p == "/v1/transfers" && r.Method == http.MethodGet:
		reply(map[string]any{"object": "list", "url": "/v1/transfers", "has_more": false, "data": []any{}})
	case p == "/v1/transfers":
		reply(map[string]any{"id": fmt.Sprintf("tr_b256_%d", n), "object": "transfer"})
	case strings.HasPrefix(p, "/v1/charges/"): // ch_<invoice> paid <invoice>
		id := strings.TrimPrefix(p, "/v1/charges/")
		reply(map[string]any{"id": id, "object": "charge", "amount": 3000, "invoice": strings.TrimPrefix(id, "ch_")})
	case p == "/v1/issuing/cardholders":
		reply(map[string]any{"id": fmt.Sprintf("ich_b256_%d", n), "object": "issuing.cardholder"})
	case p == "/v1/issuing/cards":
		reply(map[string]any{"id": fmt.Sprintf("ic_b256_%d", n), "object": "issuing.card", "last4": "4242", "exp_month": 9,
			"exp_year": 2029, "currency": "gbp", "livemode": false})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"not in this fake"}}`)
	}
}

// during runs f and returns the calls Lens made to Stripe meanwhile.
func (s *b256Stripe) during(f func()) []b256Call {
	s.mu.Lock()
	from := len(s.calls)
	s.mu.Unlock()
	f()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]b256Call(nil), s.calls[from:]...)
}

// B25.6 — AFTER THE LIVE SWITCH A TEST USER'S MARKETPLACE BILL, PAYOUTS AND AGENT CARDS STAY IN STRIPE TEST MODE,
// AND A REAL USER'S ARE UNCHANGED.
//
// Wired as main.go wires it once LENS_STRIPE_SECRET_KEY is live: the live Service and Stripe clients on sk_live_,
// the test workspaces' on sk_test_ with their own webhook and metered price. Stripe is a recorder of the key
// each call carried; webhook events are signed as Stripe signs them. Every assertion that money moved is on a row.
func TestB256_ATestUsersBillPayoutsAndCardsStayInStripeTestModeAfterTheLiveSwitch(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const (
		tBuyer, tSeller, rBuyer, rSeller = "s-b256-buyer", "s-b256-seller", "u-b256-buyer", "u-b256-seller"
		liveKey, testKey                 = "sk_live_b256", "sk_test_b256"
		liveSecret, testSecret           = "whsec_b256_live", "whsec_b256_test"
	)
	for _, ws := range []string{tBuyer, tSeller, rBuyer, rSeller} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`,
			ws, strings.HasPrefix(ws, "s-")); err != nil {
			t.Fatal(err)
		}
	}

	fake := &b256Stripe{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	liveStripe := billing.NewLiveStripe(liveKey, "", "")
	live := billing.New(pool, bank, liveStripe, liveSecret).ForTestWorkspaces(false).
		WithMarketBill(liveStripe, "price_live_market", "talyvor_marketplace_use", store).WithMarketPayouts(liveStripe, store)
	testStripe := billing.NewTestModeStripe(testKey, "", "")
	test := billing.New(pool, bank, testStripe, testSecret).ForTestWorkspaces(true).
		WithMarketBill(testStripe, "price_test_market", "talyvor_marketplace_use", store).WithMarketPayouts(testStripe, store)
	isTest := func(ws string) bool { return strings.HasPrefix(ws, "s-") }
	liveSide := stripeSide{bill: live, connect: liveStripe, cards: agentcard.NewStripe(liveKey, "gbp")}
	kinds := newStripeByKind(isTest, true, true, liveSide, stripeSide{bill: test, connect: testStripe, cards: agentcard.NewStripe(testKey, "gbp")})
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	mount := func(k stripeByKind) http.Handler {
		r := chi.NewRouter()
		mountMarketRoutes(r, store)
		mountMarketUseRoutes(r, store, lens, k, bank)
		mountMarketPayoutRoutes(r, store, k.connectFor, bank, marketPayoutURLs{refresh: "https://app.test/expired", ret: "https://app.test/selling"})
		mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
		mountAgentCardRoutes(r, bank, k)
		r.Post("/v1/billing/webhook", live.HandleWebhook)
		r.Post("/v1/billing/webhook/test", test.HandleWebhook)
		return r
	}
	r := mount(kinds)
	call := func(h http.Handler, ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	do := func(ws, method, path, body string, want int, out any) {
		t.Helper()
		code, got := call(r, ws, method, path, body)
		if code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, code, got, want)
		}
		if out != nil {
			if err := json.Unmarshal([]byte(got), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, got)
			}
		}
	}
	// only asserts that every Stripe call made carried key, and that each of paths was called.
	only := func(what, key string, calls []b256Call, paths ...string) {
		t.Helper()
		seen := map[string]bool{}
		for _, c := range calls {
			if c.key != key {
				t.Errorf("%s: %s %s went to Stripe with key %q; want %q", what, c.method, c.path, c.key, key)
			}
			seen[c.method+" "+c.path] = true
		}
		for _, p := range paths {
			if !seen[p] {
				t.Errorf("%s: Lens never called Stripe's %s (calls: %+v)", what, p, calls)
			}
		}
	}
	deliver := func(path, secret, eventType string, object map[string]any) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("evt_b256_%d", time.Now().UnixNano()), "object": "event",
			"type": eventType, "created": time.Now().Unix(), "livemode": secret == liveSecret, "data": map[string]any{"object": object}})
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(payload)))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		if r.ServeHTTP(w, req); w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", path, eventType, w.Code, w.Body.String())
		}
	}
	now := time.Now()
	paidAt, usedAt := now.AddDate(0, 0, -20), now.AddDate(0, 0, -25) // past the 14-day holdback
	invoice := func(id, sub, price string) map[string]any {
		return map[string]any{"id": id, "object": "invoice", "subscription": sub, "status_transitions": map[string]any{"paid_at": paidAt.Unix()},
			"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_" + id, "object": "line_item",
				"period": map[string]any{"start": usedAt.Add(-time.Hour).Unix(), "end": usedAt.Add(time.Hour).Unix()},
				"price":  map[string]any{"id": price, "object": "price"}}}}}
	}
	cleared := func(useID string) (ok, earnedTest, earnedLive bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT u.cleared_at IS NOT NULL, COALESCE(e.test, false), COALESCE(e.livemode, false)
			FROM market_uses u LEFT JOIN market_earnings e ON e.use_id = u.id WHERE u.id = $1`, useID).Scan(&ok, &earnedTest, &earnedLive); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Each seller lists a $30 prompt; each buyer uses the one of its own kind, on its own bill. The seller keeps
	// $25.50 of it, Talyvor 15% (B32.8).
	sell := func(seller string) market.Listing {
		var l market.Listing
		do(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
			`{"kind":"prompt","title":"Contract review","price_per_use_ulxc":300000000,"artifact":{"template":"Review {{text}}","model":"m"}}`, http.StatusCreated, &l)
		return l
	}
	useOf := func(buyer string, l market.Listing) market.Use {
		var u market.Use
		do(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+l.ID+"/use", `{"variables":{"text":"the NDA"}}`, http.StatusOK, &u)
		if u.Charge != market.ChargeBilled || u.MeterError != "" {
			t.Fatalf("%s's use = %+v, want billed and metered", buyer, u)
		}
		if _, err := pool.Exec(ctx, `UPDATE market_uses SET used_at = $2 WHERE id = $1`, u.ID, usedAt); err != nil {
			t.Fatal(err)
		}
		return u
	}
	tListing, rListing := sell(tSeller), sell(rSeller)

	// 1. METERED: the test user's use goes on a bill made and metered in Stripe TEST MODE, at the test-mode price.
	var tUse market.Use
	calls := fake.during(func() { tUse = useOf(tBuyer, tListing) })
	only("the test user's use", testKey, calls, "POST /v1/customers", "POST /v1/subscriptions", "POST /v1/billing/meter_events")
	for _, c := range calls {
		if c.path == "/v1/subscriptions" && c.form.Get("items[0][price]") != "price_test_market" {
			t.Errorf("the test user's bill is on price %q, want the test-mode price_test_market", c.form.Get("items[0][price]"))
		}
		if c.path == "/v1/billing/meter_events" && c.form.Get("identifier") != tUse.ID {
			t.Errorf("the test user's meter event is for %q, want the use %s", c.form.Get("identifier"), tUse.ID)
		}
	}
	var tSub string
	var useTest bool
	if err := pool.QueryRow(ctx, `SELECT b.stripe_subscription_id, u.test FROM market_bills b JOIN market_uses u ON u.buyer_workspace_id = b.workspace_id
		WHERE u.id = $1 AND u.metered_at IS NOT NULL`, tUse.ID).Scan(&tSub, &useTest); err != nil || tSub != "sub_"+tBuyer || !useTest {
		t.Fatalf("the test user's bill = %q, use marked test=%v (%v); want sub_%s and a metered use marked test", tSub, useTest, err, tBuyer)
	}
	// The real user's is unchanged: the live key and the live price.
	var rUse market.Use
	calls = fake.during(func() { rUse = useOf(rBuyer, rListing) })
	only("the real user's use", liveKey, calls, "POST /v1/subscriptions", "POST /v1/billing/meter_events")

	// 2. CLEARED: the test user's paid invoice clears its use from the test-mode webhook, as test earnings. Before
	// the live switch both endpoints hear it, and may name one price; the live one leaves it alone even so.
	deliver("/v1/billing/webhook", liveSecret, "invoice.paid", invoice("in_b256_test", tSub, "price_live_market"))
	if ok, _, _ := cleared(tUse.ID); ok {
		t.Fatal("the live webhook cleared a test user's marketplace invoice — test money reached the live Service")
	}
	deliver("/v1/billing/webhook/test", testSecret, "invoice.paid", invoice("in_b256_test", tSub, "price_test_market"))
	if ok, earnedTest, earnedLive := cleared(tUse.ID); !ok || !earnedTest || earnedLive {
		t.Fatalf("after its test-mode invoice was paid the test user's use cleared=%v, earning test=%v livemode=%v; want cleared, test, not live",
			ok, earnedTest, earnedLive)
	}
	deliver("/v1/billing/webhook/test", testSecret, "invoice.paid", invoice("in_b256_real", "sub_"+rBuyer, "price_test_market"))
	if ok, _, _ := cleared(rUse.ID); ok {
		t.Fatal("the test-mode webhook cleared a real user's marketplace invoice")
	}
	deliver("/v1/billing/webhook", liveSecret, "invoice.paid", invoice("in_b256_real", "sub_"+rBuyer, "price_live_market"))
	if ok, earnedTest, earnedLive := cleared(rUse.ID); !ok || earnedTest || !earnedLive {
		t.Fatalf("the real user's use cleared=%v, earning test=%v livemode=%v; want cleared, live, not test", ok, earnedTest, earnedLive)
	}

	// 3. PAID OUT: the test seller's Connect account is made and paid in test mode; the real seller's with the live key.
	calls = fake.during(func() {
		do(tSeller, http.MethodPost, "/v1/workspaces/"+tSeller+"/marketplace/payouts/connect", `{"country":"gb"}`, http.StatusOK, nil)
	})
	only("the test seller's Connect account", testKey, calls, "POST /v2/core/accounts", "POST /v2/core/account_links")
	calls = fake.during(func() {
		do(rSeller, http.MethodPost, "/v1/workspaces/"+rSeller+"/marketplace/payouts/connect", `{"country":"gb","email":"seller@b256.example"}`, http.StatusOK, nil)
	})
	only("the real seller's Connect account", liveKey, calls, "POST /v2/core/accounts", "POST /v2/core/account_links")
	calls = fake.during(func() {
		if n, err := kinds.payOut(ctx, store, now, true); err != nil || n != 2 {
			t.Fatalf("the payout run paid %d, %v; want both sellers", n, err)
		}
	})
	for _, c := range calls {
		want := map[string]string{tSeller: testKey, rSeller: liveKey}[c.form.Get("metadata[market_workspace_id]")]
		if c.method == http.MethodPost && c.path == "/v1/transfers" && c.key != want {
			t.Errorf("the transfer to %s went to Stripe with key %q; want %q", c.form.Get("metadata[market_workspace_id]"), c.key, want)
		}
	}
	for ws, want := range map[string][2]bool{tSeller: {false, true}, rSeller: {true, false}} {
		var livemode, rowTest bool
		var transfer string
		if err := pool.QueryRow(ctx, `SELECT livemode, test, stripe_transfer_id FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe'
			AND gross_usd_micros = 25500000 AND paid_at IS NOT NULL`, ws).Scan(&livemode, &rowTest, &transfer); err != nil ||
			livemode != want[0] || rowTest != want[1] || transfer == "" {
			t.Fatalf("%s's payout: livemode=%v test=%v transfer=%q (%v); want $25.50 paid with livemode=%v test=%v", ws, livemode, rowTest, transfer, err, want[0], want[1])
		}
	}

	// 4. REFUNDED: the test user's bank refunds the bill; the test-mode webhook asks test-mode Stripe which invoice
	// the charge paid and reverses the seller's earning. A taken-down listing's credit goes on the test-mode bill.
	calls = fake.during(func() {
		deliver("/v1/billing/webhook/test", testSecret, "charge.refunded", map[string]any{"id": "ch_in_b256_test", "object": "charge",
			"amount": 3000, "amount_refunded": 3000, "refunded": true})
	})
	only("the test user's refund", testKey, calls, "GET /v1/charges/ch_in_b256_test")
	var refundTest bool
	if err := pool.QueryRow(ctx, `SELECT test FROM market_refunds WHERE use_id = $1 AND cause = 'buyer_refund'`, tUse.ID).Scan(&refundTest); err != nil || !refundTest {
		t.Fatalf("the test user's refund reversal: test=%v (%v); want a market_refunds row marked test", refundTest, err)
	}
	calls = fake.during(func() {
		if _, err := kinds.CreditMarketRefund(ctx, tBuyer, tUse.ID, 300_000_000, "a taken-down listing"); err != nil {
			t.Fatalf("crediting the test user's bill: %v", err)
		}
	})
	only("the test user's bill credit", testKey, calls, "POST /v1/invoiceitems")

	// 5. AGENT CARDS: a test agent's card is issued in Stripe Issuing test mode; a real agent's, with the live key,
	// is refused as before (cards are test money only) and Stripe is never asked.
	agentOf := func(ws, name string) string {
		var a economy.Agent
		do(ws, http.MethodPost, "/v1/workspaces/"+ws+"/agents", `{"name":"`+name+`"}`, http.StatusCreated, &a)
		return a.ID
	}
	holder := `{"first_name":"Test","last_name":"User","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`
	tAgent, rAgent := agentOf(tBuyer, "buyer"), agentOf(rBuyer, "buyer")
	calls = fake.during(func() {
		do(tBuyer, http.MethodPost, "/v1/workspaces/"+tBuyer+"/agents/"+tAgent+"/card", holder, http.StatusCreated, nil)
	})
	only("the test agent's card", testKey, calls, "POST /v1/issuing/cardholders", "POST /v1/issuing/cards")
	var cardTest, cardLive bool
	if err := pool.QueryRow(ctx, `SELECT test, livemode FROM agent_cards WHERE workspace_id = $1 AND agent_id = $2`, tBuyer, tAgent).
		Scan(&cardTest, &cardLive); err != nil || !cardTest || cardLive {
		t.Fatalf("the test agent's card: test=%v livemode=%v (%v); want an agent_cards row marked test, not live", cardTest, cardLive, err)
	}
	calls = fake.during(func() {
		if code, body := call(r, rBuyer, http.MethodPost, "/v1/workspaces/"+rBuyer+"/agents/"+rAgent+"/card", holder); code != http.StatusConflict ||
			!strings.Contains(body, "no real money") {
			t.Fatalf("the real agent's card with the live key = %d %s; want 409, as before", code, body)
		}
	})
	if len(calls) != 0 {
		t.Errorf("the real agent's refused card asked Stripe %+v", calls)
	}

	// 6. UNSET: with a live key and no test-mode key, a test user's paid use, seller account and card are refused,
	// naming the variable, and Stripe is never asked — never made with live money.
	bare := mount(newStripeByKind(isTest, true, false, liveSide, stripeSide{}))
	tAgent2 := agentOf(tBuyer, "second")
	calls = fake.during(func() {
		for _, c := range []struct{ ws, path, body, want string }{
			{tBuyer, "/v1/workspaces/" + tBuyer + "/marketplace/listings/" + tListing.ID + "/use", `{"variables":{"text":"x"}}`, "LENS_STRIPE_TEST_SECRET_KEY"},
			{tSeller, "/v1/workspaces/" + tSeller + "/marketplace/payouts/connect", `{"country":"gb"}`, "LENS_STRIPE_TEST_SECRET_KEY"},
			{tBuyer, "/v1/workspaces/" + tBuyer + "/agents/" + tAgent2 + "/card", holder, "LENS_STRIPE_TEST_SECRET_KEY"},
		} {
			if code, body := call(bare, c.ws, http.MethodPost, c.path, c.body); code != http.StatusForbidden || !strings.Contains(body, c.want) {
				t.Errorf("POST %s with no test-mode key = %d %s; want 403 naming %s", c.path, code, body, c.want)
			}
		}
	})
	if len(calls) != 0 {
		t.Errorf("with no test-mode key a test user's calls still reached Stripe: %+v", calls)
	}
	// Before the live switch, with no test-mode key, a test user keeps the main key's — itself test mode then.
	if k := newStripeByKind(isTest, false, false, liveSide, stripeSide{}); k.meterFor(tBuyer) != market.Meter(live) {
		t.Errorf("with a test-mode main key and no test-mode Service, a test user's bill is not the main one")
	}
	if stripe.Key != liveKey {
		t.Errorf("the process-wide Stripe key is %q; want the live key untouched", stripe.Key)
	}
}
