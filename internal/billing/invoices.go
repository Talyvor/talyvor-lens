package billing

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strings"

	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/checkout/session"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/screening"
)

// invoices.go — B30.20: an agent's invoice paid by card, through a Stripe test-mode Checkout Session.
//
// The pay link's card button opens a Checkout Session for what is due, stamped with the invoice's id; the session's
// checkout.session.completed (or async_payment_succeeded) posts the payment into the issuing agent's account through
// economy.PayInvoiceByCard, once per session.

// InvoicePayer posts a card payment onto an invoice. *economy.DualTokenStore satisfies it.
type InvoicePayer interface {
	PayInvoiceByCard(ctx context.Context, c economy.InvoiceCardPayment) (economy.Invoice, error)
}

// WithInvoices posts invoices' card payments from the webhook. Unset, they are acked and logged.
func (s *Service) WithInvoices(p InvoicePayer) *Service {
	s.invoices = p
	return s
}

// InvoiceCheckout is a card payment of an invoice asked for: AmountMinor of Currency, back to the pay page after.
type InvoiceCheckout struct {
	InvoiceID   string
	Number      string
	Issuer      string
	Currency    string
	AmountMinor int64
	ReturnURL   string
}

// CreateInvoiceCheckoutSession opens a one-off Checkout Session for an invoice and answers its URL.
func (l *LiveStripe) CreateInvoiceCheckoutSession(ctx context.Context, p InvoiceCheckout) (string, error) {
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String(p.ReturnURL + "?paid=card"),
		CancelURL:  stripe.String(p.ReturnURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:    stripe.String(strings.ToLower(p.Currency)),
				UnitAmount:  stripe.Int64(p.AmountMinor),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{Name: stripe.String("Invoice " + p.Number + " from " + p.Issuer)},
			},
			Quantity: stripe.Int64(1),
		}},
	}
	params.Context = ctx
	params.AddMetadata("talyvor_invoice_id", p.InvoiceID)
	sess, err := session.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

func (s *Service) handleInvoiceCard(w http.ResponseWriter, ctx context.Context, event *stripe.Event, sess *stripe.CheckoutSession) {
	invoiceID := sess.Metadata["talyvor_invoice_id"]
	if s.invoices == nil {
		s.log.Warn("billing webhook: an invoice's card payment, and invoices are not configured", "event", event.ID, "invoice", invoiceID)
		w.WriteHeader(http.StatusOK)
		return
	}
	// As for a top-up: a delayed method completes unpaid first, and its async_payment_succeeded pays.
	if event.Type == "checkout.session.completed" && sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
		w.WriteHeader(http.StatusOK)
		return
	}
	c := economy.InvoiceCardPayment{InvoiceID: invoiceID, SessionID: sess.ID, AmountMinor: sess.AmountTotal, Currency: string(sess.Currency),
		Funding: economy.FundingTest}
	if event.Livemode || sess.Livemode {
		c.Funding = economy.FundingLive
	}
	if d := sess.CustomerDetails; d != nil {
		c.Payer = cmp.Or(d.Name, d.Email)
	}
	inv, err := s.invoices.PayInvoiceByCard(ctx, c)
	var refusal *screening.Refusal
	var capRefusal *economy.CapabilityRefusal
	switch {
	case errors.Is(err, economy.ErrInvoiceNotFound), errors.Is(err, economy.ErrInvoice), errors.As(err, &refusal), errors.As(err, &capRefusal):
		// It will never post, so Stripe is not asked again: an operator sees it here, or on the compliance case.
		s.log.Error("billing webhook: an invoice's card payment did not post", "event", event.ID, "invoice", invoiceID, "session", sess.ID, "err", err)
		w.WriteHeader(http.StatusOK)
	case err != nil:
		s.fail(w, "invoice card payment", event.ID, err)
	default:
		s.log.Info("billing webhook: an invoice was paid by card", "event", event.ID, "invoice", inv.ID, "status", inv.Status)
		w.WriteHeader(http.StatusOK)
	}
}
