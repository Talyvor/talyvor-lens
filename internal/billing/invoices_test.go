package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	stripe "github.com/stripe/stripe-go/v81"
)

// B17.438 — an invoice's card payment posts only once Stripe's processing fee is known (B56.21), read from the
// charge's balance transaction when checkout.session.completed arrives. Checkout's default capture is asynchronous,
// so that balance transaction is not there yet: the fee read failed, the webhook was asked again hours later, and
// the pay page still read "Pay an invoice" after the card was paid. The session is opened with synchronous capture,
// so the charge is settled, fee and all, when the session completes.
func TestCreateInvoiceCheckoutSession_CapturesSynchronously(t *testing.T) {
	var sent url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkout/sessions" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		sent = r.PostForm
		_, _ = w.Write([]byte(`{"id":"cs_test_fixture","object":"checkout.session","url":"https://checkout.stripe.test/cs_test_fixture"}`))
	}))
	defer srv.Close()
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })

	got, err := NewTestModeStripe("sk_test_fixture", "", "").CreateInvoiceCheckoutSession(context.Background(), InvoiceCheckout{
		InvoiceID: "inv_fixture", Number: "INV-000001", Issuer: "Fixture Ltd", Currency: "GBP", AmountMinor: 5000,
		ReturnURL: "https://app.test/pay/tok"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://checkout.stripe.test/cs_test_fixture" {
		t.Fatalf("checkout url = %q", got)
	}
	for k, v := range map[string]string{
		"payment_intent_data[capture_method]":    "automatic",
		"metadata[talyvor_invoice_id]":           "inv_fixture",
		"line_items[0][price_data][unit_amount]": "5000",
		"line_items[0][price_data][currency]":    "gbp",
		"success_url":                            "https://app.test/pay/tok?paid=card",
	} {
		if sent.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, sent.Get(k), v)
		}
	}
}
