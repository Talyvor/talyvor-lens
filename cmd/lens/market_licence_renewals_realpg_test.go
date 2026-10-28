package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// B32.20 — over HTTP: a subscriber cancels (200: it renews no more and runs to its end; a licence that is not theirs is
// 404), and the tick at its end renews nothing. Another subscriber's marketplace bill fails: while Stripe will try
// again nothing changes; once Stripe gives up (invoice.payment_failed with no next attempt) the subscription is
// unpaid, still covers a use until its ends_at, and the tick at its end renews nothing.
func TestMarketLicences_CancelOverHTTPAndAGivenUpBillEndsTheSubscriptionUnpaid(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, canceller, defaulter, secret = "ws-renew-seller", "ws-renew-cancel", "ws-renew-default", "whsec_renewals"
	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetListingCharger(store)
	svc := billing.New(pool, bank, stripeFake, secret).WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	r.Post("/v1/billing/webhook", svc.HandleWebhook)
	call := func(ws, method, path, key, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings", "",
		`{"kind":"prompt","title":"Summariser","artifact":{"template":"Summarise {{text}}.","model":"gpt-5-mini"},"offers":[
			{"kind":"per_use","licence":"commercial","price_usd_micros":1000000},
			{"kind":"subscribe","licence":"commercial","price_usd_micros":20000000,"period_days":30}]}`)
	var l market.Listing
	if err := json.Unmarshal([]byte(body), &l); code != http.StatusCreated || err != nil {
		t.Fatalf("publish = %d %s", code, body)
	}
	subscribeOffer := ""
	for _, o := range l.Offers {
		if o.Kind == market.OfferSubscribe {
			subscribeOffer = o.ID
		}
	}
	subscribe := func(ws string) market.Licence {
		t.Helper()
		code, body := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/marketplace/listings/"+l.ID+"/licences", "sub-"+ws, `{"offer_id":"`+subscribeOffer+`"}`)
		var lic market.Licence
		if err := json.Unmarshal([]byte(body), &lic); code != http.StatusCreated || err != nil || !lic.AutoRenew || lic.EndsAt == nil {
			t.Fatalf("subscribe = %d %s; want 201 with a licence that renews", code, body)
		}
		return lic
	}
	renewals := func(licenceID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE licence_id = $1 AND use_kind = 'renewal'`, licenceID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A cancel: it runs to its end and renews no more.
	lic := subscribe(canceller)
	cancel := "/marketplace/licences/" + lic.ID + "/cancel"
	if code, body := call(defaulter, http.MethodPost, "/v1/workspaces/"+defaulter+cancel, "", ""); code != http.StatusNotFound {
		t.Fatalf("another workspace cancelling it = %d %s; want 404", code, body)
	}
	code, body = call(canceller, http.MethodPost, "/v1/workspaces/"+canceller+cancel, "", "")
	var cancelled market.Licence
	if err := json.Unmarshal([]byte(body), &cancelled); code != http.StatusOK || err != nil || cancelled.AutoRenew ||
		cancelled.Status != market.LicenceActive || !cancelled.EndsAt.Equal(*lic.EndsAt) {
		t.Fatalf("cancel = %d %s; want 200, active to its end and renewing no more", code, body)
	}
	if res, err := bank.RunAgentSchedules(ctx, lic.EndsAt.Add(time.Minute)); err != nil || res.Renewed != 0 || renewals(lic.ID) != 0 {
		t.Fatalf("the tick at the cancelled subscription's end = %+v, %v, %d renewals; want none", res, err, renewals(lic.ID))
	}

	// A bill Stripe gives up on: the subscription ends unpaid at its ends_at.
	owed := subscribe(defaulter)
	var subscription string
	if err := pool.QueryRow(ctx, `SELECT stripe_subscription_id FROM market_bills WHERE workspace_id = $1`, defaulter).Scan(&subscription); err != nil {
		t.Fatalf("the subscriber's marketplace bill: %v", err)
	}
	failed := func(event string, next any) {
		t.Helper()
		obj := map[string]any{"id": "in_" + event, "object": "invoice", "subscription": subscription, "next_payment_attempt": next,
			"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_" + event, "object": "line_item",
				"period": map[string]any{"start": owed.StartsAt.Add(-time.Hour).Unix(), "end": owed.StartsAt.Add(time.Hour).Unix()},
				"price":  map[string]any{"id": "price_market", "object": "price"}}}}}
		raw, _ := json.Marshal(map[string]any{"id": "evt_" + event, "object": "event", "type": "invoice.payment_failed",
			"created": time.Now().Unix(), "livemode": true, "data": map[string]any{"object": obj}})
		now := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(webhook.ComputeSignature(now, raw, secret))))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("invoice.payment_failed = %d %s", w.Code, w.Body.String())
		}
	}
	licence := func() (status string, renews bool, endsAt time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, auto_renew, ends_at FROM market_licences WHERE id = $1`, owed.ID).Scan(&status, &renews, &endsAt); err != nil {
			t.Fatal(err)
		}
		return
	}
	failed("retried", time.Now().Add(72*time.Hour).Unix())
	if status, renews, _ := licence(); status != market.LicenceActive || !renews {
		t.Fatalf("after a failure Stripe will retry, the licence = %s, renews %v; want it unchanged", status, renews)
	}
	failed("given_up", nil)
	if status, renews, endsAt := licence(); status != market.LicenceUnpaid || renews || !endsAt.Equal(*owed.EndsAt) {
		t.Fatalf("after Stripe gave up, the licence = %s, renews %v, ends %v; want unpaid at its ends_at %v", status, renews, endsAt, owed.EndsAt)
	}
	code, body = call(defaulter, http.MethodPost, "/v1/workspaces/"+defaulter+"/marketplace/listings/"+l.ID+"/use", "", `{"variables":{"text":"Q3"}}`)
	var u market.Use
	if err := json.Unmarshal([]byte(body), &u); code != http.StatusOK || err != nil || u.Charge != market.ChargeLicensed || u.LicenceID != owed.ID {
		t.Fatalf("a use before its end = %d %s; want it still licensed", code, body)
	}
	if res, err := bank.RunAgentSchedules(ctx, owed.EndsAt.Add(time.Minute)); err != nil || res.Renewed != 0 || renewals(owed.ID) != 0 {
		t.Fatalf("the tick at the unpaid subscription's end = %+v, %v, %d renewals; want none", res, err, renewals(owed.ID))
	}
}
