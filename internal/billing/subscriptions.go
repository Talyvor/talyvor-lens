package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v81"
)

// MODEL 2, STEP 1 — STRIPE SUBSCRIPTIONS. W4.6.1, on TEST keys.
//
// Everything in billing.go is a ONE-OFF payment: a Checkout Session completes and
// LXC is credited. This file adds the recurring half — a subscription checkout, and
// the four webhook events that move a subscription through its life:
//
//	customer.subscription.created  → the workspace is subscribed
//	customer.subscription.updated  → renewal, past_due, cancel-at-period-end
//	customer.subscription.deleted  → cancelled, for good
//	invoice.payment_failed         → the dunning signal, recorded on its own (a marketplace bill's, once Stripe
//	                                  gives up on it, ends the licences it carried unpaid: B32.20, market_bill.go)
//
// ⚠ WHAT THIS FILE DELIBERATELY DOES NOT DO. It does not grant an allowance, and it
// does not touch the LXC ledger. W4.6.1's step 2 is the allowance ledger (phi = F/D
// < 1, HARD CAP never overage) and it is a separate merge with its own economics to
// argue. A subscription here means exactly one thing: Stripe says this workspace is
// paying. What that entitles them to is step 2's question.
//
// ⚠ F AND D ARE NOT IN THIS FILE. The item says they are Nicolai's. `price_id` records
// which Stripe Price actually billed, so the number lives in Stripe and in the
// operator's config — never hardcoded here where it would become a second source of
// truth for a price.

// ErrNoSubscriptionPrice is returned when a subscription checkout is requested on a
// deployment that has not been configured with a Stripe Price. It is a CONFIGURATION
// refusal, not a payment failure: the caller gets a clear 501-shaped answer rather
// than a Stripe error about an empty price id.
var ErrNoSubscriptionPrice = errors.New("billing: no subscription price configured (LENS_BILLING_SUBSCRIPTION_PRICE_ID)")

// SubscriptionParams is the input to a subscription Checkout Session. Unlike
// CheckoutParams there is no amount: the PRICE is the amount, and it lives in Stripe.
type SubscriptionParams struct {
	WorkspaceID string
	CustomerID  string
	PriceID     string
}

// subscriptionAPI is the subscription half of the Stripe seam. Kept separate from
// stripeAPI and satisfied by the same live client, so a Service built without a
// subscription price still compiles and every existing test double keeps working
// without learning a method it has no opinion about.
type subscriptionAPI interface {
	CreateSubscriptionCheckoutSession(ctx context.Context, p SubscriptionParams) (url string, sessionID string, err error)
	SetCancelAtPeriodEnd(ctx context.Context, subscriptionID string, cancel bool) (*stripe.Subscription, error)
}

// terminalStatuses never move again. A row in one of these is history.
var terminalStatuses = map[string]bool{"canceled": true, "incomplete_expired": true}

// CreateSubscriptionCheckout returns a Stripe Checkout URL in subscription mode.
//
// ⚠ IT REFUSES A SECOND LIVE SUBSCRIPTION BEFORE STRIPE IS EVER CALLED. The unique
// index in 0120 is the backstop and this is the courtesy: a workspace that already
// pays should be told so, not sent to a checkout that will bill them again and then
// collide on the webhook — where the money has already moved.
func (s *Service) CreateSubscriptionCheckout(ctx context.Context, workspaceID string) (string, error) {
	return s.CreatePlanCheckout(ctx, workspaceID, "")
}

// SellsSubscriptions reports whether this Service has a subscription price or plans to sell.
func (s *Service) SellsSubscriptions() bool {
	return s.subStripe != nil && (s.subPrice != "" || len(s.subPlans) > 0)
}

// ErrUnknownPlan is a checkout for a plan this deployment does not sell.
var ErrUnknownPlan = errors.New("billing: unknown subscription plan")

// CreatePlanCheckout is CreateSubscriptionCheckout for a named plan (B13.1). plan "" is the single
// configured price, when there is one.
func (s *Service) CreatePlanCheckout(ctx context.Context, workspaceID, plan string) (string, error) {
	price := s.subPrice
	if plan != "" {
		if price = s.subPlans[plan]; price == "" {
			return "", fmt.Errorf("%w %q", ErrUnknownPlan, plan)
		}
	}
	if price == "" || s.subStripe == nil {
		return "", ErrNoSubscriptionPrice
	}
	live, err := s.liveSubscriptionID(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if live != "" {
		return "", fmt.Errorf("billing: workspace %s already has a live subscription (%s)", workspaceID, live)
	}
	customerID, err := s.ensureCustomer(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("billing: ensure customer: %w", err)
	}
	url, _, err := s.subStripe.CreateSubscriptionCheckoutSession(ctx, SubscriptionParams{
		WorkspaceID: workspaceID,
		CustomerID:  customerID,
		PriceID:     price,
	})
	if err != nil {
		return "", fmt.Errorf("billing: create subscription checkout: %w", err)
	}
	return url, nil
}

// liveSubscriptionID returns the workspace's non-terminal subscription id, or "".
// The predicate is the SAME status set as 0120's partial unique index — if the two
// ever disagree, this returns "" for a workspace the database will then refuse to
// insert, which is the confusing half of a double-subscribe.
func (s *Service) liveSubscriptionID(ctx context.Context, workspaceID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT stripe_subscription_id FROM subscriptions
		WHERE workspace_id = $1 AND status IN ('trialing','active','past_due','unpaid')`, workspaceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("billing: live subscription lookup: %w", err)
	}
	return id, nil
}

// SubscriptionStatus is the read model: what the rest of Lens asks about a workspace.
type SubscriptionStatus struct {
	Subscribed        bool       `json:"subscribed"`
	Status            string     `json:"status,omitempty"`
	CurrentPeriodEnd  *time.Time `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	SubscriptionID    string     `json:"subscription_id,omitempty"`
	Livemode          bool       `json:"livemode"`
	BYOK              bool       `json:"byok"` // B27.26: the BYOK plan — own provider keys, no tokens charged
	// B32.10: the plan PlanOf answers — free with no paying subscription, enterprise on an operator's contract.
	Plan string `json:"plan"`
}

// GetSubscription answers "is this workspace paying". `Subscribed` is TRUE only for
// statuses that mean money is currently flowing.
//
// ⚠ past_due IS SUBSCRIBED AND unpaid IS NOT, and that asymmetry is the decision.
// `past_due` is Stripe's "a payment failed and we are retrying" — the customer has
// not left and Stripe may still collect, so cutting service on the first failed card
// is how a business loses a paying customer to an expired card. `unpaid` is the state
// AFTER Stripe has given up retrying; there the answer really is no.
func (s *Service) GetSubscription(ctx context.Context, workspaceID string) (*SubscriptionStatus, error) {
	var (
		st        SubscriptionStatus
		status    string
		periodEnd *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT stripe_subscription_id, status, current_period_end, cancel_at_period_end, livemode, byok
		FROM subscriptions
		WHERE workspace_id = $1 AND status IN ('trialing','active','past_due','unpaid')`, workspaceID).
		Scan(&st.SubscriptionID, &status, &periodEnd, &st.CancelAtPeriodEnd, &st.Livemode, &st.BYOK)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("billing: subscription read: %w", err)
	}
	if err == nil {
		st.Status = status
		st.CurrentPeriodEnd = periodEnd
		st.Subscribed = status == "trialing" || status == "active" || status == "past_due"
	}
	if st.Plan, err = PlanOf(ctx, s.pool, workspaceID); err != nil {
		return nil, err
	}
	return &st, nil
}

// ErrNoLiveSubscription is returned when a cancel or resume is asked of a workspace
// that has nothing live to cancel or resume.
var ErrNoLiveSubscription = errors.New("billing: workspace has no live subscription")

// SetCancelAtPeriodEnd cancels (cancel=true) or resumes (cancel=false) the
// workspace's live subscription. B1.5.
//
// ⚠ CANCEL MEANS "AT THE END OF THE PAID PERIOD", NOT NOW. The workspace paid for
// the month; it keeps what it paid for, stays Subscribed until current_period_end,
// and Stripe sends customer.subscription.deleted when the period runs out. Resume is
// the same call with false, and it exists because without it a customer who changes
// their mind is stuck: CreateSubscriptionCheckout refuses a second live subscription.
//
// ⚠ THIS WRITES NOTHING TO THE subscriptions TABLE. The webhook is the only author
// of that row — the customer.subscription.updated this call provokes is what moves
// it, through the same idempotency and out-of-order guards as every other event. The
// returned status is STRIPE'S answer to the update, so the caller sees the change
// immediately without this becoming a second writer of one state machine.
//
// B23.4 — it needs a Stripe client and nothing else. The subscription to cancel is already live, so it
// does not matter which price sold it: a deployment selling only named plans (subPrice empty) must still
// let every one of its subscribers cancel and resume.
func (s *Service) SetCancelAtPeriodEnd(ctx context.Context, workspaceID string, cancel bool) (*SubscriptionStatus, error) {
	if s.subStripe == nil {
		return nil, ErrNoSubscriptionPrice
	}
	subID, err := s.liveSubscriptionID(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if subID == "" {
		return nil, ErrNoLiveSubscription
	}
	sub, err := s.subStripe.SetCancelAtPeriodEnd(ctx, subID, cancel)
	if err != nil {
		return nil, fmt.Errorf("billing: update subscription %s: %w", subID, err)
	}
	return s.statusOf(sub), nil
}

// statusOf is the read model of Stripe's answer to an update: what the webhook it provokes will record.
func (s *Service) statusOf(sub *stripe.Subscription) *SubscriptionStatus {
	status := string(sub.Status)
	plan, _, byok := s.subscriptionPlan(sub)
	if plan == "" {
		plan = FreePlan
	}
	return &SubscriptionStatus{
		Subscribed:        status == "trialing" || status == "active" || status == "past_due",
		Status:            status,
		CurrentPeriodEnd:  periodEnd(sub),
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
		SubscriptionID:    sub.ID,
		Livemode:          sub.Livemode,
		BYOK:              byok,
		Plan:              plan,
	}
}

// ErrSamePlan is a plan change to the plan the workspace is already on.
var ErrSamePlan = errors.New("billing: the workspace is already on that plan")

// ErrBYOKPlanChange is a plan change to or from BYOK (B27.26): cancel, then subscribe to the other plan.
var ErrBYOKPlanChange = errors.New("billing: BYOK is not changed to or from another plan — cancel, then subscribe")

// ErrPlanKindChange is a plan change between a company plan and a personal one (B32.10): a personal plan's fee
// includes usage and a company plan's does not, so there is no allowance to prorate — cancel, then subscribe.
var ErrPlanKindChange = errors.New("billing: a company plan (Team, Business) and a personal plan (Plus, Pro, Max) are not changed one into the other — cancel, then subscribe")

// companyPlans are the plans a company buys for its workspace (B32.10). Their fee buys no tokens, so a period
// of one grants no allowance.
var companyPlans = map[string]bool{TeamPlan: true, BusinessPlan: true}

// subscriptionPlans are the plans a subscriptions row may name (migration 0196's CHECK).
var subscriptionPlans = map[string]bool{"plus": true, "pro": true, "max": true, BYOKPlan: true,
	TeamPlan: true, BusinessPlan: true, EnterprisePlan: true}

// planChangeAPI is the Stripe call that moves a subscription to another price (B18.14). Optional, as
// subscriptionAPI is kept apart from stripeAPI: a test double without it still builds a Service.
type planChangeAPI interface {
	ChangeSubscriptionPrice(ctx context.Context, subscriptionID, priceID string) (*stripe.Subscription, error)
}

// ChangePlan moves the workspace's live subscription to another named plan — Plus to Pro, or back —
// with proration (B18.14): Stripe credits the unused time on the old price and charges the rest of the
// period on the new one, on the next invoice.
//
// ⚠ LIKE CANCEL, THIS WRITES NOTHING. The customer.subscription.updated Stripe sends is what records the
// new price, and it is what moves this period's allowance to the new plan (applyPlanChange) — one author
// of the subscription's state, behind the same idempotency and ordering guards as every other event.
func (s *Service) ChangePlan(ctx context.Context, workspaceID, plan string) (*SubscriptionStatus, error) {
	api, ok := s.subStripe.(planChangeAPI)
	if !ok || len(s.subPlans) == 0 {
		return nil, ErrNoSubscriptionPrice
	}
	price := s.subPlans[plan]
	if price == "" {
		return nil, fmt.Errorf("%w %q", ErrUnknownPlan, plan)
	}
	var subID, current, currentPlan string
	err := s.pool.QueryRow(ctx, `
		SELECT stripe_subscription_id, COALESCE(price_id, ''), COALESCE(plan, '') FROM subscriptions
		WHERE workspace_id = $1 AND status IN ('trialing','active','past_due','unpaid')`, workspaceID).Scan(&subID, &current, &currentPlan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoLiveSubscription
	}
	if err != nil {
		return nil, fmt.Errorf("billing: live subscription lookup: %w", err)
	}
	if current == price {
		return nil, fmt.Errorf("%w (%s)", ErrSamePlan, plan)
	}
	// B27.26: BYOK carries no allowance and the others carry one, so a prorated move between them has no
	// allowance to move — it is a cancel and a new subscription.
	if plan == BYOKPlan || current == s.subPlans[BYOKPlan] || currentPlan == BYOKPlan {
		return nil, ErrBYOKPlanChange
	}
	// B32.10: Team and Business change into each other (a move to Business drops Team's BYOK add-on, which
	// Business includes), and Plus, Pro and Max into each other — never one kind into the other.
	if companyPlans[plan] != companyPlans[currentPlan] {
		return nil, ErrPlanKindChange
	}
	sub, err := api.ChangeSubscriptionPrice(ctx, subID, price)
	if err != nil {
		return nil, fmt.Errorf("billing: change plan of %s: %w", subID, err)
	}
	return s.statusOf(sub), nil
}

// ErrBYOKAddon is the BYOK add-on asked of a subscription other than Team (B32.10): Business includes BYOK,
// and the personal plans and BYOK itself do not take it.
var ErrBYOKAddon = errors.New("billing: the BYOK add-on is Team's")

// ErrBYOKAddonUnchanged is adding the add-on to a Team subscription that has it, or removing it from one that
// does not.
var ErrBYOKAddonUnchanged = errors.New("billing: the Team subscription already is as asked")

// addonAPI adds and removes a subscription's further items (B32.10). Optional, like planChangeAPI.
type addonAPI interface {
	AddSubscriptionItem(ctx context.Context, subscriptionID, priceID, idempotencyKey string) (*stripe.Subscription, error)
	RemoveSubscriptionItem(ctx context.Context, subscriptionID, priceID string) (*stripe.Subscription, error)
}

// SetBYOKAddon adds (on) or removes BYOK, Team's add-on, as a second item of the workspace's live Team
// subscription, with proration (B32.10). Like a plan change it writes nothing: the
// customer.subscription.updated Stripe sends records byok.
func (s *Service) SetBYOKAddon(ctx context.Context, workspaceID string, on bool) (*SubscriptionStatus, error) {
	api, ok := s.subStripe.(addonAPI)
	byokPrice := s.subPlans[BYOKPlan]
	if !ok || byokPrice == "" {
		return nil, ErrNoSubscriptionPrice
	}
	var (
		subID, plan string
		byok        bool
		lastEvent   time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT stripe_subscription_id, COALESCE(plan, ''), byok, last_event_at FROM subscriptions
		WHERE workspace_id = $1 AND status IN ('trialing','active','past_due','unpaid')`, workspaceID).
		Scan(&subID, &plan, &byok, &lastEvent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoLiveSubscription
	}
	if err != nil {
		return nil, fmt.Errorf("billing: live subscription lookup: %w", err)
	}
	if plan != TeamPlan {
		if plan == "" {
			plan = "an unnamed plan"
		}
		return nil, fmt.Errorf("%w — this workspace is on %s", ErrBYOKAddon, plan)
	}
	if byok == on {
		return nil, ErrBYOKAddonUnchanged
	}
	var sub *stripe.Subscription
	if on {
		// One add per recorded state: a second click before the webhook lands is the same request to Stripe.
		sub, err = api.AddSubscriptionItem(ctx, subID, byokPrice, fmt.Sprintf("byok-addon-%s-%d", subID, lastEvent.UnixNano()))
	} else {
		sub, err = api.RemoveSubscriptionItem(ctx, subID, byokPrice)
	}
	if err != nil {
		return nil, fmt.Errorf("billing: BYOK add-on on %s: %w", subID, err)
	}
	return s.statusOf(sub), nil
}

// BackfillPlans names the plan of every subscription recorded before subscriptions.plan existed (B32.10),
// from the Price it bills: each Price this Service sells under a plan's name. Migration 0196 names BYOK's.
func (s *Service) BackfillPlans(ctx context.Context) (int64, error) {
	var n int64
	for plan, priceID := range s.subPlans {
		if !subscriptionPlans[plan] || priceID == "" {
			continue
		}
		ct, err := s.pool.Exec(ctx, `UPDATE subscriptions SET plan = $1 WHERE plan IS NULL AND price_id = $2`, plan, priceID)
		if err != nil {
			return n, fmt.Errorf("billing: backfill the %s plan: %w", plan, err)
		}
		n += ct.RowsAffected()
	}
	return n, nil
}

// applyPlanChange moves an already-granted period's allowance to the plan the subscription is now on
// (B18.14). Stripe prorates the fee — the unused time on the old price is credited, the rest of the
// period charged at the new one — and the allowance follows the same share: it changes by the
// difference between the two plans' included usage times the share of the period left, never below
// what has already been used. The row it writes says what moved.
//
// ⚠ IDEMPOTENT ON THE FEE. A period whose recorded fee is already this one — a renewal, a redelivery,
// any update that did not change the price — is left alone, so an event can never apply twice.
func (s *Service) applyPlanChange(ctx context.Context, eventID, workspaceID, subscriptionID, priceID string,
	start, end time.Time, fee int64, at time.Time) error {
	if fee <= 0 || !end.After(start) {
		return nil // an unknown fee sizes nothing
	}
	var oldFee int64
	err := s.pool.QueryRow(ctx, `SELECT fee_usd_cents FROM subscription_allowance
		WHERE stripe_subscription_id = $1 AND period_start = $2`, subscriptionID, start).Scan(&oldFee)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (oldFee == fee || oldFee <= 0)) {
		return nil
	}
	if err != nil {
		return err
	}
	oldIncluded, err := s.includedUsage(ctx, oldFee, start)
	if err != nil {
		return err
	}
	newIncluded, err := s.includedUsage(ctx, fee, start)
	if err != nil {
		return err
	}
	left := math.Min(1, math.Max(0, float64(end.Sub(at))/float64(end.Sub(start))))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var granted, consumed, lockedFee int64
	if err := tx.QueryRow(ctx, `SELECT granted_ulxc, consumed_ulxc, fee_usd_cents FROM subscription_allowance
		WHERE stripe_subscription_id = $1 AND period_start = $2 FOR UPDATE`, subscriptionID, start).
		Scan(&granted, &consumed, &lockedFee); err != nil {
		return err
	}
	if lockedFee != oldFee {
		return nil // another delivery moved it first
	}
	after := max(granted+int64(math.Round(float64(newIncluded-oldIncluded)*left)), consumed)
	if _, err := tx.Exec(ctx, `UPDATE subscription_allowance SET granted_ulxc = $1, fee_usd_cents = $2
		WHERE stripe_subscription_id = $3 AND period_start = $4`, after, fee, subscriptionID, start); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO subscription_plan_changes (stripe_event_id, workspace_id, stripe_subscription_id, period_start,
			price_id, from_fee_usd_cents, to_fee_usd_cents, remaining_fraction, granted_before_ulxc, granted_after_ulxc, changed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		eventID, workspaceID, subscriptionID, start, priceID, oldFee, fee, left, granted, after, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ─── the webhook half ────────────────────────────────────────────────────────────

// handleSubscription applies a customer.subscription.* event.
//
// ⚠ THE WHOLE FUNCTION IS ONE TRANSACTION, and the event row is written on EVERY
// path — including the paths that change nothing. W4.6.1: "ASSERT THE LEDGER ROW,
// NEVER A STATUS CODE." A 200 proves only that the handler did not crash; the row
// says what it decided and why, and `applied` is the difference.
func (s *Service) handleSubscription(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		// Signed but unparseable — it will never parse; ack so Stripe stops retrying.
		s.log.Warn("billing webhook: unparseable subscription", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	periodFromItems(event.Data.Raw, &sub)
	if sub.ID == "" {
		s.log.Warn("billing webhook: subscription event with no subscription id", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	wsID := sub.Metadata["workspace_id"]
	status := string(sub.Status)
	if event.Type == "customer.subscription.deleted" {
		// Stripe sends the object as it was; the event TYPE is the fact.
		status = "canceled"
	}
	eventAt := time.Unix(event.Created, 0).UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.fail(w, "begin", event.ID, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ⚠ THE IDEMPOTENCY CLAIM COMES FIRST, and it is a real INSERT rather than a
	// SELECT-then-act: two concurrent deliveries of the same event both reach the
	// insert, one wins, and the loser sees zero rows affected and stops. A read-first
	// check has a window between the read and the write that a retry fits inside.
	var (
		existing  string
		lastEvent time.Time
		haveRow   bool
	)
	err = tx.QueryRow(ctx, `
		SELECT status, last_event_at FROM subscriptions
		WHERE stripe_subscription_id = $1 FOR UPDATE`, sub.ID).Scan(&existing, &lastEvent)
	switch {
	case err == nil:
		haveRow = true
	case errors.Is(err, pgx.ErrNoRows):
		haveRow = false
	default:
		s.fail(w, "subscription lookup", event.ID, err)
		return
	}

	// ⚠ OUT-OF-ORDER DELIVERY IS THE CASE THIS ITEM NAMES AS "where every naive
	// implementation breaks". Stripe guarantees delivery, not ORDER. An older
	// `.updated` carrying past_due can arrive after the newer one that restored
	// active, and applying it would dun a customer who has already paid. Compare the
	// event's OWN created time against the last one we applied, and refuse to go
	// backwards. A terminal row is likewise never reopened.
	stale := haveRow && (!eventAt.After(lastEvent) || terminalStatuses[existing])
	applied := !stale

	if wsID == "" && haveRow {
		// A `.deleted` payload may omit metadata; the row already knows whose it is.
		if err := tx.QueryRow(ctx,
			`SELECT workspace_id FROM subscriptions WHERE stripe_subscription_id = $1`, sub.ID).Scan(&wsID); err != nil {
			s.fail(w, "workspace lookup", event.ID, err)
			return
		}
	}
	if wsID == "" {
		// Nothing to attribute it to and nothing on file. Record and ack — retrying
		// will not add the metadata.
		s.log.Warn("billing webhook: subscription event with no workspace", "event", event.ID, "subscription", sub.ID)
		applied = false
	}
	// B25.2 — a test workspace's subscription is the test-mode Service's, a real one's the live Service's.
	// B50.13 — a test-mode event never starts or changes a real workspace's plan, but its end is let through, so a
	// plan a real workspace got from test-mode money does not outlive its subscription.
	if mine, err := s.takes(ctx, wsID, event.Livemode || event.Type == "customer.subscription.deleted"); err != nil {
		s.fail(w, "workspace kind", event.ID, err)
		return
	} else if !mine {
		s.log.Info("billing webhook: a subscription of the other kind of workspace — test and real money are kept apart",
			"event", event.ID, "subscription", sub.ID, "workspace", wsID)
		w.WriteHeader(http.StatusOK)
		return
	}

	plan, priceID, byok := s.subscriptionPlan(&sub)
	if applied {
		if haveRow {
			if _, err := tx.Exec(ctx, `
				UPDATE subscriptions
				SET status = $1, current_period_end = $2, cancel_at_period_end = $3,
				    price_id = COALESCE(NULLIF($4, ''), price_id),
				    byok = CASE WHEN $4 = '' THEN byok ELSE $7 END,
				    plan = CASE WHEN $4 = '' THEN plan ELSE NULLIF($8::text, '') END,
				    last_event_at = $5, updated_at = NOW()
				WHERE stripe_subscription_id = $6`,
				status, periodEnd(&sub), sub.CancelAtPeriodEnd, priceID, eventAt, sub.ID, byok, plan); err != nil {
				s.fail(w, "subscription update", event.ID, err)
				return
			}
		} else {
			// ⚠ A UNIQUE VIOLATION HERE IS THE DOUBLE-SUBSCRIBE the partial index in
			// 0120 exists to stop: this workspace already has a live subscription and
			// Stripe is telling us about a second one. That is money already taken, so
			// it must NOT be silently dropped — the event row is written with
			// applied=false and an operator can see both ids.
			//
			// ⚠⚠ THE SAVEPOINT IS LOAD-BEARING AND THE FIRST DRAFT DID NOT HAVE ONE.
			// In Postgres a constraint violation aborts the ENTIRE transaction: every
			// later command returns 25P02 "current transaction is aborted". So catching
			// the unique violation and carrying on to write the event row wrote nothing
			// — the insert failed, the handler returned 503, and Stripe would retry that
			// event forever while the operator saw NO row explaining why. Found by
			// TestSubscription_SecondLiveSubscription_NotApplied, which asserts the row
			// rather than the status code and so could see it at all.
			//
			// pgx's nested Begin is a SAVEPOINT: the rollback undoes only the failed
			// insert and leaves the outer transaction usable.
			sp, err := tx.Begin(ctx)
			if err != nil {
				s.fail(w, "savepoint", event.ID, err)
				return
			}
			_, err = sp.Exec(ctx, `
				INSERT INTO subscriptions
					(workspace_id, stripe_subscription_id, stripe_customer_id, price_id,
					 status, current_period_end, cancel_at_period_end, livemode, last_event_at, byok, plan)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11::text, ''))`,
				wsID, sub.ID, customerOf(&sub), priceID, status, periodEnd(&sub),
				sub.CancelAtPeriodEnd, event.Livemode, eventAt, byok, plan)
			if isUniqueViolation(err) {
				_ = sp.Rollback(ctx)
				s.log.Warn("billing webhook: workspace already has a live subscription — NOT applied",
					"event", event.ID, "workspace", wsID, "subscription", sub.ID)
				applied = false
			} else if err != nil {
				_ = sp.Rollback(ctx)
				s.fail(w, "subscription insert", event.ID, err)
				return
			} else if err := sp.Commit(ctx); err != nil {
				s.fail(w, "savepoint commit", event.ID, err)
				return
			}
		}
	}

	ct, err := tx.Exec(ctx, `
		INSERT INTO subscription_events
			(stripe_event_id, stripe_subscription_id, workspace_id, event_type,
			 status_requested, applied, livemode, event_created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (stripe_event_id) DO NOTHING`,
		event.ID, sub.ID, wsID, string(event.Type), status, applied, event.Livemode, eventAt)
	if err != nil {
		s.fail(w, "subscription event insert", event.ID, err)
		return
	}
	if ct.RowsAffected() == 0 {
		// A REDELIVERY OF AN EVENT ALREADY RECORDED. Roll back and ack.
		//
		// ⚠ THIS ROLLBACK IS DEFENCE-IN-DEPTH, NOT THE THING THAT MAKES REDELIVERY SAFE,
		// and the comment that stood here claimed otherwise. It said "without this the
		// second delivery would re-apply an update that a LATER event had already
		// superseded". That is UNREACHABLE: a redelivery carries the same
		// `event.created` as the original, so `eventAt.After(lastEvent)` is false and the
		// staleness guard has already set applied=false — there is nothing above to undo.
		// Control C3 in scripts/w461-subscription-controls-q4vn.py turned this into a
		// Commit and NOTHING went red, which is how the overclaim was found. Kept because
		// it is correct and free, and because the staleness guard is one edit away from
		// not covering this; described accurately so the next reader does not believe a
		// test is watching it.
		_ = tx.Rollback(ctx)
		s.log.Info("billing webhook: subscription event already recorded", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.fail(w, "commit", event.ID, err)
		return
	}

	// MODEL 2 STEP 2 — GRANT THE PERIOD'S ALLOWANCE.
	//
	// ⚠ AFTER THE COMMIT, DELIBERATELY, AND NOT INSIDE IT. The subscription's state
	// is the thing Stripe is telling us; the allowance is a consequence we derive. If
	// the grant were in the same transaction, a failure to grant would roll back the
	// STATE too, we would answer 5xx, and Stripe would redeliver an event whose only
	// real content had already been correct. The grant is idempotent on
	// (subscription, period_start), so a redelivery re-grants nothing and a grant
	// that never happens is repaired by the next event for the same period.
	//
	// ⚠ ONLY WHEN THE EVENT WAS APPLIED and the subscription is actually live. A
	// stale or refused event must not hand out an allowance — that would be the
	// out-of-order bug wearing a different hat, and an expensive one.
	// B27.26: BYOK is a platform fee, not tokens — its period grants no allowance; nor, B32.10, does a company
	// plan's.
	if applied && (status == "active" || status == "trialing") && plan != BYOKPlan && !companyPlans[plan] {
		if end := periodEnd(&sub); end != nil {
			start := periodStart(&sub)
			created, err := s.grantPeriod(ctx, wsID, sub.ID, start, *end, feeOf(&sub))
			if err != nil && !errors.Is(err, ErrNoAllowanceConfigured) {
				// The state is committed and correct; the allowance is repairable.
				// Log rather than 5xx, so Stripe is not asked to redeliver a fact we
				// already have.
				s.log.Error("billing: allowance grant failed (subscription state IS recorded)",
					"event", event.ID, "workspace", wsID, "subscription", sub.ID, "err", err)
			}
			// B18.14 — a period already granted whose price changed: a plan change moves its allowance.
			if err == nil && !created {
				if err := s.applyPlanChange(ctx, event.ID, wsID, sub.ID, priceID, start, *end, feeOf(&sub), eventAt); err != nil {
					s.log.Error("billing: plan change allowance failed (subscription state IS recorded)",
						"event", event.ID, "workspace", wsID, "subscription", sub.ID, "err", err)
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}

// subscriptionReader reads a subscription as Stripe serialises it (B17.21). Optional, like planChangeAPI: a test
// double without it still builds a Service, and its subscription checkouts are only acknowledged.
type subscriptionReader interface {
	SubscriptionJSON(ctx context.Context, subscriptionID string) (json.RawMessage, error)
}

// handleSubscriptionCheckout handles a checkout.session.* event for a SUBSCRIPTION-mode session. It buys no
// LXC, so there is no lxc_purchases row. B17.21 — the subscription it made is read from Stripe and recorded
// exactly as customer.subscription.created would record it, through handleSubscription and its idempotency
// and ordering guards, and the period's allowance is granted. So a subscriber is recorded on an endpoint that
// sends Stripe only checkout events; when customer.subscription.created arrives too, it is the stale
// duplicate those guards already refuse.
func (s *Service) handleSubscriptionCheckout(w http.ResponseWriter, ctx context.Context, event *stripe.Event, sess *stripe.CheckoutSession) {
	subID := ""
	if sess.Subscription != nil {
		subID = sess.Subscription.ID
	}
	wsID := sess.Metadata["workspace_id"]
	reader, ok := s.subStripe.(subscriptionReader)
	if !ok || subID == "" {
		s.log.Info("billing webhook: subscription checkout — no LXC purchase; customer.subscription.* carries it",
			"event", event.ID, "type", string(event.Type), "session", sess.ID, "subscription", subID, "workspace", wsID)
		w.WriteHeader(http.StatusOK)
		return
	}
	// The other kind of workspace's subscription is in the other Service's Stripe mode: not this key's to read.
	if mine, err := s.takes(ctx, wsID, event.Livemode); err != nil {
		s.fail(w, "workspace kind", event.ID, err)
		return
	} else if !mine {
		w.WriteHeader(http.StatusOK)
		return
	}
	raw, err := reader.SubscriptionJSON(ctx, subID)
	if err != nil {
		s.fail(w, "subscription read", event.ID, err)
		return
	}
	carried := *event
	carried.Data = &stripe.EventData{Raw: raw}
	s.handleSubscription(w, ctx, &carried)
}

// periodStart reads the subscription's current period start, falling back to the
// period end minus nothing — an absent start would make the grant's identity
// ambiguous, so it is taken from the payload and only from there.
func periodStart(sub *stripe.Subscription) time.Time {
	return time.Unix(sub.CurrentPeriodStart, 0).UTC()
}

// handleInvoicePaymentFailed records the dunning signal.
//
// ⚠ IT DOES NOT MOVE THE SUBSCRIPTION'S STATUS, and that is deliberate rather than
// incomplete. Stripe decides when a failed invoice becomes `past_due` and sends a
// `customer.subscription.updated` saying so; a handler that ALSO wrote past_due here
// would be a second author of one state machine, and the two would disagree the first
// time Stripe's retry succeeded. This records that a payment failed — the row is the
// evidence — and lets the subscription event move the state.
func (s *Service) handleInvoicePaymentFailed(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	if s.marketInvoiceFailed(w, ctx, event) { // B32.20 — a marketplace bill: Stripe giving up on it ends its licences unpaid
		return
	}
	var inv stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		s.log.Warn("billing webhook: unparseable invoice", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	subID := invoiceSubscriptionID(&inv)
	if subID == "" {
		// A one-off invoice, not a subscription renewal. Nothing here owns it.
		w.WriteHeader(http.StatusOK)
		return
	}

	var wsID string
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM subscriptions WHERE stripe_subscription_id = $1`, subID).Scan(&wsID)
	if errors.Is(err, pgx.ErrNoRows) {
		// An invoice for a subscription we have never seen. Ack: retrying cannot
		// conjure the subscription, and the `.created` event will bring it.
		s.log.Warn("billing webhook: payment failed for unknown subscription", "event", event.ID, "subscription", subID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.fail(w, "invoice workspace lookup", event.ID, err)
		return
	}

	ct, err := s.pool.Exec(ctx, `
		INSERT INTO subscription_events
			(stripe_event_id, stripe_subscription_id, workspace_id, event_type,
			 status_requested, applied, livemode, event_created_at)
		VALUES ($1, $2, $3, $4, $5, TRUE, $6, $7)
		ON CONFLICT (stripe_event_id) DO NOTHING`,
		event.ID, subID, wsID, string(event.Type), "payment_failed",
		event.Livemode, time.Unix(event.Created, 0).UTC())
	if err != nil {
		s.fail(w, "invoice event insert", event.ID, err)
		return
	}
	if ct.RowsAffected() == 0 {
		s.log.Info("billing webhook: invoice event already recorded", "event", event.ID)
	}
	w.WriteHeader(http.StatusOK)
}

// ─── payload readers ─────────────────────────────────────────────────────────────
//
// ⚠ EACH ONE TOLERATES AN ABSENT NESTED OBJECT. Stripe expands some references and
// not others depending on the event, so `sub.Customer` is a struct on one delivery
// and nil on the next. A bare `sub.Customer.ID` panics inside a webhook handler,
// which turns a recoverable event into a 500 and an infinite Stripe retry.

func customerOf(sub *stripe.Subscription) string {
	if sub.Customer == nil {
		return ""
	}
	return sub.Customer.ID
}

// subscriptionPlan reads what sub bills from its items' Prices (B32.10). plan is the plan of its first item
// that is not the BYOK Price — or byok, when that is all it bills — and "" when that Price is no plan's;
// priceID is that item's Price. byok is whether it bills the BYOK Price at all (B27.26, and Team's add-on), or
// is Business, which includes it. All three are empty for a payload with no items.
func (s *Service) subscriptionPlan(sub *stripe.Subscription) (plan, priceID string, byok bool) {
	if sub.Items == nil {
		return "", "", false
	}
	byokPrice := ""
	for _, it := range sub.Items.Data {
		if it == nil || it.Price == nil {
			continue
		}
		p := s.planOfPrice(it.Price)
		switch {
		case p == BYOKPlan:
			byok = true
			if byokPrice == "" {
				byokPrice = it.Price.ID
			}
		case priceID == "":
			plan, priceID = p, it.Price.ID
		}
	}
	if priceID == "" && byok {
		plan, priceID = BYOKPlan, byokPrice
	}
	return plan, priceID, byok || plan == BusinessPlan
}

// planOfPrice names the plan a Price is: by its lookup key, or as the Price this Service sells under that
// plan's name. "" for a Price that is no plan's.
func (s *Service) planOfPrice(pr *stripe.Price) string {
	for plan, key := range PlanLookupKeys {
		if pr.LookupKey == key {
			return plan
		}
	}
	for plan, id := range s.subPlans {
		if pr.ID != "" && pr.ID == id && subscriptionPlans[plan] {
			return plan
		}
	}
	return ""
}

// feeOf is what one period of this subscription is billed at, in US cents: the
// price's unit amount × quantity. 0 when the price is absent or not USD — an unknown
// fee, which caps "earned back" at nothing rather than at a guess.
func feeOf(sub *stripe.Subscription) int64 {
	if sub.Items == nil || len(sub.Items.Data) == 0 {
		return 0
	}
	it := sub.Items.Data[0]
	if it == nil || it.Price == nil || it.Price.Currency != stripe.CurrencyUSD || it.Price.UnitAmount <= 0 {
		return 0
	}
	qty := it.Quantity
	if qty < 1 {
		qty = 1
	}
	return it.Price.UnitAmount * qty
}

// ⚠ MEASURED AGAINST THE SDK, NOT ASSUMED. My first draft read the period off the
// subscription ITEM (`sub.Items.Data[0].CurrentPeriodEnd`) and the invoice's
// subscription off `inv.Parent.SubscriptionDetails`, which is a LATER API shape.
// stripe-go v81.4.0 puts both on the parent object — `go build` said so immediately,
// and the fix is recorded here because the two shapes are easy to confuse and the
// wrong one compiles fine against a newer SDK. B17.42: a webhook event arrives in the
// later shape all the same — periodFromItems copies its period up before this reads it.
func periodEnd(sub *stripe.Subscription) *time.Time {
	if sub.CurrentPeriodEnd == 0 {
		return nil
	}
	t := time.Unix(sub.CurrentPeriodEnd, 0).UTC()
	return &t
}

// periodFromItems gives a subscription that names no period of its own the period of its first item that
// names one (B17.42). From Stripe API 2025-03-31 current_period_start and _end are on the ITEMS, and an event
// is serialised at the webhook endpoint's API version, not stripe-go v81's — so every
// customer.subscription.updated a cancel or resume provoked wrote the row's period end NULL, and Lens named no
// day the plan ends; a .created in that shape granted no allowance at all.
func periodFromItems(raw json.RawMessage, sub *stripe.Subscription) {
	if sub.CurrentPeriodEnd != 0 {
		return
	}
	var v struct {
		Items struct {
			Data []struct {
				Start int64 `json:"current_period_start"`
				End   int64 `json:"current_period_end"`
			} `json:"data"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	for _, it := range v.Items.Data {
		if it.End != 0 {
			sub.CurrentPeriodStart, sub.CurrentPeriodEnd = it.Start, it.End
			return
		}
	}
}

func invoiceSubscriptionID(inv *stripe.Invoice) string {
	if inv.Subscription == nil {
		return ""
	}
	return inv.Subscription.ID
}
