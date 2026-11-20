package economy

import (
	"errors"
	"testing"
)

// B50.55 — a payment request names its currency as Lens holds it: "gbp" or "Gbp" is refused, not read as GBP, as
// "XQZ" and "" already were (the nightly's money-tampered scenario). The control: "GBP" is taken.
func TestB5055PaymentRequestCurrencyMustBeExact(t *testing.T) {
	item := func(c string) PaymentRequestIn {
		return PaymentRequestIn{Client: "chat", Items: []PaymentRequestItemIn{{PayeeID: "payee_1", Currency: c, AmountMinor: 100, Reference: "TAMPER"}}}
	}
	for _, c := range []string{"gbp", "Gbp", "usdc", "XQZ", ""} {
		in := item(c)
		if _, err := in.normalise(); !errors.Is(err, ErrPaymentRequest) {
			t.Errorf("currency %q: got %v, want a refusal (ErrPaymentRequest)", c, err)
		}
	}
	in := item("GBP")
	if kind, err := in.normalise(); err != nil || kind != "payment" || in.Items[0].Currency != CurrencyGBP {
		t.Fatalf("the control, currency GBP: kind %q, currency %q, err %v; want a payment in GBP", kind, in.Items[0].Currency, err)
	}
}
