package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/billing"
)

// billing_routes.go — U18b FIAT billing surface gate.
//
// billReg mirrors econReg but is gated on cfg.BillingEnabled, INDEPENDENT of the
// U3 economy master switch: billing is fiat (Stripe → LXC credit), so it must be
// registrable with the economy OFF (a pure fiat-SaaS deployment runs
// EconomyEnabled=false + BillingEnabled=true). When billing is off the routes are
// never registered → chi-native 404. Billing routes are NOT economy: they must
// NOT be registered through econReg, and must stay OUT of the economy manifest.
type billReg struct{ on bool }

func (b billReg) get(r chi.Router, pattern string, h http.HandlerFunc) {
	if b.on {
		r.Get(pattern, h)
	}
}

func (b billReg) post(r chi.Router, pattern string, h http.HandlerFunc) {
	if b.on {
		r.Post(pattern, h)
	}
}

func (b billReg) delete(r chi.Router, pattern string, h http.HandlerFunc) {
	if b.on {
		r.Delete(pattern, h)
	}
}

// purchaseLister is the read surface the admin purchases handler needs (satisfied
// by *billing.Service). Extracted to package level so the admin gate is provable
// over HTTP, per the #153 testability pattern.
type purchaseLister interface {
	ListPurchases(ctx context.Context, limit int) ([]billing.Purchase, error)
}

// newBillingPurchasesHandler — GET /v1/admin/billing/purchases. Read-only list of
// lxc_purchases, newest first. An 'anomalous' row means the customer was CHARGED
// and NOT credited (v1 resolution = manual Stripe-dashboard refund). Admin-gated
// at the route (requireAdmin) and registered only under BillingEnabled (billReg).
func newBillingPurchasesHandler(svc purchaseLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		rows, err := svc.ListPurchases(req.Context(), 200)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, rows)
	}
}

// subscriptionCanceller is the cancel/resume surface (satisfied by *billing.Service). Package level for
// the same #153 reason: the route is provable over HTTP against a real Service (B23.4).
type subscriptionCanceller interface {
	SetCancelAtPeriodEnd(ctx context.Context, workspaceID string, cancel bool) (*billing.SubscriptionStatus, error)
}

// newSubscriptionCancelHandler — POST /v1/workspaces/{wsID}/billing/subscription/cancel (cancel=true)
// and …/resume (cancel=false). B1.5. Cancel is AT PERIOD END: the workspace keeps what it paid for and
// flips to unsubscribed when Stripe's .deleted arrives. Neither route writes the subscriptions row — the
// webhook that follows does.
func newSubscriptionCancelHandler(svc subscriptionCanceller, cancel bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		st, err := svc.SetCancelAtPeriodEnd(req.Context(), wsID, cancel)
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, billing.ErrNoSubscriptionPrice):
				status = http.StatusNotImplemented
			case errors.Is(err, billing.ErrNoLiveSubscription):
				status = http.StatusConflict
			}
			writeJSONErr(w, status, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, st)
	}
}

// newBYOKAddonHandler — POST (on) and DELETE (off) /v1/workspaces/{wsID}/billing/subscription/byok: BYOK, Team's
// add-on (B32.10), as a second item of the live Team subscription. Neither writes the subscriptions row — the
// webhook that follows sets byok.
func newBYOKAddonHandler(svc *billing.Service, on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		st, err := svc.SetBYOKAddon(req.Context(), chi.URLParam(req, "wsID"), on)
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, billing.ErrNoSubscriptionPrice):
				status = http.StatusNotImplemented
			case errors.Is(err, billing.ErrNoLiveSubscription), errors.Is(err, billing.ErrBYOKAddon), errors.Is(err, billing.ErrBYOKAddonUnchanged):
				status = http.StatusConflict
			}
			writeJSONErr(w, status, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, st)
	}
}

// backfillPlans names the plan of the subscriptions recorded before B32.10, from the Prices svc sells.
func backfillPlans(ctx context.Context, svc *billing.Service, which string) {
	n, err := svc.BackfillPlans(ctx)
	if err != nil {
		slog.Error("billing: the "+which+" subscriptions' plans could not be backfilled", "err", err)
		return
	}
	if n > 0 {
		slog.Info("billing: named the plan of "+which+" subscriptions recorded before B32.10", "subscriptions", n)
	}
}

// billingRouter picks the billing Service that takes a workspace's money (B25.2). A test (synthetic)
// workspace pays through Stripe TEST MODE, always — test, a Service with the test-mode key, webhook secret
// and plans — and every other workspace through live, the Service there was before. With no test-mode
// Service a test workspace pays through live when live's own key is a test-mode one (B17.21: it is Stripe
// test mode already); otherwise it cannot pay, and the refusal names the variable that is unset. B50.13: while
// live's key is a test-mode one, a real workspace cannot pay at all — test-mode money credits only a test one.
type billingRouter struct {
	live, test *billing.Service
	isTest     func(wsID string) bool
	unset      string // the test-mode variables not set, when test is nil
	cardsOff   bool   // live's key is not a live one: a real workspace's payment is refused
}

func newBillingRouter(live, test *billing.Service, isTest func(string) bool, liveKey, testKey, testSecret string) billingRouter {
	if test == nil && liveKey != "" && !billing.LiveKey(liveKey) {
		test = live
	}
	var unset []string
	if testKey == "" {
		unset = append(unset, "LENS_STRIPE_TEST_SECRET_KEY")
	}
	if testSecret == "" {
		unset = append(unset, "LENS_STRIPE_TEST_WEBHOOK_SECRET")
	}
	return billingRouter{live: live, test: test, isTest: isTest, unset: strings.Join(unset, " and "), cardsOff: !billing.LiveKey(liveKey)}
}

// pay is the Service for a payment by wsID, or false with the refusal written.
func (b billingRouter) pay(w http.ResponseWriter, wsID string) (*billing.Service, bool) {
	if !b.isTest(wsID) {
		if b.cardsOff {
			writeJSONErr(w, http.StatusForbidden, "card payments are not switched on")
			return nil, false
		}
		return b.live, true
	}
	if b.test == nil {
		writeJSONErr(w, http.StatusForbidden, "a test workspace pays only through Stripe test mode, and "+b.unset+" is not set")
		return nil, false
	}
	return b.test, true
}

// sellsSubscriptions reports whether either Service sells a subscription — whether the subscription routes
// are registered at all.
func (b billingRouter) sellsSubscriptions() bool {
	return b.live.SellsSubscriptions() || (b.test != nil && b.test.SellsSubscriptions())
}

// onSubscriptions runs h with the Service that sells wsID its subscription. A workspace whose Service sells
// none is answered 404, as these routes answered before a test-mode Service could sell one — except that a
// test workspace asking to pay (checkout) is told which variable is unset.
func (b billingRouter) onSubscriptions(checkout bool, h func(svc *billing.Service) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		svc := b.live
		if checkout {
			var ok bool
			if svc, ok = b.pay(w, wsID); !ok {
				return
			}
			if b.isTest(wsID) && !svc.SellsSubscriptions() {
				writeJSONErr(w, http.StatusForbidden,
					"a test workspace subscribes only to Stripe test-mode plans, and LENS_STRIPE_TEST_SUBSCRIPTION_PLANS is not set")
				return
			}
		} else if b.isTest(wsID) {
			if svc = b.test; svc == nil {
				http.NotFound(w, req)
				return
			}
		}
		if !svc.SellsSubscriptions() {
			http.NotFound(w, req)
			return
		}
		h(svc)(w, req)
	}
}

// newCheckoutHandler — POST /v1/workspaces/{wsID}/billing/checkout {"usd_cents":…}: a one-off top-up's
// Stripe Checkout URL, from the Service that takes the workspace's money.
func newCheckoutHandler(b billingRouter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		svc, ok := b.pay(w, wsID)
		if !ok {
			return
		}
		var in struct {
			USDCents int64 `json:"usd_cents"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		url, err := svc.CreateCheckout(req.Context(), wsID, in.USDCents)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, billing.ErrAmountNotAllowed) {
				status = http.StatusBadRequest
			}
			writeJSONErr(w, status, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]string{"url": url})
	}
}

// newSubscribeHandler — POST /v1/workspaces/{wsID}/billing/subscribe {"plan":"plus"|"pro"|"max"}: the
// subscription checkout (MODEL 2, W4.6.1 step 1; plans B13.1). An empty body is the single configured price.
func newSubscribeHandler(svc *billing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		var body struct {
			Plan string `json:"plan"`
		}
		if req.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(req.Body, 1<<10)).Decode(&body); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "body must be {\"plan\": \"<name>\"}")
				return
			}
		}
		url, err := svc.CreatePlanCheckout(req.Context(), wsID, body.Plan)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, billing.ErrNoSubscriptionPrice) {
				// A capability this deployment does not have — not a fault.
				status = http.StatusNotImplemented
			} else if errors.Is(err, billing.ErrUnknownPlan) {
				status = http.StatusBadRequest
			}
			writeJSONErr(w, status, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]string{"url": url})
	}
}

// newPlansHandler — GET /v1/billing/plans (B28.439): public and credential-free, each plan's id, the price
// Stripe bills it at (usd_cents) and the usage it includes this month (included_ulxc) — what a new
// subscriber to it is granted. Registered only where subscriptions are sold; it describes the plans the real
// workspaces' Service sells, or the test-mode Service's on a deployment that sells only those. B32.77: beside
// them, Team's and Business's prices (company_plans), the BYOK add-on's (byok_add_on_usd_cents, absent when
// BYOK is not sold) and what Enterprise costs from (enterprise_from_usd_cents, LENS_ENTERPRISE_FROM_USD_CENTS).
func newPlansHandler(b billingRouter, enterpriseFromUSDCents int64) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		svc := b.live
		if !svc.SellsSubscriptions() && b.test != nil {
			svc = b.test
		}
		plans, err := svc.Plans(req.Context(), time.Now())
		var company []billing.PlanPrice
		var byok int64
		if err == nil {
			company, byok, err = svc.CompanyPrices(req.Context())
		}
		if err != nil {
			slog.Error("billing: the public plans read failed", "err", err)
			writeJSONErr(w, http.StatusBadGateway, "the plans could not be read")
			return
		}
		body := map[string]any{"plans": plans, "company_plans": company, "enterprise_from_usd_cents": enterpriseFromUSDCents}
		if byok > 0 {
			body["byok_add_on_usd_cents"] = byok
		}
		writeJSONOK(w, http.StatusOK, body)
	}
}
