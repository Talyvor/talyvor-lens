// Package billing owns ALL Stripe interaction for U18b: Checkout Session
// creation, webhook signature verification, and event handling that credits the
// fiat-pegged LXC usage credit. It calls economy.DualTokenStore.CreditLXCTx for
// the credit and writes its OWN tables (lxc_purchases, billing_customers); it
// never writes the LXC ledger (lxc_ledger/lxc_balances) directly.
//
// Billing is FIAT, independent of the U3 economy master switch. Money is integer
// USD cents; LXC is ALWAYS recomputed server-side from cents at the fixed peg
// (economy.LXCUSDValue), never trusted from Stripe session metadata.
package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/economy"
)

// maxWebhookBody caps the raw webhook body (Stripe events are small; this is a
// DoS guard). The full body is still needed to verify the signature.
const maxWebhookBody = 1 << 20 // 1 MiB

// A top-up may be ANY whole number of USD cents in [minTopUpCents, maxTopUpCents]. B5.1.
//
// MINIMUM $10. Stripe takes 2.9% + 30¢ of every card payment, and a credit is sold
// at face value (1 credit = $0.10), so the fee comes out of our side: 59¢ (5.9%) at
// $10, 44.5¢ (8.9%) at $5, 33¢ (33%) at $1. $10 keeps it under 6%, and it is where
// the smallest fixed size already sat, so the floor does not move.
//
// MAXIMUM $10,000. A card payment stays disputable for months, and credits spent
// before a chargeback cannot be taken back, so this is the most one stolen or
// disputed card can cost. A team spending more tops up more than once. (Stripe's
// own ceiling for a single USD payment is $999,999.99.)
//
// ⚠ THE BOUNDS MAY ONLY WIDEN while a checkout session may still be in flight:
// async payment methods can settle DAYS after session creation, and the webhook
// re-checks these bounds, so narrowing them would mark a legitimately-PAID purchase
// anomalous (charged, not credited). The old fixed sizes $10/$50/$100 all sit
// inside the range, so sessions created before B5.1 still credit.
const (
	minTopUpCents int64 = 1_000
	maxTopUpCents int64 = 1_000_000
)

// topUpPresets are the sizes the checkout UI offers as one-click buttons. Any
// amount in bounds is accepted; these are a suggestion, not a rule.
var topUpPresets = []int64{1000, 5000, 10000}

// ErrAmountNotAllowed is returned by CreateCheckout for an out-of-bounds amount.
var ErrAmountNotAllowed = fmt.Errorf("billing: usd_cents must be a whole number of cents from %d to %d", minTopUpCents, maxTopUpCents)

func topUpInBounds(usdCents int64) bool {
	return usdCents >= minTopUpCents && usdCents <= maxTopUpCents
}

// lxcCrediter is the LXC-credit surface billing needs — satisfied by
// *economy.DualTokenStore. Billing passes its OWN tx so the idempotency claim and
// the credit commit atomically.
type lxcCrediter interface {
	CreditLXCTx(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error)
}

// stripeAPI abstracts the live Stripe calls (customer + checkout session) so the
// HTTP handlers test offline. Webhook verification uses the real
// webhook.ConstructEvent (pure crypto), not this interface.
type stripeAPI interface {
	CreateCustomer(ctx context.Context, workspaceID string) (customerID string, err error)
	CreateCheckoutSession(ctx context.Context, p CheckoutParams) (url string, sessionID string, err error)
	// CardFingerprint returns the stable per-card fingerprint for a payment
	// intent (Stripe's card.fingerprint), "" when none is available. U6 PR2
	// owner-linkage; called best-effort AFTER the credit commits.
	CardFingerprint(ctx context.Context, paymentIntentID string) (fingerprint string, err error)
}

// CheckoutParams is the price-true input to a Checkout Session: cents is
// authoritative, lxc is the peg recomputation, both flow into session metadata.
type CheckoutParams struct {
	WorkspaceID string
	CustomerID  string
	USDCents    int64
	LXCAmount   int64 // µLXC (SEC-2)
}

// Service is the billing core. Construct with New.
type Service struct {
	pool          *pgxpool.Pool
	credits       lxcCrediter
	stripe        stripeAPI
	webhookSecret string
	wsExists      func(ctx context.Context, workspaceID string) (bool, error)
	log           *slog.Logger

	// MODEL 2 (W4.6.1 step 1). Both are OPTIONAL: a deployment with no subscription
	// price configured serves the one-off top-up surface exactly as before, and
	// CreateSubscriptionCheckout refuses with ErrNoSubscriptionPrice rather than
	// calling Stripe with an empty price. See subscriptions.go.
	subStripe subscriptionAPI
	subPrice  string
	subPlans  map[string]string // B13.1: plan name → Stripe Price id
	planFees  sync.Map          // B28.439: Stripe Price id → its USD cents (plans.go)
	// B13.2 — earnings off the next bill (bill_credit.go). Both nil ⇒ invoice.created is ignored.
	billLens     lensDebiter
	billInvoices invoiceCrediter

	// B20.2 — the buyers' monthly marketplace bills (market_bill.go). Unset ⇒ paid uses cannot be metered.
	marketStripe  marketStripeAPI
	marketPrice   string
	marketEvent   string
	marketClearer MarketClearer
	// B32.39 — each billed use's tax, metered as its own line (market_bill.go). Unset ⇒ a use with tax cannot be billed.
	marketTaxEvent string
	marketTaxPrice string
	// B20.5 — seller payouts (connect.go). Unset ⇒ disputes are acknowledged and ignored.
	connectStripe connectStripeAPI
	marketPayouts MarketPayouts
	// B19.25 — agent card purchases settle (agent_cards.go). Unset ⇒ Issuing events are acknowledged and ignored.
	agentCards AgentCardSettler
	// B30.20 — an invoice's card payment posts to the issuing agent (invoices.go). Unset ⇒ acked and logged.
	invoices InvoicePayer
	// B22.4 — company credit lines are invoiced monthly (credit_line.go). Unset ⇒ their draws are not invoiced.
	creditLineStripe creditLineStripeAPI
	// B26.4 — set, a test company's credit line is refused, naming these test-mode variables (RefuseTestCreditLines).
	creditLineTestUnset string

	// B25.2 — whose money this Service takes (ForTestWorkspaces). nil takes every workspace's.
	testWorkspaces *bool

	// D, in µLXC — the Model 2 allowance per billing period (W4.6.1 step 2).
	// ZERO is the default and means "no allowance configured": no grant row is ever
	// written and Consume covers nothing. See allowance.go.
	allowanceULXC int64
}

// WithSubscriptions enables the subscription surface. Separate from New so every
// existing caller and test double is unchanged — a Service without it is exactly the
// Service that shipped before, which is what keeps this merge to one feature.
func (s *Service) WithSubscriptions(api subscriptionAPI, priceID string) *Service {
	s.subStripe = api
	s.subPrice = priceID
	return s
}

// WithPlans offers named plans (B13.1: plus, pro, max → their Stripe Price ids) at checkout.
func (s *Service) WithPlans(api subscriptionAPI, plans map[string]string) *Service {
	s.subStripe = api
	s.subPlans = plans
	return s
}

// ForTestWorkspaces makes this Service take the money of test (synthetic) workspaces only, or of real ones only
// (B25.2). A test workspace always pays through Stripe TEST MODE — a Service of its own with the test-mode
// key, webhook secret and plans — so each Service's webhook ignores an event about the other kind of
// workspace: it is the other Service's. Before the live switch both webhooks hear every event of the one
// Stripe account, and each takes only its own; after it, a test-mode payment for a real workspace credits
// nothing.
func (s *Service) ForTestWorkspaces(test bool) *Service {
	s.testWorkspaces = &test
	return s
}

// takes reports whether this Service takes wsID's money from an event Stripe signed with livemode. A workspace
// with no row counts as real, as it does in workspace.CheckMoneyWall, so the live Service still records it as
// unknown_workspace. B50.13: money Stripe confirmed in test mode is taken for a synthetic workspace only — a
// real workspace is never credited, put on a plan or paid from it, whichever key the Service holds.
func (s *Service) takes(ctx context.Context, wsID string, livemode bool) (bool, error) {
	if wsID == "" || (s.testWorkspaces == nil && livemode) {
		return true, nil
	}
	var synthetic bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`,
		wsID).Scan(&synthetic); err != nil {
		return false, err
	}
	if !livemode && !synthetic {
		s.log.Warn("billing webhook: a test-mode event for a real workspace — ignored, nothing credited (B50.13)", "workspace", wsID)
		return false, nil
	}
	return s.testWorkspaces == nil || synthetic == *s.testWorkspaces, nil
}

// testMode reports whether this is the test workspaces' Service.
func (s *Service) testMode() bool { return s.testWorkspaces != nil && *s.testWorkspaces }

// New builds a Service. wsExists defaults to a workspaces-table lookup.
func New(pool *pgxpool.Pool, credits lxcCrediter, sapi stripeAPI, webhookSecret string) *Service {
	return &Service{
		pool:          pool,
		credits:       credits,
		stripe:        sapi,
		webhookSecret: webhookSecret,
		wsExists:      defaultWorkspaceExists(pool),
		log:           slog.Default(),
	}
}

func defaultWorkspaceExists(pool *pgxpool.Pool) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, wsID string) (bool, error) {
		var one int
		err := pool.QueryRow(ctx, `SELECT 1 FROM workspaces WHERE id = $1`, wsID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
}

// lxcForCents recomputes LXC from USD cents at the fixed peg — the SINGLE price
// truth, in µLXC (SEC-2). No LXC amount is hardcoded anywhere else in billing.
//
// ⚠ INTEGER ARITHMETIC, NOT FLOAT. The float form this replaced,
// floor(cents/100/0.10*1e6), under-credited by 1 µLXC on 386,523 of the first
// million cent amounts (1¢ → 99,999 instead of 100,000). It was exact only for
// the three fixed sizes, which is why nothing noticed until any amount was allowed.
func lxcForCents(usdCents int64) int64 {
	return usdCents * ulxcPerCent
}

// ulxcPerCent is how many µLXC one US cent buys at the peg: $0.01 / $0.10 × 1e6.
// Rounded once, here, from the peg constant — 100,000 at $0.10.
var ulxcPerCent = int64(math.Round(0.01 / economy.LXCUSDValue * 1e6))

// TopUpPresetCents returns the one-click sizes the checkout UI offers.
func TopUpPresetCents() []int64 { return append([]int64(nil), topUpPresets...) }

// CreateCheckout validates the amount, recomputes LXC at the peg, ensures the
// Stripe customer mapping, and creates a Checkout Session. Returns the session
// URL. An out-of-bounds amount yields ErrAmountNotAllowed (the caller maps it to 400).
func (s *Service) CreateCheckout(ctx context.Context, workspaceID string, usdCents int64) (string, error) {
	if !topUpInBounds(usdCents) {
		return "", ErrAmountNotAllowed
	}
	lxc := lxcForCents(usdCents)
	customerID, err := s.ensureCustomer(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("billing: ensure customer: %w", err)
	}
	url, _, err := s.stripe.CreateCheckoutSession(ctx, CheckoutParams{
		WorkspaceID: workspaceID,
		CustomerID:  customerID,
		USDCents:    usdCents,
		LXCAmount:   lxc,
	})
	if err != nil {
		return "", fmt.Errorf("billing: create checkout session: %w", err)
	}
	return url, nil
}

// ensureCustomer returns the workspace's Stripe customer id, creating + persisting
// the mapping on first use. Race note: two concurrent first-time checkouts may
// each create a Stripe customer; the DB INSERT ON CONFLICT keeps ONE mapping and
// the re-read returns the persisted (winning) id, so the credited path is always
// consistent — at worst an unused Stripe customer object is orphaned (no money).
func (s *Service) ensureCustomer(ctx context.Context, workspaceID string) (string, error) {
	var custID string
	err := s.pool.QueryRow(ctx,
		`SELECT stripe_customer_id FROM billing_customers WHERE workspace_id = $1`, workspaceID).Scan(&custID)
	if err == nil {
		return custID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	created, err := s.stripe.CreateCustomer(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO billing_customers (workspace_id, stripe_customer_id)
		 VALUES ($1, $2) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID, created); err != nil {
		return "", err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT stripe_customer_id FROM billing_customers WHERE workspace_id = $1`, workspaceID).Scan(&custID); err != nil {
		return "", err
	}
	return custID, nil
}

// HandleWebhook is the public POST /v1/billing/webhook handler. It reads the RAW
// body (Stripe signs raw bytes) BEFORE any JSON, verifies the signature, and
// dispatches. It returns 200 ONLY when the event's outcome is durably recorded
// (credited / anomalous-claimed / refund-marked / deliberately ignored); a DB or
// credit failure returns 5xx so Stripe redelivers (the claim is durable only on
// commit, so a retry legitimately re-processes).
func (s *Service) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		s.log.Error("billing webhook: body read failed", "err", err)
		http.Error(w, "read error", http.StatusServiceUnavailable) // retry
		return
	}
	// Verify the signature + timestamp, but TOLERATE API-version skew: real Stripe
	// events carry the account's API version, which may differ from the version
	// stripe-go/v81 pins — rejecting on that would 400 every legitimate webhook.
	// The handler reads only version-stable fields (id, type, amount_total,
	// currency, payment_status, metadata, payment_intent).
	event, err := webhook.ConstructEventWithOptions(raw, r.Header.Get("Stripe-Signature"), s.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		// Bad signature → 400, NO body echo, ZERO db writes. (Do not log the body.)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		s.handleSessionCredit(w, r.Context(), &event)
	case "charge.refunded":
		if s.reverseMarketCharge(w, r.Context(), &event) { // B20.5 — a refunded marketplace bill reverses its sellers' earnings
			s.handleRefund(w, r.Context(), &event)
		}
	case "charge.dispute.created": // B20.5 — a charged-back marketplace bill reverses its sellers' earnings
		s.handleDispute(w, r.Context(), &event)
	case "checkout.session.async_payment_failed":
		w.WriteHeader(http.StatusOK) // no money moved → ack, no action
	// MODEL 2 (W4.6.1 step 1) — the recurring half. See subscriptions.go.
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		s.handleSubscription(w, r.Context(), &event)
	case "invoice.payment_failed":
		s.handleInvoicePaymentFailed(w, r.Context(), &event)
	case "invoice.created": // B13.2 — a renewal's draft: the moment earnings can come off it
		s.handleInvoiceCreated(w, r.Context(), &event)
	case "invoice.paid": // B20.2 — a buyer paid their marketplace bill: its uses clear and their sellers earn
		s.handleInvoicePaid(w, r.Context(), &event)
	case "issuing_authorization.updated", "issuing_transaction.created": // B19.25 — an agent card purchase settles
		s.handleAgentCardSettlement(w, r.Context(), &event)
	default:
		w.WriteHeader(http.StatusOK) // unhandled type → ack, no action
	}
}

func (s *Service) handleSessionCredit(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var sess stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		// Signed but unparseable — it will never parse; ack so Stripe stops retrying.
		s.log.Warn("billing webhook: unparseable checkout session", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// B30.20 — an invoice's card payment is the issuing agent's, whatever kind of workspace it is.
	if sess.Metadata["talyvor_invoice_id"] != "" {
		s.handleInvoiceCard(w, ctx, event, &sess)
		return
	}

	// B7.1 — ROUTE BY SESSION MODE FIRST. A subscription checkout carries no
	// lxc_amount, so on the top-up path below it classified as amount_mismatch and
	// every new subscriber landed on the admin list as "charged and NOT credited".
	// Only `payment` (and a mode-less legacy payload) is a top-up.
	if sess.Mode == stripe.CheckoutSessionModeSubscription {
		s.handleSubscriptionCheckout(w, ctx, event, &sess)
		return
	}

	// Delayed payment methods fire checkout.session.completed with
	// payment_status != "paid" FIRST, then a SEPARATE
	// async_payment_succeeded (different event id) when money settles. Credit ONLY
	// on paid, and do NOT consume an idempotency id for an unpaid completed — the
	// async event is the one that credits.
	if event.Type == "checkout.session.completed" && sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
		w.WriteHeader(http.StatusOK)
		return
	}

	wsID := sess.Metadata["workspace_id"]
	if mine, err := s.takes(ctx, wsID, event.Livemode); err != nil {
		s.fail(w, "workspace kind", event.ID, err)
		return
	} else if !mine {
		s.log.Info("billing webhook: a payment for the other kind of workspace — test and real money are kept apart",
			"event", event.ID, "session", sess.ID, "workspace", wsID)
		w.WriteHeader(http.StatusOK)
		return
	}
	metaLXC, _ := strconv.ParseInt(sess.Metadata["lxc_amount"], 10, 64) // µLXC (SEC-2)
	// B22.2 — ADAPTIVE PRICING. A customer shown their own currency paid sess.Currency / sess.AmountTotal,
	// and Stripe converted it; what they bought is the session's SOURCE amount, in USD, which Stripe reports
	// in currency_conversion. Lens reads Stripe's USD figure and converts nothing. A session with no
	// conversion is judged on its own currency, so one created in anything but USD stays anomalous.
	currency, usdCents := string(sess.Currency), sess.AmountTotal
	var paid map[string]interface{}
	if cc := sess.CurrencyConversion; cc != nil && cc.SourceCurrency != "" {
		paid = map[string]interface{}{"paid_currency": string(sess.Currency), "paid_amount": sess.AmountTotal, "stripe_fx_rate": cc.FxRate}
		currency, usdCents = string(cc.SourceCurrency), cc.AmountTotal
	}
	recomp := lxcForCents(usdCents)

	anomaly, rerr := s.classify(ctx, currency, usdCents, wsID, recomp, metaLXC)
	if rerr != nil {
		// Could not VERIFY (e.g. workspace lookup failed) — do not guess; retry.
		s.fail(w, "classify", event.ID, rerr)
		return
	}

	pi := ""
	if sess.PaymentIntent != nil {
		pi = sess.PaymentIntent.ID
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.fail(w, "begin", event.ID, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	status, creditAmt := "completed", recomp
	if anomaly != "" {
		status, creditAmt = "anomalous", 0
	}

	// ⚠ HAS THIS PAYMENT ALREADY BEEN REFUNDED? Read INSIDE the transaction that makes
	// the claim, so a refund landing concurrently either precedes this read or waits
	// behind the commit — never lands between the check and the credit.
	//
	// A FULL refund suppresses the credit entirely and the row is born `refunded`: the
	// money is back with the customer, so there is nothing to credit and nothing to
	// earn from. A PARTIAL refund records the amount and credits normally, matching
	// what the in-order path does — v1 has no clawback, and a mostly-paid purchase is
	// still a purchase.
	//
	// ⚠ REFUNDED BEATS ANOMALOUS when both apply, and the order of these two branches
	// is the decision. `anomalous` means "charged, NOT credited — go refund this by
	// hand"; if the money has already gone back, that instruction is not merely stale,
	// following it would refund the customer twice. A refunded row says the operator
	// has nothing to do, which is the truth.
	refund, err := s.refundFor(ctx, tx, pi)
	if err != nil {
		s.fail(w, "refund lookup", event.ID, err)
		return
	}
	refundedCents, fullyRefunded := refund.cents, refund.fully
	// B23.6 — a refund of a foreign-currency charge that arrived first had no rate to convert at, and this
	// session carries it: the US cents it bought over what the customer paid in that currency.
	if refund.paidCurrency != "" && refund.paidCurrency != "usd" && strings.EqualFold(refund.paidCurrency, string(sess.Currency)) {
		refundedCents = usdAtChargeRate(usdCents, sess.AmountTotal, refund.paidAmount, fullyRefunded)
		if _, err := tx.Exec(ctx, `UPDATE billing_refunds SET amount_refunded_cents = GREATEST(amount_refunded_cents, $2)
			WHERE stripe_payment_intent = $1`, pi, refundedCents); err != nil {
			s.fail(w, "refund convert", event.ID, err)
			return
		}
	}
	if fullyRefunded {
		status, creditAmt = "refunded", recomp // amount KEPT: it records what was bought
		if anomaly != "" {
			s.log.Warn("billing: anomalous purchase was already refunded — recording refunded, "+
				"no manual refund needed", "reason", anomaly, "workspace", wsID, "event", event.ID)
			anomaly = ""
		}
	}

	// livemode comes from the SIGNED event, not from inspecting the API key: it is
	// the value Stripe itself reports for the transaction, so it cannot be forged by
	// a caller and cannot drift from the key actually in use. internal/earnverify
	// consumes it — a test-mode purchase must be recordable without silently
	// conferring the same earning rights as real money.
	// A row born `refunded` keeps its lxc_amount deliberately: the partial index
	// idx_lxc_purchases_session_credited is ON (stripe_session_id) WHERE lxc_amount > 0,
	// so keeping it claims the session's single crediting slot and no later event for
	// the same session can create a second one. It is the same reason the in-order
	// refund path never zeroes the amount.
	ct, err := tx.Exec(ctx, `
		INSERT INTO lxc_purchases
			(stripe_event_id, stripe_session_id, stripe_payment_intent, workspace_id, usd_cents, lxc_amount, status, livemode,
			 refunded_cents, refunded_at, paid_currency, refunded_paid_amount)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $10 THEN NOW() ELSE NULL END, $11, $12)
		ON CONFLICT (stripe_event_id) DO NOTHING`,
		event.ID, sess.ID, pi, wsID, usdCents, creditAmt, status, event.Livemode,
		refundedCents, fullyRefunded, strings.ToLower(string(sess.Currency)), refund.paidAmount)
	if err != nil {
		// A unique violation here is NOT on the ON CONFLICT target — it is the
		// partial index idx_lxc_purchases_session_credited: this SESSION already
		// has a crediting row (e.g. completed(paid) already credited, then a stray
		// async_succeeded). Durably recorded already → ack, no new credit.
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			s.log.Warn("billing webhook: duplicate session credit blocked",
				"event", event.ID, "session", sess.ID)
			w.WriteHeader(http.StatusOK)
			return
		}
		s.fail(w, "claim insert", event.ID, err)
		return
	}
	if ct.RowsAffected() == 0 {
		// Same event id re-delivered → already processed. Ack, no new credit.
		_ = tx.Rollback(ctx)
		w.WriteHeader(http.StatusOK)
		return
	}

	if anomaly != "" {
		if err := tx.Commit(ctx); err != nil {
			s.fail(w, "commit anomaly", event.ID, err)
			return
		}
		// CHARGED but NOT credited — surfaced in the admin purchases list; v1
		// resolution is a manual refund in the Stripe dashboard.
		s.log.Warn("billing anomaly: charged, NOT credited",
			"reason", anomaly, "workspace", wsID, "event", event.ID, "session", sess.ID, "usd_cents", usdCents)
		w.WriteHeader(http.StatusOK)
		return
	}

	if fullyRefunded {
		// Durably recorded as refunded, and NOT credited. Ack: this event needs no
		// redelivery, and the claim above stops any sibling event for the session.
		if err := tx.Commit(ctx); err != nil {
			s.fail(w, "commit refunded-first", event.ID, err)
			return
		}
		s.log.Warn("billing: purchase arrived AFTER its refund — recorded refunded, not credited",
			"workspace", wsID, "event", event.ID, "session", sess.ID,
			"usd_cents", usdCents, "refunded_cents", refundedCents)
		w.WriteHeader(http.StatusOK)
		return
	}

	funding := economy.FundingLive // B22.1: credits bought in Stripe test mode are test money forever
	if !event.Livemode {
		funding = economy.FundingTest
	}
	meta := map[string]interface{}{
		"usd_cents":         usdCents,
		"stripe_event_id":   event.ID,
		"stripe_session_id": sess.ID,
		"funding":           funding,
	}
	for k, v := range paid { // what the customer actually paid, in their currency (B22.2)
		meta[k] = v
	}
	if _, err := s.credits.CreditLXCTx(ctx, tx, wsID, recomp, "stripe top-up", meta); err != nil {
		s.fail(w, "credit", event.ID, err) // rollback → claim not durable → Stripe retries
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, "commit credit", event.ID, err)
		return
	}
	// U6 PR2: best-effort owner-linkage capture. Runs AFTER the credit committed
	// above, has no error channel back here, and the 200 is acked regardless — a
	// capture failure can NEVER drop or delay the payment credit. (Worst case, a
	// slow Stripe call → Stripe re-delivers → the stripe_event_id claim above
	// suppresses a second credit.)
	s.captureCardFingerprint(ctx, wsID, pi)
	w.WriteHeader(http.StatusOK)
}

// captureCardFingerprint stores a HASH of the payment's card fingerprint (never
// the raw value) as a U6 PR2 owner-linkage signal. STRUCTURALLY best-effort:
// called only after the LXC credit is durable, returns nothing, and swallows
// every failure (no payment-intent, Stripe API error, no card on the PM, store
// error) with a WARN — it cannot affect the payment.
func (s *Service) captureCardFingerprint(ctx context.Context, wsID, paymentIntentID string) {
	if wsID == "" || paymentIntentID == "" {
		return
	}
	// B25.2: every test user pays with the same Stripe test card, so its fingerprint would link them all
	// and refuse every royalty between them.
	if s.testMode() {
		return
	}
	fp, err := s.stripe.CardFingerprint(ctx, paymentIntentID)
	if err != nil || fp == "" {
		s.log.Warn("billing: card-fingerprint capture skipped (best-effort; credit unaffected)",
			"workspace", wsID, "err", errString(err))
		return
	}
	sum := sha256.Sum256([]byte(fp))
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`, wsID, hex.EncodeToString(sum[:])); err != nil {
		s.log.Warn("billing: card-fingerprint store failed (best-effort)", "workspace", wsID, "err", err.Error())
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Service) handleRefund(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var ch stripe.Charge
	if err := json.Unmarshal(event.Data.Raw, &ch); err != nil {
		s.log.Warn("billing webhook: unparseable charge.refunded", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	pi := ""
	if ch.PaymentIntent != nil {
		pi = ch.PaymentIntent.ID
	}
	if pi == "" {
		w.WriteHeader(http.StatusOK) // nothing to correlate → ack
		return
	}

	// ⚠ THE REFUND IS RECORDED WHETHER OR NOT A PURCHASE ROW EXISTS.
	//
	// This used to be one UPDATE correlated on the payment intent. When the refund
	// arrived BEFORE its purchase — which Stripe permits, and which needs no exotic
	// race — it matched no row, acked, and recorded nothing; the purchase then landed
	// and credited normally. That cost spendable LXC for returned money AND permanent
	// earning rights, since a `completed` row is what internal/earnverify reads.
	//
	// Two alternatives were considered:
	//
	//   · DEFERRED REPLAY — queue the unmatched refund, retry later. Rejected: the
	//     credit is live and spendable for the length of the retry window, and on a
	//     money path "it was correct eventually" is the failure you cannot explain to
	//     the customer who spent it.
	//   · 5xx SO STRIPE RETRIES — zero schema, but it outsources OUR ordering problem
	//     to Stripe's retry schedule, reports a working service as broken for up to
	//     days, and when the retries are exhausted the refund is lost silently anyway.
	//     It also turns a legitimate refund for a charge this deployment never handled
	//     into a permanent error loop.
	//
	// Recording it makes BOTH orders converge on the same state, which is the only
	// property worth having here. The cost is one row and one indexed lookup on the
	// credit path (in the transaction that path already opens).
	//
	// Amounts are ASSIGNED, not accumulated: Stripe reports the charge's running
	// total on every event, so a partial→full sequence lands on the full amount.
	// GREATEST guards the one case that would otherwise lose information — events
	// delivered out of order relative to each other.
	// ⚠ THE DEFAULT WHEN THE EVENT DOES NOT SAY IS "FULL", AND THE DIRECTION IS THE
	// POINT. A charge.refunded event means money went back; the only question is how
	// much. Partial refunds always report a non-zero amount_refunded, so an event
	// carrying none is either a fixture or a shape we did not anticipate — and reading
	// that as "a refund of nothing" would silently ignore a refund, which is the exact
	// class of bug this change exists to remove. Treating it as full is recoverable
	// (an operator sees a purchase marked refunded and can look); the other way is not.
	fully := ch.Refunded
	switch {
	case fully:
	case ch.AmountRefunded == 0:
		fully = true // no amount information → treat as full
	case ch.Amount > 0 && ch.AmountRefunded >= ch.Amount:
		fully = true // amounts say full even if the flag has not caught up
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.fail(w, "refund begin", event.ID, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// B23.6 — the charge's amounts are in the currency the customer PAID, which since B22.2 need not be USD.
	// The US cents refunded are what that share of the charge bought: the purchase's usd_cents at the charge's
	// own rate. Before the purchase is seen there is no rate, so a foreign refund records 0 US cents here and
	// the credit path converts it when the purchase lands; the paid-currency amount is recorded either way.
	paidCurrency := strings.ToLower(string(ch.Currency))
	refundedUSD := ch.AmountRefunded
	if paidCurrency != "" && paidCurrency != "usd" {
		var usdCents int64
		err := tx.QueryRow(ctx, `SELECT usd_cents FROM lxc_purchases WHERE stripe_payment_intent = $1
			ORDER BY lxc_amount DESC LIMIT 1`, pi).Scan(&usdCents)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			refundedUSD = 0
		case err != nil:
			s.fail(w, "refund purchase read", event.ID, err)
			return
		default:
			refundedUSD = usdAtChargeRate(usdCents, ch.Amount, ch.AmountRefunded, fully)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO billing_refunds
			(stripe_payment_intent, fully_refunded, amount_refunded_cents, first_event_id, paid_currency, amount_refunded_paid)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (stripe_payment_intent) DO UPDATE SET
			fully_refunded        = billing_refunds.fully_refunded OR EXCLUDED.fully_refunded,
			amount_refunded_cents = GREATEST(billing_refunds.amount_refunded_cents, EXCLUDED.amount_refunded_cents),
			paid_currency         = COALESCE(NULLIF(EXCLUDED.paid_currency, ''), billing_refunds.paid_currency),
			amount_refunded_paid  = GREATEST(billing_refunds.amount_refunded_paid, EXCLUDED.amount_refunded_paid),
			updated_at            = NOW()`,
		pi, fully, refundedUSD, event.ID, paidCurrency, ch.AmountRefunded); err != nil {
		s.fail(w, "refund record", event.ID, err)
		return
	}

	// Apply to the purchase if we have one. A PARTIAL refund records the amount and
	// leaves status alone — marking a $50 purchase refunded because $1 came back is
	// wrong today (reporting) and dangerous the moment a clawback reads this status.
	// refunded_at is still set once and only on a full refund, preserving the
	// original idempotency.
	if _, err := tx.Exec(ctx, `
		UPDATE lxc_purchases
		SET refunded_cents       = GREATEST(refunded_cents, $2),
		    status               = CASE WHEN $3 THEN 'refunded' ELSE status END,
		    refunded_at          = CASE WHEN $3 AND refunded_at IS NULL THEN NOW() ELSE refunded_at END,
		    paid_currency        = CASE WHEN paid_currency = '' THEN $4 ELSE paid_currency END,
		    refunded_paid_amount = GREATEST(refunded_paid_amount, $5)
		WHERE stripe_payment_intent = $1`, pi, refundedUSD, fully, paidCurrency, ch.AmountRefunded); err != nil {
		s.fail(w, "refund mark", event.ID, err)
		return
	}

	// ⚠ REMOVE THE CASH BACKING THIS REFUND TOOK BACK OUT OF THE SYSTEM.
	//
	// Lens does NOT claw back the credit on a refund — the workspace keeps the spendable LXC and
	// only the purchase row is marked. That is a deliberate decision, but it means the money behind
	// that credit is gone while the credit remains, and without this the workspace keeps PHANTOM
	// BACKING that mints a royalty on every future settle: a purchase-then-refund would fund
	// royalties forever with money that was returned.
	//
	// Scoped to the rows this payment intent actually credited, so a refund can never remove more
	// backing than that purchase added. ReduceCashBackedForRefund clamps at zero, which is the
	// correct reading when the backing was already spent: there is nothing left to take back, and
	// the mints that already happened are not clawed back here (there is no clawback path).
	//
	// Same transaction as the tombstone and the purchase mark, so a crash cannot leave the refund
	// recorded with the backing intact.
	rows, err := tx.Query(ctx,
		`SELECT workspace_id, lxc_amount FROM lxc_purchases
		  WHERE stripe_payment_intent = $1 AND lxc_amount > 0`, pi)
	if err != nil {
		s.fail(w, "refund backing read", event.ID, err)
		return
	}
	type backing struct {
		ws     string
		amount int64
	}
	var toReduce []backing
	for rows.Next() {
		var b backing
		if err := rows.Scan(&b.ws, &b.amount); err != nil {
			rows.Close()
			s.fail(w, "refund backing scan", event.ID, err)
			return
		}
		toReduce = append(toReduce, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.fail(w, "refund backing rows", event.ID, err)
		return
	}
	for _, b := range toReduce {
		// A PARTIAL refund removes the refunded FRACTION of that purchase's backing; a full refund
		// removes all of it. Reading the fraction off the amounts rather than assuming full keeps a
		// $1 refund on a $50 purchase from deleting $50 of backing.
		reduce := b.amount
		if !fully && ch.Amount > 0 && ch.AmountRefunded < ch.Amount {
			reduce = b.amount * ch.AmountRefunded / ch.Amount
		}
		if err := economy.ReduceCashBackedForRefund(ctx, tx, b.ws, reduce); err != nil {
			s.fail(w, "refund backing reduce", event.ID, err)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		s.fail(w, "refund commit", event.ID, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// classify returns a non-empty anomaly reason when the (signed) event must NOT be
// credited, or a non-nil error when the outcome cannot be VERIFIED (caller retries
// with 5xx rather than guessing). NEVER trusts session metadata as the amount.
func (s *Service) classify(ctx context.Context, currency string, usdCents int64, wsID string, recomp, metaLXC int64) (string, error) {
	if !strings.EqualFold(currency, "usd") {
		return "currency", nil
	}
	if usdCents <= 0 {
		return "nonpositive_amount", nil
	}
	if !topUpInBounds(usdCents) {
		return "amount_not_allowlisted", nil
	}
	if wsID == "" {
		return "unknown_workspace", nil
	}
	ok, err := s.wsExists(ctx, wsID)
	if err != nil {
		return "", err // cannot verify → retry, do not mark anomalous
	}
	if !ok {
		return "unknown_workspace", nil
	}
	// SEC-2: µLXC is an exact integer, so the server recomputation and the
	// client-metadata value must be BIT-EQUAL (the float-epsilon check is gone).
	if recomp != metaLXC {
		return "amount_mismatch", nil
	}
	return "", nil
}

// refundFor reads any refund already recorded against this payment intent, ON THE
// CALLER'S TX so the read is serialized with the claim it informs. No row is the
// ordinary case and is not an error. An empty payment intent (a session with no
// charge yet) cannot correlate to a refund, so it short-circuits.
func (s *Service) refundFor(ctx context.Context, tx pgx.Tx, paymentIntentID string) (r recordedRefund, err error) {
	if paymentIntentID == "" {
		return r, nil
	}
	err = tx.QueryRow(ctx,
		`SELECT amount_refunded_cents, fully_refunded, paid_currency, amount_refunded_paid FROM billing_refunds
		 WHERE stripe_payment_intent = $1`, paymentIntentID).Scan(&r.cents, &r.fully, &r.paidCurrency, &r.paidAmount)
	if errors.Is(err, pgx.ErrNoRows) {
		return recordedRefund{}, nil
	}
	return r, err
}

// recordedRefund is a billing_refunds row: US cents, and the same refund in the currency the charge was paid in.
type recordedRefund struct {
	cents        int64
	fully        bool
	paidCurrency string
	paidAmount   int64
}

// usdAtChargeRate is the US cents a refund of paidRefunded (in the charge's currency) gives back, at that
// charge's own rate: the usdCents the purchase bought over the paidTotal it cost. A full refund is all of
// them; a charge with no total cannot be converted and reads 0 rather than a guess. Rounded to the cent.
func usdAtChargeRate(usdCents, paidTotal, paidRefunded int64, fully bool) int64 {
	switch {
	case fully || (paidTotal > 0 && paidRefunded >= paidTotal):
		return usdCents
	case paidTotal <= 0:
		return 0
	}
	return (usdCents*paidRefunded + paidTotal/2) / paidTotal
}

func (s *Service) fail(w http.ResponseWriter, stage, eventID string, err error) {
	// 5xx → Stripe redelivers. err is a DB/infra error, never a secret.
	s.log.Error("billing webhook failed", "stage", stage, "event", eventID, "err", err)
	http.Error(w, "temporary error", http.StatusServiceUnavailable)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Purchase is one lxc_purchases row, for the admin refund-visibility list.
type Purchase struct {
	ID              string     `json:"id"`
	StripeEventID   string     `json:"stripe_event_id"`
	StripeSessionID string     `json:"stripe_session_id"`
	WorkspaceID     string     `json:"workspace_id"`
	USDCents        int64      `json:"usd_cents"`
	LXCAmount       int64      `json:"lxc_amount_ulxc"` // µLXC (SEC-2)
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	RefundedAt      *time.Time `json:"refunded_at,omitempty"`
}

// ListPurchases returns recent purchase rows, newest first (admin, read-only).
func (s *Service) ListPurchases(ctx context.Context, limit int) ([]Purchase, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, stripe_event_id, stripe_session_id, workspace_id, usd_cents, lxc_amount, status, created_at, refunded_at
		FROM lxc_purchases ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Purchase, 0, limit)
	for rows.Next() {
		var p Purchase
		if err := rows.Scan(&p.ID, &p.StripeEventID, &p.StripeSessionID, &p.WorkspaceID,
			&p.USDCents, &p.LXCAmount, &p.Status, &p.CreatedAt, &p.RefundedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
