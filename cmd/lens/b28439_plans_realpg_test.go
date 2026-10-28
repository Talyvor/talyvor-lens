package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
)

// b28439Fees are the plan Prices the fake Stripe account holds, in US cents.
var b28439Fees = map[string]int64{"price_plus": 2000, "price_pro": 10000, "price_max": 20000,
	"price_team": 4900, "price_business": 29900, "price_byok": 19900}

// b28439Stripe answers GET /v1/prices/{id} with the Price's amount, as Stripe does.
type b28439Stripe struct{}

func (b28439Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/prices/")
	fee, ok := b28439Fees[id]
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"not in this fake"}}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "price", "active": true, "currency": "usd", "unit_amount": fee})
}

// B28.439 — GET /v1/billing/plans, with no credential, tells a visitor each plan's price and the usage it
// includes this month; and that figure is exactly the allowance a new subscriber to the plan is granted —
// read here off the subscription_allowance row their customer.subscription.created writes.
func TestB28439_ThePublicPlansReadIsWhatANewSubscriberIsGranted(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const secret, key = "whsec_b28439", "sk_test_b28439"
	srv := httptest.NewServer(b28439Stripe{})
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	liveStripe := billing.NewLiveStripe(key, "", "")
	svc := billing.New(pool, economy.NewDualTokenStore(nil, pool, nil), liveStripe, secret).
		WithPlans(liveStripe, map[string]string{"plus": "price_plus", "pro": "price_pro", "max": "price_max", "byok": "price_byok"})
	b := newBillingRouter(svc, nil, func(string) bool { return false }, key, "", "")
	r := chi.NewRouter()
	r.Get("/v1/billing/plans", newPlansHandler(b, billing.EnterpriseFromUSDCentsDefault))
	r.Post("/v1/billing/webhook", svc.HandleWebhook)

	// A visitor, with no credential, reads the plans.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/billing/plans", nil))
	var got struct{ Plans []billing.Plan }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("GET /v1/billing/plans = %d %s", w.Code, w.Body.String())
	}
	// At h = 0 — no pooled traffic measured — D is 90% of the fee net of Stripe's 2.9% + 0.7% + 30¢ (B32.13):
	// about 171, 865 and 1,733 LXC.
	want := []billing.Plan{
		{ID: "plus", USDCents: 2000, IncludedULXC: 170_820_000},
		{ID: "pro", USDCents: 10000, IncludedULXC: 864_900_000},
		{ID: "max", USDCents: 20000, IncludedULXC: 1_732_500_000},
	}
	if len(got.Plans) != len(want) {
		t.Fatalf("plans = %+v; want plus, pro and max (BYOK includes no usage)", got.Plans)
	}
	for i := range want {
		if got.Plans[i] != want[i] {
			t.Errorf("plan %d = %+v; want %+v", i, got.Plans[i], want[i])
		}
	}

	// A new subscriber to each plan this month is granted exactly that figure.
	now := time.Now().UTC().Truncate(time.Second)
	for _, plan := range got.Plans {
		ws, sub := "ws-b28439-"+plan.ID, "sub_b28439_"+plan.ID
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
			t.Fatal(err)
		}
		object := map[string]any{"id": sub, "object": "subscription", "status": "active", "livemode": false,
			"customer": "cus_" + ws, "cancel_at_period_end": false,
			"current_period_start": now.Unix(), "current_period_end": now.AddDate(0, 1, 0).Unix(),
			"metadata": map[string]string{"workspace_id": ws},
			"items": map[string]any{"object": "list", "data": []any{map[string]any{"id": "si_" + ws, "object": "subscription_item", "quantity": 1,
				"price": map[string]any{"id": "price_" + plan.ID, "object": "price", "currency": "usd", "unit_amount": plan.USDCents}}}}}
		payload, _ := json.Marshal(map[string]any{"id": "evt_" + sub, "object": "event", "type": "customer.subscription.created",
			"created": now.Unix(), "livemode": true, "data": map[string]any{"object": object}})
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(string(payload)))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s's customer.subscription.created = %d %s", plan.ID, w.Code, w.Body.String())
		}
		var granted, fee int64
		if err := pool.QueryRow(ctx, `SELECT granted_ulxc, fee_usd_cents FROM subscription_allowance WHERE stripe_subscription_id = $1`,
			sub).Scan(&granted, &fee); err != nil {
			t.Fatalf("%s's allowance row: %v", plan.ID, err)
		}
		if granted != plan.IncludedULXC || fee != plan.USDCents {
			t.Errorf("a new %s subscriber was granted %d µLXC for %d cents; the plans read said %d µLXC for %d cents",
				plan.ID, granted, fee, plan.IncludedULXC, plan.USDCents)
		}
	}
}
