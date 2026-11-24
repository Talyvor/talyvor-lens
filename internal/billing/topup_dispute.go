package billing

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/economy"
)

// reverseTopUpDispute takes back the credits of a top-up whose payment was charged back (B30.92): the
// unspent part of what it credited comes off the balance, its cash backing goes, and its purchase is marked
// charged_back, so it no longer confers the right to earn (internal/earnverify reads only completed rows).
// Once per purchase: a redelivered dispute finds it already charged back. It answers false when it has
// already answered the webhook (an error Stripe should retry).
func (s *Service) reverseTopUpDispute(w http.ResponseWriter, ctx context.Context, event *stripe.Event) bool {
	var d struct {
		ID            string          `json:"id"`
		Amount        int64           `json:"amount"`
		PaymentIntent json.RawMessage `json:"payment_intent"`
	}
	if err := json.Unmarshal(event.Data.Raw, &d); err != nil || d.ID == "" {
		return true // handleDispute acks what it cannot parse
	}
	pi := idOf(d.PaymentIntent)
	if pi == "" {
		return true
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.fail(w, "chargeback begin", event.ID, err)
		return false
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `UPDATE lxc_purchases SET status = 'charged_back'
		WHERE stripe_payment_intent = $1 AND status = 'completed' AND lxc_amount > 0
		RETURNING workspace_id, lxc_amount, usd_cents, refunded_cents, livemode IS FALSE`, pi)
	if err != nil {
		s.fail(w, "chargeback mark", event.ID, err)
		return false
	}
	type purchase struct {
		ws                       string
		lxc, usdCents, refundedC int64
		test                     bool
	}
	var charged []purchase
	for rows.Next() {
		var p purchase
		if err := rows.Scan(&p.ws, &p.lxc, &p.usdCents, &p.refundedC, &p.test); err != nil {
			rows.Close()
			s.fail(w, "chargeback scan", event.ID, err)
			return false
		}
		charged = append(charged, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.fail(w, "chargeback rows", event.ID, err)
		return false
	}
	for _, p := range charged {
		// A partial refund already removed its share of the backing (handleRefund); the rest goes now.
		backing := p.lxc
		if p.usdCents > 0 && p.refundedC > 0 {
			backing -= p.lxc * min(p.refundedC, p.usdCents) / p.usdCents
		}
		taken, err := economy.ReverseTopUpTx(ctx, tx, p.ws, p.lxc, backing, p.test, "top-up charged back", map[string]interface{}{
			"stripe_dispute_id": d.ID, "stripe_payment_intent": pi, "stripe_event_id": event.ID, "dispute_amount_cents": d.Amount,
		})
		if err != nil {
			s.fail(w, "chargeback reversal", event.ID, err)
			return false
		}
		s.log.Warn("billing webhook: a top-up was charged back", "workspace", p.ws, "dispute", d.ID,
			"credits_taken_ulxc", taken, "credits_already_spent_ulxc", p.lxc-taken)
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, "chargeback commit", event.ID, err)
		return false
	}
	return true
}
