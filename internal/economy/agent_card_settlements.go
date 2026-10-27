package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/ecbrate"
)

// agent_card_settlements.go — B19.25: A CARD PURCHASE'S CAPTURE, REVERSAL OR REFUND SETTLES THE AGENT'S BALANCE.
//
// An approved authorisation debits the agent the moment it is approved (agent_cards.go): that debit is the
// authorisation's HOLD. What becomes of it reaches Lens afterwards on the regular Stripe webhook, and each
// event that settles anything is an agent_card_settlements row (migration 0157), append-only, in ONE
// transaction with the lxc_ledger row (type agent_card) and agent_postings entry (kind 'card') it moves:
//
//   - a capture consumes the hold: at the hold's amount it moves nothing, above it the agent is debited the
//     difference;
//   - the authorisation closing, being reversed or expiring releases what is still held — so a capture below
//     the hold is credited the difference then — and a partial reversal while pending releases what it reversed;
//   - a refund credits the agent.
//
// Captures and closes arrive in either order; both orders end on the same balance. Amounts convert at the ECB
// rate recorded on the authorisation's row, and a hold released in full is credited exactly what it debited.
// Test mode only, like the cards: a live-mode event moves nothing.

// The kinds of settlement.
const (
	CardCapture = "capture"
	CardRelease = "release"
	CardRefund  = "refund"
)

// CardSettlement is one Stripe event about a card purchase after its authorisation was decided.
type CardSettlement struct {
	EventID         string // evt_…: the row's key
	Kind            string // CardCapture, CardRefund, or CardRelease for an issuing_authorization.updated
	AuthorizationID string // iauth_…; '' for a refund Stripe did not link to one
	TransactionID   string // ipi_…, for a capture or a refund
	CardID          string
	Status          string // for a release: the authorisation's status (pending, closed, reversed, expired)
	AmountMinor     int64  // a capture's or refund's amount, or what a pending authorisation still holds; card currency
	Currency        string
	Livemode        bool
	At              time.Time
}

// CardSettled is what a settlement moved.
type CardSettled struct {
	Recorded   bool   `json:"recorded"` // false when there was nothing to settle
	AgentID    string `json:"agent_id,omitempty"`
	AmountULXC int64  `json:"amount_ulxc"` // + credited the agent, − debited it
	Reason     string `json:"reason"`
}

// cardRate is the ECB rate an authorisation was priced at.
type cardRate struct {
	date     *time.Time
	usd, ccy *string
	currency string
	merchant string
}

func (r cardRate) ulxc(amountMinor int64) (int64, error) {
	if amountMinor == 0 {
		return 0, nil
	}
	if strings.EqualFold(r.currency, "usd") {
		return amountMinor * 10_000 * ULXCPerUSDMicro, nil
	}
	if r.usd == nil || r.ccy == nil {
		return 0, fmt.Errorf("economy: no ECB rate is recorded to convert %d %s at", amountMinor, r.currency)
	}
	return ecbrate.USDMicros(amountMinor, *r.usd, *r.ccy) * ULXCPerUSDMicro, nil
}

// SettleAgentCard settles one event against the authorisation's hold and records it. An event already
// settled (Stripe sent it again) returns what it moved and moves nothing more.
func (s *DualTokenStore) SettleAgentCard(ctx context.Context, st CardSettlement) (CardSettled, error) {
	if done, ok, err := recordedCardSettlement(ctx, s.pool, st.EventID); err != nil || ok {
		return done, err
	}
	if st.Livemode {
		return CardSettled{Reason: "agent cards take no real money yet (class RED): a live-mode event moves nothing"}, nil
	}
	var ws, agentID string
	err := s.pool.QueryRow(ctx, `SELECT workspace_id, agent_id FROM agent_cards WHERE id = $1`, st.CardID).Scan(&ws, &agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CardSettled{Reason: "the card is not one Talyvor issued to an agent"}, nil
	}
	if err != nil {
		return CardSettled{}, fmt.Errorf("economy: card's agent: %w", err)
	}
	out := CardSettled{AgentID: agentID}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, ws, agentID); err != nil { // every settlement of this agent's holds serialises here
		return out, err
	}
	if done, ok, err := recordedCardSettlement(ctx, tx, st.EventID); err != nil || ok { // a concurrent delivery settled it first
		return done, err
	}
	heldMinor, heldULXC, rate, err := cardHold(ctx, tx, agentID, st.AuthorizationID, st.CardID)
	if err != nil {
		return out, err
	}
	if rate.currency == "" {
		rate.currency = st.Currency
	}

	var holdMinor, holdULXC int64 // how much of the hold this settles
	var why string
	switch st.Kind {
	case CardCapture:
		captured := abs64(st.AmountMinor)
		holdMinor = min(captured, heldMinor)
		if holdMinor == heldMinor {
			holdULXC = heldULXC
		} else if holdULXC, err = rate.ulxc(holdMinor); err != nil {
			return out, err
		}
		holdULXC = min(holdULXC, heldULXC)
		above, err := rate.ulxc(captured - holdMinor)
		if err != nil {
			return out, err
		}
		out.AmountULXC = -above
		why = "captured at the amount held"
		switch {
		case heldMinor == 0:
			why = "captured with nothing held"
		case captured > heldMinor:
			why = "captured above the amount held"
		case captured < heldMinor:
			why = "captured below the amount held; the rest is released when the authorisation closes"
		}
	case CardRelease:
		stillHeld := int64(0)
		if st.Status == "pending" {
			stillHeld = min(max(st.AmountMinor, 0), heldMinor)
		}
		holdMinor = heldMinor - stillHeld
		if stillHeld == 0 {
			holdULXC = heldULXC
		} else if holdULXC, err = rate.ulxc(holdMinor); err != nil {
			return out, err
		}
		holdULXC = min(holdULXC, heldULXC)
		out.AmountULXC = holdULXC
		why = "the authorisation is " + st.Status + ": its hold is released"
		if st.Status == "pending" {
			why = "part of the authorisation was reversed: that much of its hold is released"
		}
	case CardRefund:
		credit, err := rate.ulxc(abs64(st.AmountMinor))
		if err != nil {
			return out, err
		}
		out.AmountULXC = credit
		why = "refunded by the merchant"
	default:
		return out, fmt.Errorf("economy: unknown card settlement %q", st.Kind)
	}
	if holdMinor == 0 && holdULXC == 0 && out.AmountULXC == 0 {
		out.Reason = "nothing to settle: the authorisation holds nothing more"
		return out, nil
	}

	var testMoved int64 // B22.1: a debit takes test-funded credits before the balance write, a credit gives them after it
	if out.AmountULXC < 0 {
		if testMoved, err = cardTestFunded(ctx, tx, ws, agentID, st.AuthorizationID, out.AmountULXC); err != nil {
			return out, err
		}
	}
	if out.AmountULXC != 0 {
		wsBal, minted, wsSpent, err := readLXCBalance(ctx, tx, ws)
		if err != nil {
			return out, err
		}
		desc := "agent card " + st.Kind
		if rate.merchant != "" {
			desc += ": " + rate.merchant
		}
		if err := insertLXCLedger(ctx, tx, ws, out.AmountULXC, wsBal+out.AmountULXC, LXCTypeAgentCard, desc,
			map[string]interface{}{"agent_id": agentID, "authorization_id": st.AuthorizationID, "transaction_id": st.TransactionID,
				"settlement": st.Kind, "event_id": st.EventID}); err != nil {
			return out, err
		}
		if err := writeLXCBalance(ctx, tx, ws, wsBal+out.AmountULXC, minted, wsSpent-out.AmountULXC); err != nil {
			return out, err
		}
		ref := st.AuthorizationID
		if ref == "" {
			ref = st.TransactionID
		}
		if err := postEntry(ctx, tx, ws, "card", ref, leg{agentAccount(agentID), out.AmountULXC}, leg{"spend", -out.AmountULXC}); err != nil {
			return out, err
		}
	}
	if out.AmountULXC > 0 {
		if testMoved, err = cardTestFunded(ctx, tx, ws, agentID, st.AuthorizationID, out.AmountULXC); err != nil {
			return out, err
		}
	}
	out.Recorded, out.Reason = true, why
	if _, err := tx.Exec(ctx, `INSERT INTO agent_card_settlements (id, kind, authorization_id, transaction_id, card_id, workspace_id,
		agent_id, status, amount_minor, currency, hold_minor, hold_ulxc, amount_ulxc, rate_date, ecb_usd_per_eur, ecb_currency_per_eur, created_at,
		test_funded_ulxc)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::numeric, $16::numeric, $17, $18)`,
		st.EventID, st.Kind, st.AuthorizationID, st.TransactionID, st.CardID, ws, agentID, st.Status, st.AmountMinor,
		strings.ToLower(st.Currency), holdMinor, holdULXC, out.AmountULXC, rate.date, rate.usd, rate.ccy, st.At, testMoved); err != nil {
		return out, fmt.Errorf("economy: record card settlement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if done, ok, rerr := recordedCardSettlement(ctx, s.pool, st.EventID); rerr == nil && ok {
			return done, nil // a concurrent delivery of the same event settled it first
		}
		return out, err
	}
	return out, nil
}

// cardHold reads what an authorisation still holds — Σ its approved requests less what earlier settlements
// took off the hold — and the rate it was priced at. With no approved request (a refund Stripe did not link,
// a capture with no authorisation Lens approved), nothing is held and the rate is the card's latest.
func cardHold(ctx context.Context, tx pgx.Tx, agentID, authorizationID, cardID string) (heldMinor, heldULXC int64, r cardRate, err error) {
	if authorizationID != "" {
		if err = tx.QueryRow(ctx, `SELECT
			(SELECT COALESCE(sum(amount_minor), 0) FROM agent_card_authorizations WHERE authorization_id = $1 AND agent_id = $2 AND approved)
			- (SELECT COALESCE(sum(hold_minor), 0) FROM agent_card_settlements WHERE authorization_id = $1 AND agent_id = $2),
			(SELECT COALESCE(sum(amount_ulxc), 0) FROM agent_card_authorizations WHERE authorization_id = $1 AND agent_id = $2 AND approved)
			- (SELECT COALESCE(sum(hold_ulxc), 0) FROM agent_card_settlements WHERE authorization_id = $1 AND agent_id = $2)`,
			authorizationID, agentID).Scan(&heldMinor, &heldULXC); err != nil {
			return 0, 0, r, fmt.Errorf("economy: card hold: %w", err)
		}
		err = tx.QueryRow(ctx, `SELECT rate_date, ecb_usd_per_eur::text, ecb_currency_per_eur::text, currency, merchant_name
			FROM agent_card_authorizations WHERE authorization_id = $1 AND agent_id = $2 AND amount_usd_micros IS NOT NULL
			ORDER BY approved DESC, created_at, id LIMIT 1`, authorizationID, agentID).Scan(&r.date, &r.usd, &r.ccy, &r.currency, &r.merchant)
		if err == nil {
			return max(heldMinor, 0), max(heldULXC, 0), r, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, r, fmt.Errorf("economy: card rate: %w", err)
		}
	}
	err = tx.QueryRow(ctx, `SELECT rate_date, ecb_usd_per_eur::text, ecb_currency_per_eur::text, currency, merchant_name
		FROM agent_card_authorizations WHERE card_id = $1 AND agent_id = $2 AND amount_usd_micros IS NOT NULL
		ORDER BY created_at DESC, id DESC LIMIT 1`, cardID, agentID).Scan(&r.date, &r.usd, &r.ccy, &r.currency, &r.merchant)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return max(heldMinor, 0), max(heldULXC, 0), r, nil
	case err != nil:
		return 0, 0, r, fmt.Errorf("economy: card rate: %w", err)
	}
	return max(heldMinor, 0), max(heldULXC, 0), r, nil
}

// cardTestFunded moves the test-funded credits one settlement of a card purchase gives back or takes
// (B22.1), and returns them (+ given back, − taken). Only a purchase that took test-funded credits (an
// uncleared RED spend) moves any: what comes back to it is test-funded again, up to what it has taken so far,
// and a capture above its hold takes test-funded credits first.
func cardTestFunded(ctx context.Context, tx pgx.Tx, ws, agentID, authorizationID string, amountULXC int64) (int64, error) {
	if authorizationID == "" || amountULXC == 0 {
		return 0, nil
	}
	var took, net int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COALESCE(sum(test_funded_ulxc), 0) FROM agent_card_authorizations WHERE authorization_id = $1 AND agent_id = $2 AND approved),
		(SELECT COALESCE(sum(test_funded_ulxc), 0) FROM agent_card_settlements WHERE authorization_id = $1 AND agent_id = $2)`,
		authorizationID, agentID).Scan(&took, &net); err != nil {
		return 0, fmt.Errorf("economy: card's test-funded credits: %w", err)
	}
	if took == 0 {
		return 0, nil
	}
	if amountULXC > 0 {
		back := min(amountULXC, took-net)
		if back <= 0 {
			return 0, nil
		}
		return back, addTestFunded(ctx, tx, ws, back)
	}
	have, err := testFundedULXC(ctx, tx, ws)
	if err != nil {
		return 0, err
	}
	take := min(-amountULXC, have)
	if _, err := tx.Exec(ctx, `UPDATE lxc_balances SET test_funded_ulxc = $2 WHERE workspace_id = $1`, ws, have-take); err != nil {
		return 0, fmt.Errorf("economy: take test-funded credits: %w", err)
	}
	return -take, nil
}

func recordedCardSettlement(ctx context.Context, q pgxDB, eventID string) (CardSettled, bool, error) {
	var out CardSettled
	var kind string
	err := q.QueryRow(ctx, `SELECT agent_id, amount_ulxc, kind FROM agent_card_settlements WHERE id = $1`, eventID).
		Scan(&out.AgentID, &out.AmountULXC, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, fmt.Errorf("economy: card settlement on record: %w", err)
	}
	out.Recorded, out.Reason = true, "already settled ("+kind+")"
	return out, true, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
