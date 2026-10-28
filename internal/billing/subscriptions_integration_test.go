package billing

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bytes"

	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"
)

// MODEL 2 STEP 1 — real-PG subscription tests. W4.6.1.
//
// ⚠ EVERY ASSERTION HERE READS A ROW. W4.6.1: "ASSERT THE LEDGER ROW, NEVER A STATUS
// CODE." A webhook handler that returns 200 has told you nothing — Stripe retries on
// non-2xx, so the ONLY safe thing a handler can do with an event it cannot act on is
// ack it, which means 200 is also what "deliberately did nothing" looks like. The
// status code is checked where it is load-bearing (a 5xx must provoke redelivery) and
// never as evidence that something happened.
//
// ⚠ HONEST NOTE ON ORDER: this file was written AFTER the implementation, not before
// it. Red-first was not followed for W4.6.1 step 1, and saying so is better than
// implying otherwise. What stands in for it is scripts/w461-subscription-controls-q4vn.py,
// which mutates the implementation once per behaviour and requires each test below to
// go RED on its own — the same evidence red-first produces, obtained afterwards.

// signedAt is `signed` plus the one field the out-of-order guard turns on: Stripe's
// `event.created`. The existing helper omits it, which makes every event tie at the
// Unix epoch — and a test whose events all share a timestamp cannot tell an ordering
// bug from correct behaviour.
func signedAt(secret, eventID, eventType string, created time.Time, object map[string]any) ([]byte, string) {
	body, _ := json.Marshal(map[string]any{
		"id":       eventID,
		"type":     eventType,
		"created":  created.Unix(),
		"livemode": true, // a real workspace's subscription: a test-mode event never changes its plan (B50.13)
		"data":     map[string]any{"object": object},
	})
	now := time.Now()
	sig := webhook.ComputeSignature(now, body, secret)
	return body, fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(sig))
}

func subObj(subID, wsID, customerID, priceID, status string, periodEnd time.Time, cancelAtEnd bool) map[string]any {
	return map[string]any{
		"id":                   subID,
		"status":               status,
		"customer":             map[string]any{"id": customerID},
		"cancel_at_period_end": cancelAtEnd,
		"current_period_end":   periodEnd.Unix(),
		"metadata":             map[string]string{"workspace_id": wsID},
		"items": map[string]any{
			"data": []any{map[string]any{"price": map[string]any{"id": priceID}}},
		},
	}
}

func postEvent(svc *Service, body []byte, sig string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	svc.HandleWebhook(rec, req)
	return rec.Code
}

// resetSubs clears only the two tables this file owns. Row-level DELETE, matching
// `reset` above — a TRUNCATE on a busy shared test DB is a lock flake waiting.
func resetSubs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, tbl := range []string{"subscription_events", "subscriptions"} {
		if _, err := pool.Exec(context.Background(), "DELETE FROM "+tbl); err != nil {
			t.Fatalf("reset %s: %v", tbl, err)
		}
	}
}

type subRow struct {
	status      string
	cancelAtEnd bool
	lastEventAt time.Time
	periodEnd   *time.Time
}

func readSub(t *testing.T, pool *pgxpool.Pool, subID string) (subRow, bool) {
	t.Helper()
	var r subRow
	err := pool.QueryRow(context.Background(), `
		SELECT status, cancel_at_period_end, last_event_at, current_period_end
		FROM subscriptions WHERE stripe_subscription_id = $1`, subID).
		Scan(&r.status, &r.cancelAtEnd, &r.lastEventAt, &r.periodEnd)
	if err != nil {
		return subRow{}, false
	}
	return r, true
}

func readEvent(t *testing.T, pool *pgxpool.Pool, eventID string) (applied bool, statusReq string, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT applied, status_requested FROM subscription_events WHERE stripe_event_id = $1`, eventID).
		Scan(&applied, &statusReq)
	if err != nil {
		return false, "", false
	}
	return applied, statusReq, true
}

func countEvents(t *testing.T, pool *pgxpool.Pool, subID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM subscription_events WHERE stripe_subscription_id = $1`, subID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

type fakeSubStripe struct {
	sessions  int
	lastPrice string
	updates   []fakeSubUpdate
}

type fakeSubUpdate struct {
	subID  string
	cancel bool
}

func (f *fakeSubStripe) SetCancelAtPeriodEnd(_ context.Context, subID string, cancel bool) (*stripe.Subscription, error) {
	f.updates = append(f.updates, fakeSubUpdate{subID, cancel})
	return &stripe.Subscription{ID: subID, Status: stripe.SubscriptionStatusActive, CancelAtPeriodEnd: cancel,
		CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour).Unix()}, nil
}

func (f *fakeSubStripe) CreateSubscriptionCheckoutSession(_ context.Context, p SubscriptionParams) (string, string, error) {
	f.sessions++
	f.lastPrice = p.PriceID
	return "https://checkout.stripe.test/sub/" + p.WorkspaceID, "cs_sub_" + p.WorkspaceID, nil
}

func newSubService(t *testing.T) (*Service, *pgxpool.Pool, *fakeSubStripe) {
	t.Helper()
	svc, pool, _ := newBillingService(t)
	resetSubs(t, pool)
	fss := &fakeSubStripe{}
	return svc.WithSubscriptions(fss, "price_test_model2"), pool, fss
}

// ── the happy path, and it is a ROW ──────────────────────────────────────────────

func TestSubscription_Created_MarksWorkspaceSubscribed(t *testing.T) {
	svc, pool, _ := newSubService(t)
	seedWS(t, pool, "ws-sub-1")
	end := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)

	body, sig := signedAt(testWebhookSecret, "evt_sub_created_1", "customer.subscription.created",
		time.Now(), subObj("sub_1", "ws-sub-1", "cus_1", "price_test_model2", "active", end, false))
	if code := postEvent(svc, body, sig); code != http.StatusOK {
		t.Fatalf("webhook code = %d, want 200", code)
	}

	row, ok := readSub(t, pool, "sub_1")
	if !ok {
		t.Fatal("no subscriptions row — the 200 said nothing about whether the workspace is subscribed")
	}
	if row.status != "active" {
		t.Errorf("status = %q, want active", row.status)
	}
	if row.periodEnd == nil || !row.periodEnd.Equal(end.UTC()) {
		t.Errorf("current_period_end = %v, want %v", row.periodEnd, end.UTC())
	}
	applied, _, ok := readEvent(t, pool, "evt_sub_created_1")
	if !ok || !applied {
		t.Errorf("subscription_events: ok=%v applied=%v, want a row with applied=true", ok, applied)
	}

	st, err := svc.GetSubscription(context.Background(), "ws-sub-1")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if !st.Subscribed || st.Status != "active" {
		t.Errorf("GetSubscription = %+v, want subscribed active", st)
	}
}

// ── idempotency: Stripe retries, and a retry must not act twice ──────────────────

func TestSubscription_SameEventTwice_OneEventRow(t *testing.T) {
	svc, pool, _ := newSubService(t)
	seedWS(t, pool, "ws-sub-2")
	end := time.Now().Add(30 * 24 * time.Hour)
	body, sig := signedAt(testWebhookSecret, "evt_dup", "customer.subscription.created",
		time.Now(), subObj("sub_2", "ws-sub-2", "cus_2", "price_test_model2", "active", end, false))

	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("first delivery = %d", c)
	}
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("redelivery = %d, want 200 (already recorded → ack)", c)
	}
	if n := countEvents(t, pool, "sub_2"); n != 1 {
		t.Errorf("subscription_events rows = %d, want 1 — the redelivery acted twice", n)
	}
}

// ── ⚠ THE ONE THIS ITEM NAMES: out-of-order delivery ─────────────────────────────

func TestSubscription_StaleEventDoesNotOverwriteNewerState(t *testing.T) {
	svc, pool, _ := newSubService(t)
	seedWS(t, pool, "ws-sub-3")
	end := time.Now().Add(30 * 24 * time.Hour)
	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()

	// The NEWER event lands first: the card recovered and Stripe says active.
	b1, s1 := signedAt(testWebhookSecret, "evt_new", "customer.subscription.created",
		newer, subObj("sub_3", "ws-sub-3", "cus_3", "price_test_model2", "active", end, false))
	if c := postEvent(svc, b1, s1); c != http.StatusOK {
		t.Fatalf("newer event = %d", c)
	}

	// Then the OLDER past_due arrives — Stripe guarantees delivery, not order.
	b2, s2 := signedAt(testWebhookSecret, "evt_old", "customer.subscription.updated",
		older, subObj("sub_3", "ws-sub-3", "cus_3", "price_test_model2", "past_due", end, false))
	if c := postEvent(svc, b2, s2); c != http.StatusOK {
		t.Fatalf("older event = %d, want 200 (recorded, not applied)", c)
	}

	row, ok := readSub(t, pool, "sub_3")
	if !ok {
		t.Fatal("subscription row vanished")
	}
	if row.status != "active" {
		t.Errorf("status = %q, want active — a STALE past_due overwrote newer state, "+
			"which duns a customer who has already paid", row.status)
	}
	// ⚠ AND IT IS RECORDED RATHER THAN DROPPED: an operator must be able to see that
	// Stripe sent it and that we refused it.
	applied, statusReq, ok := readEvent(t, pool, "evt_old")
	if !ok {
		t.Fatal("the stale event was not recorded at all — it must be visible, not silently dropped")
	}
	if applied {
		t.Error("the stale event is recorded as applied=true")
	}
	if statusReq != "past_due" {
		t.Errorf("status_requested = %q, want past_due — the row must say what was asked for", statusReq)
	}
}

// ── failed payment: recorded, and it does NOT author the state machine ───────────

func TestSubscription_InvoicePaymentFailed_RecordedStatusUnmoved(t *testing.T) {
	svc, pool, _ := newSubService(t)
	seedWS(t, pool, "ws-sub-4")
	end := time.Now().Add(30 * 24 * time.Hour)
	b1, s1 := signedAt(testWebhookSecret, "evt_c4", "customer.subscription.created",
		time.Now(), subObj("sub_4", "ws-sub-4", "cus_4", "price_test_model2", "active", end, false))
	postEvent(svc, b1, s1)

	b2, s2 := signedAt(testWebhookSecret, "evt_fail_4", "invoice.payment_failed", time.Now(),
		map[string]any{"id": "in_4", "subscription": map[string]any{"id": "sub_4"}})
	if c := postEvent(svc, b2, s2); c != http.StatusOK {
		t.Fatalf("payment_failed = %d", c)
	}

	applied, statusReq, ok := readEvent(t, pool, "evt_fail_4")
	if !ok || !applied || statusReq != "payment_failed" {
		t.Errorf("payment_failed row: ok=%v applied=%v status=%q, want recorded", ok, applied, statusReq)
	}
	// ⚠ Stripe decides when a failed invoice becomes past_due and says so in its OWN
	// event. A handler that also wrote past_due here would be a second author of one
	// state machine, and the two would disagree the moment a retry succeeded.
	row, _ := readSub(t, pool, "sub_4")
	if row.status != "active" {
		t.Errorf("status = %q — invoice.payment_failed must not move the subscription itself", row.status)
	}
}

// ── cancellation is terminal ─────────────────────────────────────────────────────

func TestSubscription_Deleted_IsTerminal_AndNotSubscribed(t *testing.T) {
	svc, pool, _ := newSubService(t)
	seedWS(t, pool, "ws-sub-5")
	end := time.Now().Add(30 * 24 * time.Hour)
	b1, s1 := signedAt(testWebhookSecret, "evt_c5", "customer.subscription.created",
		time.Now().Add(-time.Hour), subObj("sub_5", "ws-sub-5", "cus_5", "price_test_model2", "active", end, false))
	postEvent(svc, b1, s1)

	b2, s2 := signedAt(testWebhookSecret, "evt_del_5", "customer.subscription.deleted",
		time.Now(), subObj("sub_5", "ws-sub-5", "cus_5", "price_test_model2", "active", end, false))
	postEvent(svc, b2, s2)

	row, _ := readSub(t, pool, "sub_5")
	if row.status != "canceled" {
		t.Fatalf("status = %q, want canceled — the event TYPE is the fact, not the object's status", row.status)
	}
	st, _ := svc.GetSubscription(context.Background(), "ws-sub-5")
	if st.Subscribed {
		t.Error("GetSubscription still reports subscribed after cancellation")
	}

	// ⚠ AND IT CANNOT BE REOPENED by a later event — a cancelled subscription that
	// silently comes back is a customer billed for a product they cancelled.
	b3, s3 := signedAt(testWebhookSecret, "evt_zombie_5", "customer.subscription.updated",
		time.Now().Add(time.Hour), subObj("sub_5", "ws-sub-5", "cus_5", "price_test_model2", "active", end, false))
	postEvent(svc, b3, s3)
	row, _ = readSub(t, pool, "sub_5")
	if row.status != "canceled" {
		t.Errorf("status = %q — a terminal subscription was reopened", row.status)
	}
	applied, _, _ := readEvent(t, pool, "evt_zombie_5")
	if applied {
		t.Error("the reopening event is recorded as applied=true")
	}
}

// ── one live subscription per workspace ──────────────────────────────────────────

func TestSubscription_SecondLiveSubscription_NotApplied(t *testing.T) {
	svc, pool, _ := newSubService(t)
	seedWS(t, pool, "ws-sub-6")
	end := time.Now().Add(30 * 24 * time.Hour)
	b1, s1 := signedAt(testWebhookSecret, "evt_c6a", "customer.subscription.created",
		time.Now(), subObj("sub_6a", "ws-sub-6", "cus_6", "price_test_model2", "active", end, false))
	postEvent(svc, b1, s1)

	b2, s2 := signedAt(testWebhookSecret, "evt_c6b", "customer.subscription.created",
		time.Now(), subObj("sub_6b", "ws-sub-6", "cus_6", "price_test_model2", "active", end, false))
	if c := postEvent(svc, b2, s2); c != http.StatusOK {
		t.Fatalf("second subscription = %d, want 200 (recorded, not applied)", c)
	}

	var live int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM subscriptions WHERE workspace_id = $1 AND status IN ('trialing','active','past_due','unpaid')`,
		"ws-sub-6").Scan(&live); err != nil {
		t.Fatalf("count live: %v", err)
	}
	if live != 1 {
		t.Errorf("live subscriptions = %d, want 1", live)
	}
	// ⚠ Money has already moved for the second one, so it must be VISIBLE.
	applied, _, ok := readEvent(t, pool, "evt_c6b")
	if !ok {
		t.Fatal("the second subscription was not recorded — an operator cannot see the double charge")
	}
	if applied {
		t.Error("the second live subscription was applied")
	}
}

// ── past_due is still paying; unpaid is not ──────────────────────────────────────

func TestSubscription_PastDueIsSubscribed_UnpaidIsNot(t *testing.T) {
	svc, pool, _ := newSubService(t)
	seedWS(t, pool, "ws-sub-7")
	end := time.Now().Add(30 * 24 * time.Hour)
	b1, s1 := signedAt(testWebhookSecret, "evt_c7", "customer.subscription.created",
		time.Now().Add(-2*time.Hour), subObj("sub_7", "ws-sub-7", "cus_7", "price_test_model2", "active", end, false))
	postEvent(svc, b1, s1)

	b2, s2 := signedAt(testWebhookSecret, "evt_pd7", "customer.subscription.updated",
		time.Now().Add(-time.Hour), subObj("sub_7", "ws-sub-7", "cus_7", "price_test_model2", "past_due", end, false))
	postEvent(svc, b2, s2)
	st, _ := svc.GetSubscription(context.Background(), "ws-sub-7")
	if !st.Subscribed {
		t.Error("past_due reports NOT subscribed — Stripe is still retrying the card, and " +
			"cutting service on the first failure loses a paying customer to an expired card")
	}

	b3, s3 := signedAt(testWebhookSecret, "evt_up7", "customer.subscription.updated",
		time.Now(), subObj("sub_7", "ws-sub-7", "cus_7", "price_test_model2", "unpaid", end, false))
	postEvent(svc, b3, s3)
	st, _ = svc.GetSubscription(context.Background(), "ws-sub-7")
	if st.Subscribed {
		t.Error("unpaid reports subscribed — Stripe has given up retrying, the answer is no")
	}
}

// ── the checkout refusals ────────────────────────────────────────────────────────

func TestSubscriptionCheckout_NoPriceConfigured_Refuses(t *testing.T) {
	svc, pool, _ := newBillingService(t) // NOT WithSubscriptions
	resetSubs(t, pool)
	seedWS(t, pool, "ws-sub-8")
	if _, err := svc.CreateSubscriptionCheckout(context.Background(), "ws-sub-8"); err == nil {
		t.Fatal("a deployment with no subscription price created a checkout anyway")
	}
}

func TestSubscriptionCheckout_AlreadySubscribed_RefusesBeforeStripe(t *testing.T) {
	svc, pool, fss := newSubService(t)
	seedWS(t, pool, "ws-sub-9")
	end := time.Now().Add(30 * 24 * time.Hour)
	b1, s1 := signedAt(testWebhookSecret, "evt_c9", "customer.subscription.created",
		time.Now(), subObj("sub_9", "ws-sub-9", "cus_9", "price_test_model2", "active", end, false))
	postEvent(svc, b1, s1)

	before := fss.sessions
	if _, err := svc.CreateSubscriptionCheckout(context.Background(), "ws-sub-9"); err == nil {
		t.Fatal("a workspace that already pays was sent to a second checkout")
	}
	// ⚠ BEFORE STRIPE, not after: the refusal must not cost a Checkout Session, because
	// the collision the index would catch happens on the webhook — where money has moved.
	if fss.sessions != before {
		t.Errorf("Stripe was called %d time(s) for a refused checkout", fss.sessions-before)
	}
}

func TestSubscriptionCheckout_HappyPath_CallsStripeOnce(t *testing.T) {
	svc, pool, fss := newSubService(t)
	seedWS(t, pool, "ws-sub-10")
	url, err := svc.CreateSubscriptionCheckout(context.Background(), "ws-sub-10")
	if err != nil {
		t.Fatalf("CreateSubscriptionCheckout: %v", err)
	}
	if url == "" {
		t.Error("empty checkout url")
	}
	if fss.sessions != 1 {
		t.Errorf("stripe sessions = %d, want 1", fss.sessions)
	}
}

// B13.1 — a named plan checks out at that plan's price; an unknown plan is refused before Stripe.
func TestSubscriptionCheckout_NamedPlan_SendsItsPrice_UnknownPlanRefusedBeforeStripe(t *testing.T) {
	svc, pool, fss := newSubService(t)
	svc = svc.WithPlans(fss, map[string]string{"plus": "price_plus", "pro": "price_pro", "max": "price_max"})
	seedWS(t, pool, "ws-sub-plan")
	if _, err := svc.CreatePlanCheckout(context.Background(), "ws-sub-plan", "pro"); err != nil {
		t.Fatalf("CreatePlanCheckout(pro): %v", err)
	}
	if fss.lastPrice != "price_pro" {
		t.Errorf("Stripe was sent price %q, want price_pro", fss.lastPrice)
	}
	before := fss.sessions
	if _, err := svc.CreatePlanCheckout(context.Background(), "ws-sub-plan", "gold"); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("unknown plan: err = %v, want ErrUnknownPlan", err)
	}
	if fss.sessions != before {
		t.Errorf("Stripe was called for an unknown plan")
	}
}

// ── B1.5: cancel, and the flip back ──────────────────────────────────────────────

// The whole lifecycle a customer sees: subscribed → cancel (still subscribed, the
// period is paid for) → Stripe's .deleted at period end → not subscribed, and
// nothing left to cancel. Every step after the Stripe call is read from the ROW the
// webhook wrote, because SetCancelAtPeriodEnd itself writes none.
func TestSubscription_CancelAtPeriodEnd_ThenDeleted_FlipsBack(t *testing.T) {
	svc, pool, fss := newSubService(t)
	seedWS(t, pool, "ws-sub-cancel")
	ctx := context.Background()
	end := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	t0 := time.Now().Add(-time.Minute)

	body, sig := signedAt(testWebhookSecret, "evt_cancel_1", "customer.subscription.created",
		t0, subObj("sub_cancel", "ws-sub-cancel", "cus_c", "price_test_model2", "active", end, false))
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("created = %d", c)
	}

	st, err := svc.SetCancelAtPeriodEnd(ctx, "ws-sub-cancel", true)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(fss.updates) != 1 || fss.updates[0] != (fakeSubUpdate{"sub_cancel", true}) {
		t.Fatalf("Stripe updates = %+v, want one cancel of sub_cancel", fss.updates)
	}
	if !st.CancelAtPeriodEnd || !st.Subscribed {
		t.Errorf("cancel answer = %+v, want cancel_at_period_end and still subscribed", st)
	}

	body, sig = signedAt(testWebhookSecret, "evt_cancel_2", "customer.subscription.updated",
		t0.Add(10*time.Second), subObj("sub_cancel", "ws-sub-cancel", "cus_c", "price_test_model2", "active", end, true))
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("updated = %d", c)
	}
	if row, _ := readSub(t, pool, "sub_cancel"); !row.cancelAtEnd || row.status != "active" {
		t.Errorf("row after cancel = %+v, want active with cancel_at_period_end", row)
	}
	if got, _ := svc.GetSubscription(ctx, "ws-sub-cancel"); !got.Subscribed {
		t.Errorf("GetSubscription after cancel = %+v, want still subscribed until period end", got)
	}

	body, sig = signedAt(testWebhookSecret, "evt_cancel_3", "customer.subscription.deleted",
		t0.Add(20*time.Second), subObj("sub_cancel", "ws-sub-cancel", "cus_c", "price_test_model2", "canceled", end, true))
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("deleted = %d", c)
	}
	if row, _ := readSub(t, pool, "sub_cancel"); row.status != "canceled" {
		t.Errorf("row status after period end = %q, want canceled", row.status)
	}
	if got, _ := svc.GetSubscription(ctx, "ws-sub-cancel"); got.Subscribed {
		t.Errorf("GetSubscription after .deleted = %+v, want not subscribed", got)
	}

	if _, err := svc.SetCancelAtPeriodEnd(ctx, "ws-sub-cancel", true); !errors.Is(err, ErrNoLiveSubscription) {
		t.Errorf("second cancel err = %v, want ErrNoLiveSubscription", err)
	}
	if len(fss.updates) != 1 {
		t.Errorf("Stripe called %d times, want 1 — a cancel with nothing live must not reach Stripe", len(fss.updates))
	}
}

func TestSubscription_Resume_ClearsCancelAtPeriodEnd(t *testing.T) {
	svc, pool, fss := newSubService(t)
	seedWS(t, pool, "ws-sub-resume")
	end := time.Now().Add(30 * 24 * time.Hour)
	body, sig := signedAt(testWebhookSecret, "evt_resume_1", "customer.subscription.created",
		time.Now(), subObj("sub_resume", "ws-sub-resume", "cus_r", "price_test_model2", "active", end, true))
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("created = %d", c)
	}

	st, err := svc.SetCancelAtPeriodEnd(context.Background(), "ws-sub-resume", false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(fss.updates) != 1 || fss.updates[0] != (fakeSubUpdate{"sub_resume", false}) {
		t.Fatalf("Stripe updates = %+v, want one resume of sub_resume", fss.updates)
	}
	if st.CancelAtPeriodEnd {
		t.Errorf("resume answer = %+v, want cancel_at_period_end cleared", st)
	}
}
