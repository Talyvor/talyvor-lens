package billing

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
)

// B30.92 — a charged-back top-up takes its credits back. Asserted on the lxc_ledger row, the balance, the
// cash backing and the earn predicate, never the status code.

// chargebackRows reads the workspace's chargeback ledger rows: how many, and what they moved.
func chargebackRows(t *testing.T, pool *pgxpool.Pool, ws string) (n int, sum int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(sum(amount), 0)::bigint FROM lxc_ledger
		WHERE workspace_id = $1 AND type = $2`, ws, economy.LXCTypeTopUpChargeback).Scan(&n, &sum); err != nil {
		t.Fatalf("read chargeback rows: %v", err)
	}
	return n, sum
}

func cashBacked(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var v int64
	if err := pool.QueryRow(context.Background(), `SELECT cash_backed_ulxc FROM lxc_balances WHERE workspace_id = $1`, ws).Scan(&v); err != nil {
		t.Fatalf("read cash backing: %v", err)
	}
	return v
}

func topUp(t *testing.T, svc *Service, ws, sess, pi string, usdCents, lxc int64) {
	t.Helper()
	body, sig := signedLive(testWebhookSecret, "evt_pay_"+sess, "checkout.session.completed",
		sessionObj(sess, ws, usdCents, "usd", "paid", pi, lxc))
	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("top-up: got %d", got)
	}
}

func dispute(t *testing.T, svc *Service, eventID, pi string, cents int64) {
	t.Helper()
	body, sig := signedLive(testWebhookSecret, eventID, "charge.dispute.created",
		map[string]any{"id": "dp_" + pi, "amount": cents, "currency": "usd", "charge": "ch_" + pi, "payment_intent": pi})
	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("dispute: got %d", got)
	}
}

func TestWebhook_DisputedTopUp_TakesItsCreditsBackAndItsRightToEarn(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, pi = "ws_cb_full", "cs_cb_full", "pi_cb_full"
	seedWS(t, pool, ws)
	topUp(t, svc, ws, sess, pi, 5000, micro(500))
	if !mayEarn(t, pool, ws, true) {
		t.Fatal("precondition: a completed live top-up confers the right to earn")
	}

	dispute(t, svc, "evt_cb_full", pi, 5000)

	if n, sum := chargebackRows(t, pool, ws); n != 1 || sum != -micro(500) {
		t.Errorf("chargeback ledger rows = %d moving %d µLXC; want 1 moving %d", n, sum, -micro(500))
	}
	if b := balance(t, pool, ws); b != 0 {
		t.Errorf("balance after the chargeback = %d µLXC; want 0", b)
	}
	if c := cashBacked(t, pool, ws); c != 0 {
		t.Errorf("cash backing after the chargeback = %d µLXC; want 0 (it would fund royalties)", c)
	}
	if mayEarn(t, pool, ws, false) || mayEarn(t, pool, ws, true) {
		t.Error("a charged-back top-up still confers the right to earn")
	}
	assertStatus(t, pool, sess, "charged_back")

	// Stripe redelivers: the same dispute takes nothing a second time.
	dispute(t, svc, "evt_cb_full", pi, 5000)
	dispute(t, svc, "evt_cb_full_again", pi, 5000)
	if n, _ := chargebackRows(t, pool, ws); n != 1 {
		t.Errorf("a redelivered dispute wrote %d chargeback rows; want 1", n)
	}
}

func TestWebhook_DisputedTopUp_TakesTheUnspentPartWhenSomeWasSpent(t *testing.T) {
	svc, pool, dt := newBillingService(t)
	const ws, sess, pi = "ws_cb_spent", "cs_cb_spent", "pi_cb_spent"
	seedWS(t, pool, ws)
	topUp(t, svc, ws, sess, pi, 5000, micro(500))
	if err := dt.SpendLXC(context.Background(), ws, micro(200), "model call"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	unspent := balance(t, pool, ws)

	dispute(t, svc, "evt_cb_spent", pi, 5000)

	if n, sum := chargebackRows(t, pool, ws); n != 1 || sum != -unspent {
		t.Errorf("chargeback ledger rows = %d moving %d µLXC; want 1 moving %d (all that was unspent)", n, sum, -unspent)
	}
	if b := balance(t, pool, ws); b != 0 {
		t.Errorf("balance after the chargeback = %d µLXC; want 0", b)
	}
	var unrecovered int64
	if err := pool.QueryRow(context.Background(), `SELECT (metadata->>'unrecovered_ulxc')::bigint FROM lxc_ledger
		WHERE workspace_id = $1 AND type = $2`, ws, economy.LXCTypeTopUpChargeback).Scan(&unrecovered); err != nil {
		t.Fatalf("read unrecovered: %v", err)
	}
	if unrecovered != micro(500)-unspent {
		t.Errorf("unrecovered = %d µLXC; want %d (what was already spent)", unrecovered, micro(500)-unspent)
	}
}
