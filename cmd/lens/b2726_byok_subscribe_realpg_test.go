package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/byok"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/envelope"
)

// b2726Stripe is a Stripe test-mode account holding Plus, Pro and Max but no BYOK Price until Lens creates one,
// and the BYOK subscription a checkout makes. It records every call.
type b2726Stripe struct {
	mu        sync.Mutex
	calls     []stripeCall
	prices    []map[string]any
	periodEnd int64
}

func (s *b2726Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	if r.Method == http.MethodGet {
		form = r.URL.Query()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stripeCall{path: r.URL.Path, key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), form: form})
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/prices" && r.Method == http.MethodGet:
		data := []map[string]any{}
		for _, p := range s.prices {
			for field, keys := range form {
				if strings.HasPrefix(field, "lookup_keys") && keys[0] == p["lookup_key"] {
					data = append(data, p)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "url": "/v1/prices", "has_more": false, "data": data})
	case r.URL.Path == "/v1/prices":
		// B32.10: Lens creates Team's and Business's beside BYOK's; each is named for its plan.
		id := "price_test_" + strings.TrimSuffix(strings.TrimPrefix(form.Get("lookup_key"), "talyvor_"), "_monthly")
		p := map[string]any{"id": id, "object": "price", "active": true, "lookup_key": form.Get("lookup_key"),
			"currency": form.Get("currency"), "unit_amount": json.Number(form.Get("unit_amount"))}
		s.prices = append(s.prices, p)
		_ = json.NewEncoder(w).Encode(p)
	case r.URL.Path == "/v1/customers":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cus_b2726", "object": "customer"})
	case r.URL.Path == "/v1/checkout/sessions":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cs_test_b2726", "object": "checkout.session", "url": "https://checkout.stripe.com/c/pay/cs_test_b2726"})
	case r.URL.Path == "/v1/subscriptions/sub_b2726":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "sub_b2726", "object": "subscription", "status": "active", "customer": "cus_b2726",
			"current_period_start": s.periodEnd - 30*86400, "current_period_end": s.periodEnd, "cancel_at_period_end": false,
			"metadata": map[string]string{"workspace_id": "s-b2726-tester"},
			"items": map[string]any{"object": "list", "data": []map[string]any{{"id": "si_b2726", "object": "subscription_item", "quantity": 1,
				"price": map[string]any{"id": "price_test_byok", "object": "price", "currency": "usd", "unit_amount": 19900,
					"lookup_key": billing.BYOKLookupKey}}}}})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"not in this fake"}}`)
	}
}

// call is the first call to path whose form carries field.
func (s *b2726Stripe) call(path, field string) (stripeCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c.path == path && c.form.Has(field) {
			return c, true
		}
	}
	return stripeCall{}, false
}

// B27.26 — ON A TEST-MODE LENS, A TEST USER SUBSCRIBES TO BYOK AT $199 A MONTH AND ADDS THEIR OWN KEY.
//
// Lens creates the BYOK Price in the Stripe test-mode account ($199, monthly, lookup key talyvor_byok_monthly)
// because it has none, sells it beside Plus, Pro and Max, opens its checkout, and records the paid subscription
// as BYOK with no token allowance. The user's OpenAI key is then refused before the subscription and accepted
// after it, sealed at rest and never shown back beyond its last four characters. That a question then goes
// upstream on the key with no token charge is TestB2726_ABYOKQuestionGoesUpstreamOnItsOwnKeyWithNoTokenCharge.
func TestB2726_ATestUserSubscribesToBYOKAt199AndAddsTheirKey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const testWS, secret, key, ownKey = "s-b2726-tester", "whsec_b2726", "sk_test_b2726", "sk-proj-b2726-own-key-9f3c"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, testWS); err != nil {
		t.Fatal(err)
	}
	fake := &b2726Stripe{periodEnd: time.Now().Add(29 * 24 * time.Hour).Unix()}
	for plan, cents := range map[string]int{"plus": 2000, "pro": 10000, "max": 20000} {
		fake.prices = append(fake.prices, map[string]any{"id": "price_test_" + plan, "object": "price", "active": true,
			"lookup_key": "talyvor_" + plan + "_monthly", "currency": "usd", "unit_amount": cents})
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	liveStripe := billing.NewLiveStripe(key, "", "")
	plans := sellablePlans(ctx, nil, key, liveStripe, "LENS_BILLING_SUBSCRIPTION_PLANS")
	created, ok := fake.call("/v1/prices", "unit_amount")
	if !ok || created.key != key || created.form.Get("unit_amount") != "19900" || created.form.Get("currency") != "usd" ||
		created.form.Get("recurring[interval]") != "month" || created.form.Get("lookup_key") != billing.BYOKLookupKey {
		t.Fatalf("the BYOK Price Lens created = %v on key %q; want $199 (19900 usd cents) a month under %s on %s",
			created.form, created.key, billing.BYOKLookupKey, key)
	}
	if plans["byok"] != "price_test_byok" || len(plans) != 6 {
		t.Fatalf("the test-mode account's plans = %v; want plus, pro, max, byok, team and business", plans)
	}
	// The next boot finds it and creates no second one.
	sellablePlans(ctx, nil, key, liveStripe, "LENS_BILLING_SUBSCRIPTION_PLANS")
	if n := len(fake.prices); n != 6 {
		t.Fatalf("after a second boot the account holds %d Prices; want the BYOK Price created once", n)
	}

	ring, err := envelope.NewKeyring(bytes.Repeat([]byte{9}, envelope.KEKLen))
	if err != nil {
		t.Fatal(err)
	}
	store := byok.New(pool, ring)
	svc := billing.New(pool, economy.NewDualTokenStore(nil, pool, nil), liveStripe, secret).WithPlans(liveStripe, plans).WithAllowance(5_000_000)
	b := newBillingRouter(svc, nil, func(ws string) bool { return ws == testWS }, key, "", "")
	r := chi.NewRouter()
	r.Post("/v1/workspaces/{wsID}/billing/subscribe", b.onSubscriptions(true, newSubscribeHandler))
	r.Post("/v1/billing/webhook", svc.HandleWebhook)
	r.Get("/v1/workspaces/{wsID}/provider-keys", newProviderKeysListHandler(store))
	r.Put("/v1/workspaces/{wsID}/provider-keys/{provider}", newProviderKeyPutHandler(store))
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	putKey := `{"key":"` + ownKey + `"}`

	// Before subscribing, the key is refused: BYOK is the subscription.
	if w := do(http.MethodPut, "/v1/workspaces/"+testWS+"/provider-keys/openai", putKey); w.Code != http.StatusPaymentRequired {
		t.Fatalf("a key before the BYOK subscription = %d %s; want 402", w.Code, w.Body.String())
	}
	// B17.278: and the list offers no provider to add one for, and holds none.
	if w := do(http.MethodGet, "/v1/workspaces/"+testWS+"/provider-keys", ""); w.Code != http.StatusOK ||
		w.Body.String() != `{"byok":false,"keys":[],"providers":[]}`+"\n" {
		t.Fatalf("the key list before the BYOK subscription = %d %q; want byok false, no keys, no providers", w.Code, w.Body.String())
	}

	// Plans → Choose BYOK: a Stripe test-mode checkout for the BYOK Price.
	if w := do(http.MethodPost, "/v1/workspaces/"+testWS+"/billing/subscribe", `{"plan":"byok"}`); w.Code != http.StatusOK {
		t.Fatalf("Choose BYOK = %d %s", w.Code, w.Body.String())
	}
	if c, _ := fake.call("/v1/checkout/sessions", "line_items[0][price]"); c.form.Get("line_items[0][price]") != "price_test_byok" {
		t.Fatalf("the checkout's price = %q; want price_test_byok", c.form.Get("line_items[0][price]"))
	}

	// Paid with card 4242: Stripe sends checkout.session.completed.
	payload, _ := json.Marshal(map[string]any{"id": "evt_b2726_completed", "object": "event", "type": "checkout.session.completed",
		"created": time.Now().Unix(), "livemode": false, "data": map[string]any{"object": map[string]any{"id": "cs_test_b2726",
			"object": "checkout.session", "mode": "subscription", "payment_status": "paid", "status": "complete",
			"subscription": "sub_b2726", "metadata": map[string]string{"workspace_id": testWS}}}})
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(payload))
	req.Header.Set("Stripe-Signature", signed.Header)
	ww := httptest.NewRecorder()
	r.ServeHTTP(ww, req)
	var price string
	var isBYOK bool
	var grants int
	if err := pool.QueryRow(ctx, `SELECT price_id, byok, (SELECT count(*) FROM subscription_allowance WHERE workspace_id = $1)
		FROM subscriptions WHERE workspace_id = $1 AND status = 'active'`, testWS).Scan(&price, &isBYOK, &grants); err != nil ||
		price != "price_test_byok" || !isBYOK || grants != 0 {
		t.Fatalf("the subscription = %q byok %v with %d allowance grants (%v, webhook %d); want an active BYOK subscription, no allowance",
			price, isBYOK, grants, err, ww.Code)
	}

	// Settings → Your provider keys: add the OpenAI key. Only its last four characters ever come back.
	w := do(http.MethodPut, "/v1/workspaces/"+testWS+"/provider-keys/openai", putKey)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"last4":"9f3c"`) || strings.Contains(w.Body.String(), ownKey) {
		t.Fatalf("adding the key = %d %s; want 200 with last4 9f3c and never the key", w.Code, w.Body.String())
	}
	w = do(http.MethodGet, "/v1/workspaces/"+testWS+"/provider-keys", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"byok":true`) ||
		!strings.Contains(w.Body.String(), `"provider":"openai","last4":"9f3c"`) || !strings.Contains(w.Body.String(), `"providers":["anthropic",`) || strings.Contains(w.Body.String(), ownKey) {
		t.Fatalf("the key list = %d %s; want byok and openai ending 9f3c, never the key", w.Code, w.Body.String())
	}
	var row []byte
	if err := pool.QueryRow(ctx, `SELECT row_to_json(k)::text::bytea FROM workspace_provider_keys k WHERE workspace_id = $1`, testWS).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row, []byte(ownKey)) || bytes.Contains(row, []byte(ownKey[:12])) {
		t.Fatal("the stored row holds the key in the clear")
	}
	if keys, err := store.OwnKeys(ctx, testWS); err != nil || keys["openai"] != ownKey {
		t.Fatalf("the serving path opens %d keys (%v); want the OpenAI key back", len(keys), err)
	}
}
