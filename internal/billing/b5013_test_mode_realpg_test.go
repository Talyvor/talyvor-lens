package billing

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/economy"
)

// signedMode is a signed event Stripe confirmed in the given mode.
func signedMode(eventID, eventType string, livemode bool, object map[string]any) ([]byte, string) {
	body, _ := json.Marshal(map[string]any{"id": eventID, "type": eventType, "created": time.Now().Unix(), "livemode": livemode,
		"data": map[string]any{"object": object}})
	now := time.Now()
	return body, fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(webhook.ComputeSignature(now, body, testWebhookSecret)))
}

type invoicePayerCalls struct{ n int }

func (p *invoicePayerCalls) PayInvoiceByCard(context.Context, economy.InvoiceCardPayment) (economy.Invoice, error) {
	p.n++
	return economy.Invoice{}, nil
}

// B50.13 — money Stripe confirms in test mode never becomes credit in a real workspace. On the main key's webhook, a
// test-mode event naming a real workspace is acknowledged (200, so Stripe stops retrying) and moves nothing: no top-up
// credit, no plan, no credit line repaid, no invoice paid. The same top-up and repayment for a synthetic workspace land.
func TestB5013_ATestModeEventCreditsOnlyASyntheticWorkspace(t *testing.T) {
	svc, pool, _ := newSubService(t)
	payer := &invoicePayerCalls{}
	svc = svc.WithInvoices(payer)
	ctx := context.Background()
	const real, synthetic = "ws-b5013-real", "s-b5013-test"
	seedWS(t, pool, real)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, synthetic); err != nil {
		t.Fatal(err)
	}
	deliver := func(eventID, eventType string, object map[string]any) {
		t.Helper()
		body, sig := signedMode(eventID, eventType, false, object)
		if code := post(svc, body, sig); code != http.StatusOK {
			t.Fatalf("%s %s = %d; want 200", eventType, eventID, code)
		}
	}

	// A top-up paid with Stripe's test card.
	deliver("evt_b5013_topup_real", "checkout.session.completed", sessionObj("cs_b5013_real", real, 2500, "usd", "paid", "pi_b5013_real", micro(250)))
	if n, _ := sessionRows(t, pool, "cs_b5013_real"); n != 0 || balance(t, pool, real) != 0 {
		t.Fatalf("a test-mode top-up for a real workspace left %d purchase row(s) and a balance of %d µLXC; want none", n, balance(t, pool, real))
	}
	deliver("evt_b5013_topup_synth", "checkout.session.completed", sessionObj("cs_b5013_synth", synthetic, 2500, "usd", "paid", "pi_b5013_synth", micro(250)))
	if n, sum := sessionRows(t, pool, "cs_b5013_synth"); n != 1 || sum != micro(250) || balance(t, pool, synthetic) != micro(250) {
		t.Fatalf("a test-mode top-up for a synthetic workspace = %d row(s) of %d µLXC, balance %d; want one credit of 250 LXC", n, sum, balance(t, pool, synthetic))
	}

	// A plan bought in test mode.
	deliver("evt_b5013_sub_real", "customer.subscription.created",
		subObj("sub_b5013_real", real, "cus_b5013", "price_test_model2", "active", time.Now().Add(30*24*time.Hour), false))
	if _, ok := readSub(t, pool, "sub_b5013_real"); ok {
		t.Fatal("a test-mode subscription put a real workspace on a plan")
	}

	// A credit line's monthly invoice paid in test mode.
	for _, ws := range []string{real, synthetic} {
		if _, err := pool.Exec(ctx, `INSERT INTO credit_line_invoices (id, workspace_id, period_end, amount_ulxc, amount_cents, stripe_invoice_id, due_at)
			VALUES ('cli_' || $1, $1, now(), 1000000, 100, 'in_b5013_' || $1, now() + interval '14 days')`, ws); err != nil {
			t.Fatal(err)
		}
		deliver("evt_b5013_cl_"+ws, "invoice.paid", map[string]any{"id": "in_b5013_" + ws, "object": "invoice"})
	}
	var realPaid, synthPaid bool
	if err := pool.QueryRow(ctx, `SELECT bool_or(paid_at IS NOT NULL) FILTER (WHERE workspace_id = $1), bool_or(paid_at IS NOT NULL) FILTER (WHERE workspace_id = $2)
		FROM credit_line_invoices`, real, synthetic).Scan(&realPaid, &synthPaid); err != nil {
		t.Fatal(err)
	}
	if realPaid || !synthPaid {
		t.Fatalf("after test-mode payments the real company's credit line reads repaid=%v and the synthetic one's %v; want false and true", realPaid, synthPaid)
	}

	// A real workspace's invoice paid by a test-mode card.
	deliver("evt_b5013_inv_real", "checkout.session.completed", map[string]any{"id": "cs_b5013_inv", "object": "checkout.session", "mode": "payment",
		"payment_status": "paid", "amount_total": 12000, "currency": "gbp", "metadata": map[string]string{"talyvor_invoice_id": "inv_b5013_real"}})
	if payer.n != 0 {
		t.Fatalf("a test-mode card payment of a real workspace's invoice was posted %d time(s); want none", payer.n)
	}
}
