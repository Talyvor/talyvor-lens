package billing

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v81/webhook"
)

// B7.1 — a checkout.session.completed is routed by the session's MODE. Both
// fixtures are full Stripe test-mode event bodies (testdata/), signed with the
// SDK's own signer and posted through the real HandleWebhook.

func postFixture(t *testing.T, svc *Service, name string) int {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	now := time.Now()
	sig := fmt.Sprintf("t=%d,v1=%x", now.Unix(), webhook.ComputeSignature(now, body, testWebhookSecret))
	return post(svc, body, sig)
}

// A subscription checkout is paid for by the subscription, not by LXC: it must
// leave NO lxc_purchases row, so nothing lands on the admin list as "charged and
// NOT credited, refund manually".
func TestWebhook_SubscriptionCheckout_WritesNoPurchaseRow(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	seedWS(t, pool, "ws-b71-sub")

	if code := postFixture(t, svc, "checkout_session_completed_subscription.json"); code != http.StatusOK {
		t.Fatalf("webhook code = %d, want 200", code)
	}

	if n, _ := sessionRows(t, pool, "cs_test_b71_subscription"); n != 0 {
		t.Errorf("lxc_purchases rows for the subscription session = %d, want 0", n)
	}
	rows, err := svc.ListPurchases(context.Background(), 500)
	if err != nil {
		t.Fatalf("ListPurchases: %v", err)
	}
	for _, r := range rows {
		if r.Status == "anomalous" {
			t.Errorf("admin purchases list has an anomalous row after a subscription checkout: %+v", r)
		}
	}
	if b := balance(t, pool, "ws-b71-sub"); b != 0 {
		t.Errorf("balance = %d µLXC, want 0 — a subscription checkout is not a top-up", b)
	}
}

// The top-up path is unchanged: a payment-mode checkout credits exactly its amount.
func TestWebhook_PaymentCheckout_StillCreditsExactAmount(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	seedWS(t, pool, "ws-b71-topup")
	// The fixture is a Stripe test-mode event, which credits a test (synthetic) workspace only (B50.13).
	if _, err := pool.Exec(context.Background(), `UPDATE workspaces SET synthetic = true WHERE id = 'ws-b71-topup'`); err != nil {
		t.Fatal(err)
	}

	if code := postFixture(t, svc, "checkout_session_completed_payment.json"); code != http.StatusOK {
		t.Fatalf("webhook code = %d, want 200", code)
	}

	n, sum := sessionRows(t, pool, "cs_test_b71_topup")
	if n != 1 || sum != micro(500) {
		t.Errorf("lxc_purchases for the top-up = %d rows, %d µLXC; want 1 row, %d µLXC", n, sum, micro(500))
	}
	assertStatus(t, pool, "cs_test_b71_topup", "completed")
	if b := balance(t, pool, "ws-b71-topup"); b != micro(500) {
		t.Errorf("balance = %d µLXC, want %d ($50 at $0.10/LXC)", b, micro(500))
	}
}
