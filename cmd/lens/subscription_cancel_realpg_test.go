package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// stripeSubAPI is Stripe test mode's subscription endpoint for one subscription: it applies the
// cancel_at_period_end a POST sends and answers with the subscription as it now stands.
type stripeSubAPI struct {
	mu  sync.Mutex
	sub map[string]any
}

func (s *stripeSubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if v := form.Get("cancel_at_period_end"); v != "" {
			s.sub["cancel_at_period_end"] = v == "true"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.sub)
}

func (s *stripeSubAPI) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.sub))
	for k, v := range s.sub {
		out[k] = v
	}
	return out
}

// B23.4 — on a deployment that sells only named plans (no single subscription price, as production is
// configured), a Plus subscriber cancels at period end and resumes through the cancel and resume routes.
// Each is asserted on the Stripe subscription's cancel_at_period_end and on the subscriptions row the
// customer.subscription.updated Stripe then sends records.
func TestSubscriptionCancelResume_PlanOnlyDeployment_PlusSubscriber(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, subID, secret = "ws-plus-cancel", "sub_plus_cancel", "whsec_b234"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-5 * 24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	fake := &stripeSubAPI{sub: map[string]any{
		"id": subID, "object": "subscription", "status": "active", "livemode": false,
		"customer":             map[string]any{"id": "cus_plus"},
		"cancel_at_period_end": false,
		"current_period_start": start.Unix(),
		"current_period_end":   end.Unix(),
		"metadata":             map[string]string{"workspace_id": ws},
		"items": map[string]any{"object": "list", "data": []any{map[string]any{
			"id": "si_plus", "object": "subscription_item", "quantity": 1,
			"price": map[string]any{"id": "price_plus", "object": "price", "currency": "usd", "unit_amount": 2000},
		}}},
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	// Wired as main.go wires a plan-only deployment: WithPlans, and no WithSubscriptions.
	svc := billing.New(pool, economy.NewDualTokenStore(nil, pool, nil), nil, secret).
		WithPlans(billing.NewLiveStripe("sk_test_b234", "", ""), map[string]string{"plus": "price_plus", "pro": "price_pro"})
	r := chi.NewRouter()
	subs := billReg{on: true}
	subs.post(r, "/v1/workspaces/{wsID}/billing/subscription/cancel", newSubscriptionCancelHandler(svc, true))
	subs.post(r, "/v1/workspaces/{wsID}/billing/subscription/resume", newSubscriptionCancelHandler(svc, false))

	// Stripe's event carrying the subscription as it stands in Stripe, delivered to the real webhook.
	t0 := time.Now().Add(-time.Minute)
	deliver := func(n int, eventType string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"id": "evt_b234_" + strconv.Itoa(n), "object": "event", "type": eventType,
			"created": t0.Add(time.Duration(n) * 10 * time.Second).Unix(), "livemode": true,
			"data": map[string]any{"object": fake.snapshot()},
		})
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(string(payload)))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		svc.HandleWebhook(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s webhook = %d %s", eventType, w.Code, w.Body.String())
		}
	}
	post := func(action string) billing.SubscriptionStatus {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/billing/subscription/"+action, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", action, w.Code, w.Body.String())
		}
		var st billing.SubscriptionStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	row := func() (status string, cancelAtEnd bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, cancel_at_period_end FROM subscriptions WHERE stripe_subscription_id = $1`,
			subID).Scan(&status, &cancelAtEnd); err != nil {
			t.Fatal(err)
		}
		return
	}

	deliver(0, "customer.subscription.created")
	if status, c := row(); status != "active" || c {
		t.Fatalf("subscribed row = %s cancel_at_period_end=%v; want active, false", status, c)
	}

	// Cancel: Stripe's subscription is set to cancel at period end, and the row records it once Stripe says so.
	if st := post("cancel"); !st.CancelAtPeriodEnd || !st.Subscribed {
		t.Errorf("cancel answered %+v; want cancel_at_period_end and still subscribed", st)
	}
	if fake.snapshot()["cancel_at_period_end"] != true {
		t.Fatalf("Stripe subscription cancel_at_period_end = %v after cancel; want true", fake.snapshot()["cancel_at_period_end"])
	}
	deliver(1, "customer.subscription.updated")
	if status, c := row(); status != "active" || !c {
		t.Errorf("row after cancel = %s cancel_at_period_end=%v; want active, true", status, c)
	}

	// Resume: Stripe's subscription renews again, and the row records it.
	if st := post("resume"); st.CancelAtPeriodEnd || !st.Subscribed {
		t.Errorf("resume answered %+v; want cancel_at_period_end cleared, subscribed", st)
	}
	if fake.snapshot()["cancel_at_period_end"] != false {
		t.Fatalf("Stripe subscription cancel_at_period_end = %v after resume; want false", fake.snapshot()["cancel_at_period_end"])
	}
	deliver(2, "customer.subscription.updated")
	if status, c := row(); status != "active" || c {
		t.Errorf("row after resume = %s cancel_at_period_end=%v; want active, false", status, c)
	}
}
