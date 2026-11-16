package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
)

// b3210Stripe is a Stripe test-mode account: its Prices (each id named for its plan), the subscription each
// checkout makes, and the item changes a plan change or an add-on asks of it. It records every call.
type b3210Stripe struct {
	mu        sync.Mutex
	calls     []stripeCall
	prices    []map[string]any
	subs      map[string]map[string]any
	items     int
	periodEnd int64
}

func (s *b3210Stripe) price(id string) map[string]any {
	for _, p := range s.prices {
		if p["id"] == id {
			return p
		}
	}
	return nil
}

func (s *b3210Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	if r.Method == http.MethodGet {
		form = r.URL.Query()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stripeCall{path: r.Method + " " + r.URL.Path, key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), form: form})
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
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
		reply(map[string]any{"object": "list", "url": "/v1/prices", "has_more": false, "data": data})
	case r.URL.Path == "/v1/prices":
		id := "price_test_" + strings.TrimSuffix(strings.TrimPrefix(form.Get("lookup_key"), "talyvor_"), "_monthly")
		p := map[string]any{"id": id, "object": "price", "active": true, "lookup_key": form.Get("lookup_key"),
			"currency": form.Get("currency"), "unit_amount": json.Number(form.Get("unit_amount"))}
		s.prices = append(s.prices, p)
		reply(p)
	case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
		reply(s.price(strings.TrimPrefix(r.URL.Path, "/v1/prices/")))
	case r.URL.Path == "/v1/customers":
		reply(map[string]any{"id": "cus_b3210", "object": "customer"})
	case r.URL.Path == "/v1/checkout/sessions":
		// The subscription the paid checkout makes, on the checkout's Price.
		ws := form.Get("subscription_data[metadata][workspace_id]")
		s.items++
		s.subs["sub_"+ws] = map[string]any{"id": "sub_" + ws, "object": "subscription", "status": "active", "customer": "cus_b3210",
			"current_period_start": s.periodEnd - 30*86400, "current_period_end": s.periodEnd, "cancel_at_period_end": false,
			"metadata": map[string]string{"workspace_id": ws},
			"items": map[string]any{"object": "list", "data": []map[string]any{{"id": fmt.Sprintf("si_%d", s.items),
				"object": "subscription_item", "quantity": 1, "price": s.price(form.Get("line_items[0][price]"))}}}}
		reply(map[string]any{"id": "cs_test_" + ws, "object": "checkout.session", "url": "https://checkout.stripe.com/c/pay/cs_test_" + ws})
	case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
		sub := s.subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]
		if r.Method == http.MethodPost {
			items := sub["items"].(map[string]any)
			data := items["data"].([]map[string]any)
			for i := 0; form.Has(fmt.Sprintf("items[%d][id]", i)) || form.Has(fmt.Sprintf("items[%d][price]", i)); i++ {
				id, pr := form.Get(fmt.Sprintf("items[%d][id]", i)), form.Get(fmt.Sprintf("items[%d][price]", i))
				kept := []map[string]any{}
				for _, it := range data {
					switch {
					case it["id"] == id && form.Get(fmt.Sprintf("items[%d][deleted]", i)) == "true":
						continue
					case it["id"] == id:
						it["price"] = s.price(pr)
					}
					kept = append(kept, it)
				}
				if id == "" {
					s.items++
					kept = append(kept, map[string]any{"id": fmt.Sprintf("si_%d", s.items), "object": "subscription_item", "quantity": 1, "price": s.price(pr)})
				}
				data = kept
			}
			items["data"] = data
		}
		reply(sub)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"not in this fake"}}`)
	}
}

// calledTo is every call to "METHOD path", in order.
func (s *b3210Stripe) calledTo(path string) []stripeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stripeCall
	for _, c := range s.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// B32.10 — TEAM AT $49 AND BUSINESS AT $299 ARE ON SALE IN STRIPE TEST MODE, AND EVERY SUBSCRIPTION NAMES ITS PLAN.
//
// Lens refuses to create a Price on a live key, and on a test-mode key creates the Team and Business Prices
// once. A test user buys Team through a test-mode checkout and the subscription is recorded as team, with no
// token allowance; adding BYOK, Team's add-on, as a second item sets byok; moving to Business drops the add-on
// (Business includes BYOK, and byok stays set) and a Team subscription is never moved to a personal plan. A
// workspace with no subscription is on free, and on enterprise once the operator records its contract — in
// the operator audit trail.
func TestB3210_TeamAndBusinessAreSoldInTestModeAndEveryPlanIsRecorded(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const secret, key = "whsec_b3210", "sk_test_b3210"
	const teamWS, freeWS = "s-b3210-team", "s-b3210-free"
	for _, ws := range []string{teamWS, freeWS} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	fake := &b3210Stripe{subs: map[string]map[string]any{}, periodEnd: time.Now().Add(29 * 24 * time.Hour).Unix()}
	for plan, cents := range map[string]int{"plus": 2000, "pro": 10000, "max": 20000, "byok": 19900} {
		fake.prices = append(fake.prices, map[string]any{"id": "price_test_" + plan, "object": "price", "active": true,
			"lookup_key": "talyvor_" + plan + "_monthly", "currency": "usd", "unit_amount": cents})
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend) })

	// A live key is refused before Stripe is called: nothing in code creates a live Price.
	if err := billing.NewTestModeStripe("sk_live_b3210", "", "").EnsurePlanPrices(ctx); err == nil || len(fake.calledTo("POST /v1/prices"))+len(fake.calledTo("GET /v1/prices")) != 0 {
		t.Fatalf("EnsurePlanPrices on a live key = %v after %d Stripe calls; want a refusal and no call", err, len(fake.calls))
	}

	// On a test-mode key, the boot creates Team and Business, once.
	st := billing.NewTestModeStripe(key, "", "")
	plans := sellablePlans(ctx, nil, key, st, "LENS_STRIPE_TEST_SUBSCRIPTION_PLANS")
	sellablePlans(ctx, nil, key, st, "LENS_STRIPE_TEST_SUBSCRIPTION_PLANS")
	created := fake.calledTo("POST /v1/prices")
	want := []struct{ lookup, cents, product string }{
		{billing.TeamLookupKey, "4900", "Talyvor Team"},
		{billing.BusinessLookupKey, "29900", "Talyvor Business"},
	}
	if len(created) != len(want) {
		t.Fatalf("two boots created %d Prices; want Team and Business, once each", len(created))
	}
	for i, w := range want {
		f := created[i].form
		if created[i].key != key || f.Get("lookup_key") != w.lookup || f.Get("unit_amount") != w.cents || f.Get("currency") != "usd" ||
			f.Get("recurring[interval]") != "month" || f.Get("product_data[name]") != w.product {
			t.Errorf("Price %d = %v on %q; want %s US cents a month, %s, %q, on %s", i, f, created[i].key, w.cents, w.lookup, w.product, key)
		}
	}
	if plans["team"] != "price_test_team" || plans["business"] != "price_test_business" {
		t.Fatalf("the plans on sale = %v; want team and business beside the others", plans)
	}

	svc := billing.New(pool, economy.NewDualTokenStore(nil, pool, nil), st, secret).WithPlans(st, plans).WithAllowance(5_000_000)
	b := newBillingRouter(svc, nil, func(ws string) bool { return strings.HasPrefix(ws, "s-b3210-") }, key, "", "")
	r := chi.NewRouter()
	r.Post("/v1/workspaces/{wsID}/billing/subscribe", b.onSubscriptions(true, newSubscribeHandler))
	r.Post("/v1/workspaces/{wsID}/billing/subscription/byok", b.onSubscriptions(true, func(svc *billing.Service) http.HandlerFunc {
		return newBYOKAddonHandler(svc, true)
	}))
	r.Method(http.MethodPut, "/v1/admin/workspaces/{wsID}/contract", newContractPutHandler(pool))
	r.Post("/v1/billing/webhook", svc.HandleWebhook)
	do := func(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	events := 0
	deliver := func(typ string, object any) {
		t.Helper()
		events++
		payload, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("evt_b3210_%d", events), "object": "event", "type": typ,
			"created": time.Now().Add(time.Duration(events) * time.Second).Unix(), "livemode": false, "data": map[string]any{"object": object}})
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
		if w := do(http.MethodPost, "/v1/billing/webhook", string(payload), "Stripe-Signature", signed.Header); w.Code != http.StatusOK {
			t.Fatalf("%s webhook = %d %s", typ, w.Code, w.Body.String())
		}
	}
	subscription := func() map[string]any {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		raw, _ := json.Marshal(fake.subs["sub_"+teamWS])
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		return v
	}
	row := func(wantPlan, wantPrice string, wantBYOK bool) {
		t.Helper()
		var plan, price string
		var byok bool
		var grants int
		if err := pool.QueryRow(ctx, `SELECT plan, price_id, byok, (SELECT count(*) FROM subscription_allowance WHERE workspace_id = $1)
			FROM subscriptions WHERE workspace_id = $1 AND status = 'active'`, teamWS).Scan(&plan, &price, &byok, &grants); err != nil {
			t.Fatalf("no active subscription for %s: %v", teamWS, err)
		}
		if plan != wantPlan || price != wantPrice || byok != wantBYOK || grants != 0 {
			t.Fatalf("the subscription = plan %s, price %s, byok %v, %d allowance grants; want plan %s, price %s, byok %v, no allowance",
				plan, price, byok, grants, wantPlan, wantPrice, wantBYOK)
		}
	}

	// Plans → Team: a test-mode checkout for the Team Price; paid with card 4242, it is recorded as team.
	if w := do(http.MethodPost, "/v1/workspaces/"+teamWS+"/billing/subscribe", `{"plan":"team"}`); w.Code != http.StatusOK {
		t.Fatalf("Choose Team = %d %s", w.Code, w.Body.String())
	}
	if c := fake.calledTo("POST /v1/checkout/sessions"); len(c) != 1 || c[0].form.Get("line_items[0][price]") != "price_test_team" {
		t.Fatalf("the checkout = %v; want one on price_test_team", c)
	}
	deliver("checkout.session.completed", map[string]any{"id": "cs_test_" + teamWS, "object": "checkout.session", "mode": "subscription",
		"payment_status": "paid", "status": "complete", "subscription": "sub_" + teamWS, "metadata": map[string]string{"workspace_id": teamWS}})
	row("team", "price_test_team", false)
	if got, err := svc.GetSubscription(ctx, teamWS); err != nil || got.Plan != "business_company" || !got.Subscribed {
		t.Fatalf("GetSubscription = %+v, %v; want subscribed, on Company (team's successor)", got, err)
	}

	// A company plan is never moved to a personal one: there is no allowance to prorate.
	if _, err := svc.ChangePlan(ctx, teamWS, "pro"); !errors.Is(err, billing.ErrPlanKindChange) {
		t.Fatalf("Team → Pro = %v; want ErrPlanKindChange", err)
	}

	// Add BYOK, Team's add-on: the BYOK Price as a second item, and Stripe's update sets byok.
	if w := do(http.MethodPost, "/v1/workspaces/"+teamWS+"/billing/subscription/byok", ""); w.Code != http.StatusOK {
		t.Fatalf("Add BYOK = %d %s", w.Code, w.Body.String())
	}
	if c := fake.calledTo("POST /v1/subscriptions/sub_" + teamWS); len(c) != 1 || c[0].form.Get("items[0][price]") != "price_test_byok" || c[0].form.Has("items[0][id]") {
		t.Fatalf("the add-on's Stripe update = %v; want a new item on price_test_byok", c)
	}
	deliver("customer.subscription.updated", subscription())
	row("team", "price_test_team", true)

	// Team → Business: the plan item moves and the add-on is dropped in the same update; Business includes BYOK.
	if _, err := svc.ChangePlan(ctx, teamWS, "business"); err != nil {
		t.Fatalf("Team → Business: %v", err)
	}
	up := fake.calledTo("POST /v1/subscriptions/sub_" + teamWS)
	if f := up[len(up)-1].form; f.Get("items[0][price]") != "price_test_business" || f.Get("items[1][deleted]") != "true" {
		t.Fatalf("the plan change's Stripe update = %v; want the plan item on price_test_business and the BYOK item deleted", f)
	}
	deliver("customer.subscription.updated", subscription())
	row("business", "price_test_business", true)

	// A subscription recorded before plans were named is named from its Price.
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id, status,
		livemode, last_event_at) VALUES ('s-b3210-old', 'sub_b3210_old', 'cus_old', 'price_test_pro', 'canceled', false, NOW())`); err != nil {
		t.Fatal(err)
	}
	var old string
	if _, err := svc.BackfillPlans(ctx); err != nil {
		t.Fatal(err)
	} else if err := pool.QueryRow(ctx, `SELECT plan FROM subscriptions WHERE stripe_subscription_id = 'sub_b3210_old'`).Scan(&old); err != nil || old != "pro" {
		t.Fatalf("the old subscription's plan = %q, %v; want pro, from price_test_pro", old, err)
	}

	// No subscription: Studio, a business's first plan (B61.57). The operator records a contract: Custom, with its fee,
	// in the trail.
	if plan, err := billing.PlanOf(ctx, pool, freeWS); err != nil || plan != "business_studio" {
		t.Fatalf("PlanOf(no subscription) = %q, %v; want Studio", plan, err)
	}
	if w := do(http.MethodPut, "/v1/admin/workspaces/"+freeWS+"/contract", `{"plan":"business_custom","platform_fee_bps":80,"reference":"ACME-2026-01"}`,
		moderatorOperatorHeader, "nicolai"); w.Code != http.StatusOK {
		t.Fatalf("the operator's contract = %d %s", w.Code, w.Body.String())
	}
	if plan, err := billing.PlanOf(ctx, pool, freeWS); err != nil || plan != "business_custom" {
		t.Fatalf("PlanOf(on a contract) = %q, %v; want Custom", plan, err)
	}
	if got, err := svc.GetSubscription(ctx, freeWS); err != nil || got.Plan != "business_custom" || got.Subscribed {
		t.Fatalf("GetSubscription(on a contract) = %+v, %v; want Custom, no Stripe subscription", got, err)
	}
	if c, err := billing.ContractOf(ctx, pool, freeWS); err != nil || c == nil || c.PlatformFeeBPS == nil || *c.PlatformFeeBPS != 80 || c.SetBy != "nicolai" {
		t.Fatalf("the contract = %+v, %v; want a platform fee of 80 bps, set by nicolai", c, err)
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail FROM operator_audit WHERE actor = 'nicolai' AND action = 'workspace.contract.set'
		AND target = $1`, "workspace:"+freeWS).Scan(&detail); err != nil || !bytes.Contains([]byte(detail), []byte(`"platform_fee_bps":80`)) {
		t.Fatalf("the operator audit row = %q, %v; want the contract set by nicolai, with its fee", detail, err)
	}
}
