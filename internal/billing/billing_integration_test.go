package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/migrations"
)

// Real-PG billing money-path tests (LENS_TEST_DATABASE_URL-gated, the CI
// real-migration job sets it). The partial index idx_lxc_purchases_session_credited,
// ON CONFLICT/RowsAffected, FOR UPDATE and tx isolation are the money guarantee —
// pgxmock cannot model them faithfully, so these run against a real Postgres with
// the REAL 0054 schema applied.

const testWebhookSecret = "whsec_test_secret_0123456789"

var migrateOnce sync.Once

func newBillingService(t *testing.T) (*Service, *pgxpool.Pool, *economy.DualTokenStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG billing tests")
	}
	ctx := context.Background()
	migrateOnce.Do(func() {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	reset(t, pool)

	ledger := mining.NewLedgerStore(pool)
	engine := economy.NewRateEngine(ledger, pool)
	dt := economy.NewDualTokenStore(ledger, pool, engine)
	svc := New(pool, dt, &fakeStripe{}, testWebhookSecret)
	return svc, pool, dt
}

func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// lxc_ledger is APPEND-ONLY since U14 (migration 0055) and is intentionally NOT
	// reset here — these tests assert on lxc_balances + lxc_purchases and use unique
	// workspace ids, so accumulated ledger rows are harmless. lxc_balances IS reset
	// (it is not guarded) for balance isolation. DELETE (row-level) avoids the
	// partitioned-TRUNCATE lock flake.
	for _, tbl := range []string{"lxc_purchases", "billing_customers", "lxc_balances"} {
		if _, err := pool.Exec(context.Background(), "DELETE FROM "+tbl); err != nil {
			t.Fatalf("reset %s: %v", tbl, err)
		}
	}
}

func seedWS(t *testing.T, pool *pgxpool.Pool, wsID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1,$2,$3) ON CONFLICT (id) DO NOTHING`,
		wsID, wsID, wsID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// ─── Stripe test doubles ──────────────────────────────────────────────

type fakeStripe struct {
	customers      int
	sessions       int
	last           CheckoutParams // the most recent CreateCheckoutSession input
	fingerprint    string         // returned by CardFingerprint
	fingerprintErr error          // when set, CardFingerprint errors (the capture must swallow it)
}

func (f *fakeStripe) CreateCustomer(_ context.Context, workspaceID string) (string, error) {
	f.customers++
	return "cus_" + workspaceID, nil
}

func (f *fakeStripe) CreateCheckoutSession(_ context.Context, p CheckoutParams) (string, string, error) {
	f.sessions++
	f.last = p
	return "https://checkout.stripe.test/pay/" + p.WorkspaceID, "cs_test_" + p.WorkspaceID, nil
}

func (f *fakeStripe) CardFingerprint(_ context.Context, _ string) (string, error) {
	return f.fingerprint, f.fingerprintErr
}

// flakyCrediter fails the FIRST CreditLXCTx (simulating a transient credit
// failure), then delegates — to prove a 5xx + redelivery credits exactly once.
type flakyCrediter struct {
	inner    lxcCrediter
	failNext atomic.Bool
}

func (f *flakyCrediter) CreditLXCTx(ctx context.Context, tx pgx.Tx, ws string, amt int64, reason string, md map[string]interface{}) (int64, error) {
	if f.failNext.Swap(false) {
		return 0, errors.New("simulated credit failure")
	}
	return f.inner.CreditLXCTx(ctx, tx, ws, amt, reason, md)
}

// ─── signed-event fixtures (offline HMAC, the Stripe scheme) ───────────

func signed(secret, eventID, eventType string, object map[string]any) ([]byte, string) {
	// livemode: these are the real workspaces' payments, which a test-mode event never credits (B50.13).
	body, _ := json.Marshal(map[string]any{
		"id":       eventID,
		"type":     eventType,
		"livemode": true,
		"data":     map[string]any{"object": object},
	})
	// Build the Stripe-Signature header with the SDK's OWN signer so the test can
	// never drift from ConstructEvent's verification scheme.
	now := time.Now()
	sig := webhook.ComputeSignature(now, body, secret)
	return body, fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(sig))
}

func sessionObj(sessID, wsID string, usdCents int64, currency, payStatus, pi string, metaLXC int64) map[string]any {
	return map[string]any{
		"id":             sessID,
		"amount_total":   usdCents,
		"currency":       currency,
		"payment_status": payStatus,
		"payment_intent": pi,
		"metadata": map[string]string{
			"workspace_id": wsID,
			"lxc_amount":   strconv.FormatInt(metaLXC, 10),
			"usd_cents":    strconv.FormatInt(usdCents, 10),
		},
	}
}

func post(svc *Service, body []byte, sig string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	rec := httptest.NewRecorder()
	svc.HandleWebhook(rec, req)
	return rec.Code
}

func balance(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	err := pool.QueryRow(context.Background(),
		`SELECT balance FROM lxc_balances WHERE workspace_id=$1`, ws).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return b
}

func sessionRows(t *testing.T, pool *pgxpool.Pool, sessID string) (count int, sumLXC int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(sum(lxc_amount),0) FROM lxc_purchases WHERE stripe_session_id=$1`,
		sessID).Scan(&count, &sumLXC); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return
}

// ─── THE money test: same completion event twice ⇒ exactly one credit ──

func TestWebhook_Idempotency_SameEventTwice_OneCredit(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, evt = "ws_idem", "cs_idem", "evt_idem_1"
	seedWS(t, pool, ws)

	body, sig := signed(testWebhookSecret, evt, "checkout.session.completed",
		sessionObj(sess, ws, 1000, "usd", "paid", "pi_idem", micro(100))) // 1000c/$0.10 = 100 LXC

	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("first delivery: got %d, want 200", got)
	}
	if got := post(svc, body, sig); got != http.StatusOK { // SAME event id again
		t.Fatalf("second delivery: got %d, want 200 (idempotent ack)", got)
	}

	if c, sum := sessionRows(t, pool, sess); c != 1 || sum != micro(100) {
		t.Errorf("rows=%d sumLXC=%v, want exactly 1 row crediting 100", c, sum)
	}
	if b := balance(t, pool, ws); b != micro(100) {
		t.Errorf("balance=%v, want 100 (credited ONCE)", b)
	}
}

func TestWebhook_BadSignature_400_NoWrites(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess = "ws_badsig", "cs_badsig"
	seedWS(t, pool, ws)
	body, _ := signed(testWebhookSecret, "evt_badsig", "checkout.session.completed",
		sessionObj(sess, ws, 1000, "usd", "paid", "pi_x", micro(100)))

	if got := post(svc, body, "t=1,v1=deadbeef"); got != http.StatusBadRequest {
		t.Fatalf("bad signature: got %d, want 400", got)
	}
	if c, _ := sessionRows(t, pool, sess); c != 0 {
		t.Errorf("bad signature must write ZERO rows; got %d", c)
	}
	if b := balance(t, pool, ws); b != 0 {
		t.Errorf("bad signature must not credit; balance=%v", b)
	}
}

func TestWebhook_AmountMismatch_Anomalous_NoCredit(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess = "ws_mismatch", "cs_mismatch"
	seedWS(t, pool, ws)
	// usd 1000c ⇒ recompute 100 LXC, but metadata claims 9999 → refuse + anomaly.
	body, sig := signed(testWebhookSecret, "evt_mismatch", "checkout.session.completed",
		sessionObj(sess, ws, 1000, "usd", "paid", "pi_m", micro(9999)))

	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("got %d, want 200 (anomalous claimed)", got)
	}
	if b := balance(t, pool, ws); b != 0 {
		t.Errorf("amount mismatch must NOT credit; balance=%v", b)
	}
	assertStatus(t, pool, sess, "anomalous")
}

func TestWebhook_UnknownWorkspace_Refused_NoCredit(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const sess = "cs_unknownws"
	// NOTE: workspace "ws_ghost" is deliberately NOT seeded.
	body, sig := signed(testWebhookSecret, "evt_unknownws", "checkout.session.completed",
		sessionObj(sess, "ws_ghost", 1000, "usd", "paid", "pi_g", micro(100)))

	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("got %d, want 200 (anomalous claimed)", got)
	}
	if b := balance(t, pool, "ws_ghost"); b != 0 {
		t.Errorf("unknown workspace must NOT credit; balance=%v", b)
	}
	assertStatus(t, pool, sess, "anomalous")
}

func TestWebhook_CurrencyNotUSD_Anomalous(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess = "ws_eur", "cs_eur"
	seedWS(t, pool, ws)
	body, sig := signed(testWebhookSecret, "evt_eur", "checkout.session.completed",
		sessionObj(sess, ws, 1000, "eur", "paid", "pi_e", micro(100)))
	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("got %d, want 200", got)
	}
	if b := balance(t, pool, ws); b != 0 {
		t.Errorf("non-USD must NOT credit; balance=%v", b)
	}
	assertStatus(t, pool, sess, "anomalous")
}

func TestWebhook_NonpositiveAndDisallowed_Anomalous(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	seedWS(t, pool, "ws_amt")
	for name, cents := range map[string]int64{"zero": 0, "negative": -500, "below_min": 999, "above_max": 1_000_001} {
		t.Run(name, func(t *testing.T) {
			sess := "cs_amt_" + name
			lxc := lxcForCents(cents)
			body, sig := signed(testWebhookSecret, "evt_amt_"+name, "checkout.session.completed",
				sessionObj(sess, "ws_amt", cents, "usd", "paid", "pi_"+name, lxc))
			if got := post(svc, body, sig); got != http.StatusOK {
				t.Fatalf("got %d, want 200", got)
			}
			if b := balance(t, pool, "ws_amt"); b != 0 {
				t.Errorf("%s amount must NOT credit; balance=%v", name, b)
			}
			assertStatus(t, pool, sess, "anomalous")
		})
	}
}

func TestWebhook_Refund_MarksRefunded_BalanceUnchanged(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, pi = "ws_refund", "cs_refund", "pi_refund"
	seedWS(t, pool, ws)
	// credit first.
	body, sig := signed(testWebhookSecret, "evt_refund_pay", "checkout.session.completed",
		sessionObj(sess, ws, 5000, "usd", "paid", pi, micro(500)))
	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("credit: got %d", got)
	}
	if b := balance(t, pool, ws); b != micro(500) {
		t.Fatalf("pre-refund balance=%v µLXC, want micro(500)", b)
	}
	// refund the charge (correlated by payment_intent).
	rb, rsig := signed(testWebhookSecret, "evt_refund_chg", "charge.refunded",
		map[string]any{"id": "ch_refund", "payment_intent": pi})
	if got := post(svc, rb, rsig); got != http.StatusOK {
		t.Fatalf("refund: got %d", got)
	}
	if b := balance(t, pool, ws); b != micro(500) {
		t.Errorf("v1 refund must NOT claw back; balance=%v, want 500 unchanged", b)
	}
	assertStatus(t, pool, sess, "refunded")
}

// ─── Amendment 1: async double-credit guard ───────────────────────────

func TestWebhook_AsyncDoubleCredit_OneCredit(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess = "ws_async", "cs_async"
	seedWS(t, pool, ws)

	// completed(PAID) credits.
	cb, cs := signed(testWebhookSecret, "evt_async_completed", "checkout.session.completed",
		sessionObj(sess, ws, 1000, "usd", "paid", "pi_async", micro(100)))
	if got := post(svc, cb, cs); got != http.StatusOK {
		t.Fatalf("completed: got %d", got)
	}
	// a stray async_payment_succeeded for the SAME session, DIFFERENT event id.
	ab, as := signed(testWebhookSecret, "evt_async_succeeded", "checkout.session.async_payment_succeeded",
		sessionObj(sess, ws, 1000, "usd", "paid", "pi_async", micro(100)))
	if got := post(svc, ab, as); got != http.StatusOK {
		t.Fatalf("async after completed: got %d, want 200 (blocked, already credited)", got)
	}

	if c, sum := sessionRows(t, pool, sess); c != 1 || sum != micro(100) {
		t.Errorf("rows=%d sumLXC=%v, want exactly ONE credit of 100", c, sum)
	}
	if b := balance(t, pool, ws); b != micro(100) {
		t.Errorf("balance=%v, want 100 (credited exactly once across completed+async)", b)
	}
}

func TestWebhook_UnpaidCompletedThenAsync_CreditsOnce(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess = "ws_delayed", "cs_delayed"
	seedWS(t, pool, ws)

	// delayed method: completed arrives UNPAID first → no claim, no credit.
	ub, us := signed(testWebhookSecret, "evt_delayed_unpaid", "checkout.session.completed",
		sessionObj(sess, ws, 1000, "usd", "unpaid", "pi_delayed", micro(100)))
	if got := post(svc, ub, us); got != http.StatusOK {
		t.Fatalf("unpaid completed: got %d", got)
	}
	if c, _ := sessionRows(t, pool, sess); c != 0 {
		t.Fatalf("unpaid completed must NOT consume an idempotency row; rows=%d", c)
	}
	// then the money settles via async_payment_succeeded → credit.
	ab, as := signed(testWebhookSecret, "evt_delayed_succeeded", "checkout.session.async_payment_succeeded",
		sessionObj(sess, ws, 1000, "usd", "paid", "pi_delayed", micro(100)))
	if got := post(svc, ab, as); got != http.StatusOK {
		t.Fatalf("async succeeded: got %d", got)
	}
	if b := balance(t, pool, ws); b != micro(100) {
		t.Errorf("balance=%v, want 100 (credited on async only)", b)
	}
}

func TestWebhook_AsyncFailed_NoCredit(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess = "ws_asyncfail", "cs_asyncfail"
	seedWS(t, pool, ws)
	fb, fs := signed(testWebhookSecret, "evt_asyncfail", "checkout.session.async_payment_failed",
		sessionObj(sess, ws, 1000, "usd", "unpaid", "pi_af", micro(100)))
	if got := post(svc, fb, fs); got != http.StatusOK {
		t.Fatalf("async failed: got %d, want 200 noop", got)
	}
	if b := balance(t, pool, ws); b != 0 {
		t.Errorf("async failed must NOT credit; balance=%v", b)
	}
}

// ─── Amendment 2: credit failure ⇒ 5xx ⇒ retry credits exactly once ────

func TestWebhook_CreditFailure_5xx_ThenRetryOneCredit(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG billing tests")
	}
	_, pool, dt := newBillingService(t)
	const ws, sess, evt = "ws_creditfail", "cs_creditfail", "evt_creditfail"
	seedWS(t, pool, ws)

	flaky := &flakyCrediter{inner: dt}
	flaky.failNext.Store(true)
	svc := New(pool, flaky, &fakeStripe{}, testWebhookSecret)

	body, sig := signed(testWebhookSecret, evt, "checkout.session.completed",
		sessionObj(sess, ws, 1000, "usd", "paid", "pi_cf", micro(100)))

	if got := post(svc, body, sig); got < 500 { // first attempt: credit fails → 5xx
		t.Fatalf("credit failure: got %d, want 5xx (so Stripe retries)", got)
	}
	if c, _ := sessionRows(t, pool, sess); c != 0 {
		t.Fatalf("a failed credit must roll back the claim; rows=%d, want 0", c)
	}
	if got := post(svc, body, sig); got != http.StatusOK { // redeliver SAME event → credits
		t.Fatalf("retry: got %d, want 200", got)
	}
	if c, sum := sessionRows(t, pool, sess); c != 1 || sum != micro(100) {
		t.Errorf("rows=%d sumLXC=%v, want exactly one credit after retry", c, sum)
	}
	if b := balance(t, pool, ws); b != micro(100) {
		t.Errorf("balance=%v, want 100 (credited exactly once after retry)", b)
	}
}

// ─── Adversarial: concurrent same-event delivery ⇒ one credit ──────────

func TestWebhook_ConcurrentSameEvent_OneCredit(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, evt = "ws_concurrent", "cs_concurrent", "evt_concurrent"
	seedWS(t, pool, ws)
	body, sig := signed(testWebhookSecret, evt, "checkout.session.completed",
		sessionObj(sess, ws, 10000, "usd", "paid", "pi_cc", micro(1000)))

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i] = post(svc, body, sig)
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("delivery %d: got %d, want 200", i, c)
		}
	}
	if c, sum := sessionRows(t, pool, sess); c != 1 || sum != micro(1000) {
		t.Errorf("rows=%d sumLXC=%v, want exactly ONE credit of 1000 under concurrency", c, sum)
	}
	if b := balance(t, pool, ws); b != micro(1000) {
		t.Errorf("balance=%v, want 1000 (credited exactly once)", b)
	}
}

// ─── Checkout ──────────────────────────────────────────────────────────

func TestCheckout_OutOfBoundsAmount_Rejected(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	seedWS(t, pool, "ws_chk")
	for _, cents := range []int64{0, 999, 1_000_001} {
		if _, err := svc.CreateCheckout(context.Background(), "ws_chk", cents); !errors.Is(err, ErrAmountNotAllowed) {
			t.Errorf("%d cents: err=%v, want ErrAmountNotAllowed", cents, err)
		}
	}
}

// B5.1: any whole-cent amount in bounds, checkout through to the LEDGER ROW.
// $12.34 is one of the amounts the old float peg under-credited (123,399,999 µLXC);
// $2,500 is the top-up the item names.
func TestCheckout_AnyAmount_CreditsExactLedgerRow(t *testing.T) {
	_, pool, dt := newBillingService(t)
	fake := &fakeStripe{}
	svc := New(pool, dt, fake, testWebhookSecret)
	for _, tc := range []struct {
		ws    string
		cents int64
		ulxc  int64
	}{
		{"ws_any_2500", 250_000, 25_000 * 1_000_000}, // $2,500 → 25,000 credits
		{"ws_any_1234", 1_234, 123_400_000},          // $12.34 → 123.4 credits
	} {
		seedWS(t, pool, tc.ws)
		if _, err := svc.CreateCheckout(context.Background(), tc.ws, tc.cents); err != nil {
			t.Fatalf("%s: checkout: %v", tc.ws, err)
		}
		if fake.last.USDCents != tc.cents || fake.last.LXCAmount != tc.ulxc {
			t.Fatalf("%s: Stripe asked for %d¢ / %d µLXC, want %d¢ / %d µLXC",
				tc.ws, fake.last.USDCents, fake.last.LXCAmount, tc.cents, tc.ulxc)
		}
		sess := "cs_" + tc.ws
		body, sig := signed(testWebhookSecret, "evt_"+tc.ws, "checkout.session.completed",
			sessionObj(sess, tc.ws, tc.cents, "usd", "paid", "pi_"+tc.ws, fake.last.LXCAmount))
		if got := post(svc, body, sig); got != http.StatusOK {
			t.Fatalf("%s: webhook %d", tc.ws, got)
		}
		assertStatus(t, pool, sess, "completed")
		if c, sum := sessionRows(t, pool, sess); c != 1 || sum != tc.ulxc {
			t.Errorf("%s: purchase rows=%d lxc=%d, want 1 row of %d µLXC", tc.ws, c, sum, tc.ulxc)
		}
		if b := balance(t, pool, tc.ws); b != tc.ulxc {
			t.Errorf("%s: balance=%d µLXC, want %d", tc.ws, b, tc.ulxc)
		}
	}
}

func TestCheckout_Allowed_CreatesSession_ReusesCustomer(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG billing tests")
	}
	_, pool, dt := newBillingService(t)
	seedWS(t, pool, "ws_chk2")
	fake := &fakeStripe{}
	svc := New(pool, dt, fake, testWebhookSecret)

	url1, err := svc.CreateCheckout(context.Background(), "ws_chk2", 5000)
	if err != nil || url1 == "" {
		t.Fatalf("checkout 1: url=%q err=%v", url1, err)
	}
	if _, err := svc.CreateCheckout(context.Background(), "ws_chk2", 1000); err != nil {
		t.Fatalf("checkout 2: %v", err)
	}
	if fake.customers != 1 {
		t.Errorf("Stripe customer must be created once and reused; CreateCustomer calls=%d", fake.customers)
	}
	if fake.sessions != 2 {
		t.Errorf("two checkouts ⇒ two sessions; got %d", fake.sessions)
	}
}

// TestListPurchases_IncludesAnomalousNewestFirst — the admin list surfaces BOTH a
// credited and an anomalous row (charged-not-credited), newest first, so refunds
// are visible. The real SQL behind the admin route (route gate proven separately
// in cmd/lens).
func TestListPurchases_IncludesAnomalousNewestFirst(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws = "ws_list"
	seedWS(t, pool, ws)

	cb, cs := signed(testWebhookSecret, "evt_list_ok", "checkout.session.completed",
		sessionObj("cs_list_ok", ws, 1000, "usd", "paid", "pi_ok", micro(100)))
	if post(svc, cb, cs) != http.StatusOK {
		t.Fatal("seed credited row")
	}
	ab, as := signed(testWebhookSecret, "evt_list_anom", "checkout.session.completed",
		sessionObj("cs_list_anom", ws, 1000, "usd", "paid", "pi_anom", micro(9999))) // mismatch ⇒ anomalous
	if post(svc, ab, as) != http.StatusOK {
		t.Fatal("seed anomalous row")
	}

	rows, err := svc.ListPurchases(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListPurchases: %v", err)
	}
	var sawCompleted, sawAnomalous bool
	for _, r := range rows {
		switch r.Status {
		case "completed":
			sawCompleted = true
		case "anomalous":
			sawAnomalous = true
		}
	}
	if !sawCompleted || !sawAnomalous {
		t.Errorf("admin list must show both rows: completed=%v anomalous=%v", sawCompleted, sawAnomalous)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].CreatedAt.Before(rows[i].CreatedAt) {
			t.Errorf("rows must be newest-first; row %d older than row %d", i-1, i)
		}
	}
}

func assertStatus(t *testing.T, pool *pgxpool.Pool, sessID, want string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM lxc_purchases WHERE stripe_session_id=$1 ORDER BY created_at DESC LIMIT 1`,
		sessID).Scan(&status); err != nil {
		t.Fatalf("read status for %s: %v", sessID, err)
	}
	if status != want {
		t.Errorf("status=%q, want %q", status, want)
	}
}

// ─── U6 PR2: card-fingerprint capture (best-effort, money-safe) ────────────

// TestWebhook_FingerprintCaptureFailure_CreditStillLands — THE money-safety
// proof: the fingerprint capture errors, yet the LXC credit STILL lands, the
// webhook acks 200, and NO fingerprint row is written. The capture is
// structurally post-commit, so it can never drop the payment.
func TestWebhook_FingerprintCaptureFailure_CreditStillLands(t *testing.T) {
	_, pool, dt := newBillingService(t)
	fs := &fakeStripe{fingerprintErr: errors.New("stripe API down")}
	svc := New(pool, dt, fs, testWebhookSecret)
	ws := "ws_fp_fail"
	seedWS(t, pool, ws)

	body, sig := signed(testWebhookSecret, "evt_fp_fail", "checkout.session.completed",
		sessionObj("cs_fp_fail", ws, 1000, "usd", "paid", "pi_fp_fail", lxcForCents(1000)))
	if code := post(svc, body, sig); code != 200 {
		t.Fatalf("webhook must ack 200 despite capture failure, got %d", code)
	}
	if b := balance(t, pool, ws); b <= 0 {
		t.Fatalf("the LXC credit MUST land despite the fingerprint-capture failure, balance=%v", b)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workspace_card_fingerprints WHERE workspace_id=$1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a capture failure must write NO fingerprint row, got %d", n)
	}
}

// TestWebhook_FingerprintCaptureSuccess_StoresHash — on success, a HASH (never
// the raw fingerprint) is stored, and the credit lands.
func TestWebhook_FingerprintCaptureSuccess_StoresHash(t *testing.T) {
	_, pool, dt := newBillingService(t)
	fs := &fakeStripe{fingerprint: "card_fp_ABC123"}
	svc := New(pool, dt, fs, testWebhookSecret)
	ws := "ws_fp_ok"
	seedWS(t, pool, ws)

	body, sig := signed(testWebhookSecret, "evt_fp_ok", "checkout.session.completed",
		sessionObj("cs_fp_ok", ws, 1000, "usd", "paid", "pi_fp_ok", lxcForCents(1000)))
	if code := post(svc, body, sig); code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	if b := balance(t, pool, ws); b <= 0 {
		t.Fatalf("credit must land, balance=%v", b)
	}
	var hash string
	if err := pool.QueryRow(context.Background(),
		`SELECT fingerprint_hash FROM workspace_card_fingerprints WHERE workspace_id=$1`, ws).Scan(&hash); err != nil {
		t.Fatalf("a fingerprint row must be stored on success: %v", err)
	}
	sum := sha256.Sum256([]byte("card_fp_ABC123"))
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Errorf("stored hash = %q, want sha256 hex %q", hash, want)
	}
	if hash == "card_fp_ABC123" {
		t.Error("must store the HASH, never the raw fingerprint")
	}
}

// micro converts a whole-LXC test value to integer µLXC (SEC-2: 1 LXC = 1e6 µLXC).
func micro(lxc float64) int64 { return int64(lxc * 1e6) }
