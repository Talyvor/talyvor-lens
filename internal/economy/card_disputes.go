package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
)

// card_disputes.go — B59.24: CARD DISPUTES AND REFUNDS, AND THE CARD EXPORT.
//
// A card payment is a charge Lens approved on one of its cards (money_card_authorizations, approved). The owner
// disputes one with a reason (OpenCardDispute): the dispute is kept, opened, and handed to the card's issuer through its
// DisputeProvider, and is with the issuer once it took it. The issuer decides it (ReceiveCardDisputeOutcome): won, the
// payment comes back to the card's account; lost, nothing moves. A merchant's refund reaches Lens the same way
// (ReceiveCardRefund) and comes back to the card's account too. Each credit is its own money entry, from the partner
// account the payment went to, once under its own key, and a payment never gets back more than it paid: what is left of
// it is read with the payment's row locked. Amounts are the payment's own minor units, so nothing is converted or rounded.
//
// CardExport is the workspace's card payments, disputes and refunds over a period, for the owner's CSV.

// The states of a card dispute.
const (
	CardDisputeOpened     = "opened"      // kept; the issuer has not taken it yet
	CardDisputeWithIssuer = "with_issuer" // the issuer took it and is deciding
	CardDisputeWon        = "won"         // the payment came back
	CardDisputeLost       = "lost"        // nothing came back

	MaxCardDisputeReason = 500 // characters
	MaxCardExportDays    = 366
)

var (
	// ErrCardDispute: a dispute or refund Lens cannot take — no such card payment, no reason, or a refund in another currency.
	ErrCardDispute = errors.New("economy: invalid card dispute")
	// ErrCardDisputeNotFound: no such dispute in this workspace, or none the issuer took under that reference.
	ErrCardDisputeNotFound = errors.New("economy: no such card dispute")
	// ErrCardDisputeDecided: the dispute was already decided the other way.
	ErrCardDisputeDecided = errors.New("economy: the card dispute is already decided")
)

// CardDispute is the owner's dispute of one card payment, and where it stands.
type CardDispute struct {
	ID            string     `json:"id"`
	Partner       string     `json:"partner"`
	PaymentID     string     `json:"payment_id"`
	WorkspaceID   string     `json:"workspace_id"`
	AgentID       string     `json:"agent_id,omitempty"`
	CardID        string     `json:"card_id"`
	Merchant      string     `json:"merchant"`
	AmountMinor   int64      `json:"amount_minor"`
	Currency      string     `json:"currency"`
	Reason        string     `json:"reason"`
	Status        string     `json:"status"`
	CreditedMinor int64      `json:"credited_minor"`
	OpenedBy      string     `json:"opened_by,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	partnerRef    string
}

const cardDisputeCols = `id, partner, payment_id, workspace_id, agent_id, card_id, merchant, amount_minor, currency, reason, status,
	credited_minor, opened_by, decided_at, created_at, partner_ref`

func (d *CardDispute) fields() []any {
	return []any{&d.ID, &d.Partner, &d.PaymentID, &d.WorkspaceID, &d.AgentID, &d.CardID, &d.Merchant, &d.AmountMinor, &d.Currency,
		&d.Reason, &d.Status, &d.CreditedMinor, &d.OpenedBy, &d.DecidedAt, &d.CreatedAt, &d.partnerRef}
}

// CardPaymentRefund is a merchant's refund of one card payment, and what of it was credited.
type CardPaymentRefund struct {
	ID            string    `json:"id"`
	Partner       string    `json:"partner"`
	PaymentID     string    `json:"payment_id"`
	WorkspaceID   string    `json:"workspace_id"`
	AgentID       string    `json:"agent_id,omitempty"`
	CardID        string    `json:"card_id"`
	Merchant      string    `json:"merchant"`
	AmountMinor   int64     `json:"amount_minor"` // what the merchant refunded
	Currency      string    `json:"currency"`
	CreditedMinor int64     `json:"credited_minor"` // what came back: no more than was left of the payment
	CreatedAt     time.Time `json:"created_at"`
}

const cardRefundCols = `id, partner, payment_id, workspace_id, agent_id, card_id, merchant, amount_minor, currency, credited_minor, created_at`

func (r *CardPaymentRefund) fields() []any {
	return []any{&r.ID, &r.Partner, &r.PaymentID, &r.WorkspaceID, &r.AgentID, &r.CardID, &r.Merchant, &r.AmountMinor, &r.Currency,
		&r.CreditedMinor, &r.CreatedAt}
}

// cardPayment is an approved card payment, locked for what comes back of it.
type cardPayment struct {
	partner, id, workspaceID, agentID, cardID, merchant, currency string
	amountMinor                                                   int64
	accountID                                                     string // the card's account, which paid
}

// lockCardPayment reads and locks partner's approved payment id — in workspaceID, or any when it is "".
func lockCardPayment(ctx context.Context, tx pgx.Tx, partner, id, workspaceID string) (cardPayment, error) {
	p := cardPayment{partner: partner, id: id}
	err := tx.QueryRow(ctx, `SELECT workspace_id, card_id, merchant, amount_minor, currency FROM money_card_authorizations
		WHERE partner = $1 AND id = $2 AND approved AND ($3 = '' OR workspace_id = $3) FOR UPDATE`, partner, id, workspaceID).
		Scan(&p.workspaceID, &p.cardID, &p.merchant, &p.amountMinor, &p.currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: no card payment %q", ErrCardDispute, id)
	}
	if err != nil {
		return p, fmt.Errorf("economy: card payment: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT account_id, agent_id FROM money_cards WHERE id = $1 AND workspace_id = $2
		UNION ALL SELECT account_id, agent_id FROM money_reusable_cards WHERE id = $1 AND workspace_id = $2 LIMIT 1`, p.cardID, p.workspaceID).
		Scan(&p.accountID, &p.agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: card payment %q is on no card of Lens's", ErrCardDispute, id)
	}
	if err != nil {
		return p, fmt.Errorf("economy: card payment: %w", err)
	}
	return p, nil
}

// paymentPartner answers which partner's approved payment id is in workspaceID.
func (s *DualTokenStore) paymentPartner(ctx context.Context, workspaceID, id string) (string, error) {
	var partner string
	err := s.pool.QueryRow(ctx, `SELECT partner FROM money_card_authorizations WHERE workspace_id = $1 AND id = $2 AND approved
		ORDER BY created_at LIMIT 1`, workspaceID, id).Scan(&partner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: no card payment %q in this workspace", ErrCardDispute, id)
	}
	if err != nil {
		return "", fmt.Errorf("economy: card payment: %w", err)
	}
	return partner, nil
}

// creditCardPayment credits p's card account with up to want of p, in tx with p locked, as one money entry of kind under
// key: never more than is left of p after its refunds and its won dispute. It answers what it credited and the entry.
func (s *DualTokenStore) creditCardPayment(ctx context.Context, tx pgx.Tx, p cardPayment, want int64, kind, key string) (int64, string, error) {
	var back int64
	if err := tx.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(credited_minor) FROM money_card_refunds WHERE partner = $1 AND payment_id = $2), 0)::bigint
		+ COALESCE((SELECT sum(credited_minor) FROM money_card_disputes WHERE partner = $1 AND payment_id = $2 AND status = $3), 0)::bigint`,
		p.partner, p.id, CardDisputeWon).Scan(&back); err != nil {
		return 0, "", fmt.Errorf("economy: card payment: %w", err)
	}
	credit := min(want, p.amountMinor-back)
	if credit <= 0 {
		return 0, "", nil
	}
	// From the partner account the payment went to, in its funding.
	var partnerAccount, funding string
	err := tx.QueryRow(ctx, `SELECT p.account_id, p.funding FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		JOIN money_accounts a ON a.id = p.account_id
		WHERE e.workspace_id = $1 AND e.kind = 'card_charge' AND e.partner_ref = $2 AND a.purpose = $3 AND p.amount_minor > 0
		ORDER BY e.created_at LIMIT 1`, p.workspaceID, p.id, MoneyPartner).Scan(&partnerAccount, &funding)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: card payment %q has no charge on the ledger", ErrCardDispute, p.id)
	}
	if err != nil {
		return 0, "", fmt.Errorf("economy: card payment: %w", err)
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: p.workspaceID, Capability: CapabilityPaymentsOut, Kind: kind, IdempotencyKey: key,
		Funding: funding, Memo: p.merchant, Counterparty: p.merchant, PartnerRef: p.id, Postings: []MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -credit}, {AccountID: p.accountID, AmountMinor: credit}}}, s.screener)
	if err != nil {
		return 0, "", err
	}
	return credit, e.ID, nil
}

// SetSandboxDisputes sets the Sandbox's card disputes, which send their outcomes and refunds here; the Sandbox routes ask it.
func (s *DualTokenStore) SetSandboxDisputes(d *money.SandboxDisputes) {
	d.Intake = money.DisputeIntake{
		Outcome: func(ctx context.Context, partner string, o money.DisputeOutcome) error {
			_, err := s.ReceiveCardDisputeOutcome(ctx, partner, o)
			return err
		},
		Refund: func(ctx context.Context, partner string, r money.CardRefund) error {
			_, err := s.ReceiveCardRefund(ctx, partner, r)
			return err
		},
	}
	s.sandboxDisputes = d
}

// OpenCardDispute is by disputing workspaceID's card payment paymentID for reason: kept, then handed to the card's
// issuer. Disputed again, the payment's one dispute is answered, and handed to the issuer again if it has not taken it.
func (s *DualTokenStore) OpenCardDispute(ctx context.Context, workspaceID, paymentID, reason, by string) (CardDispute, error) {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" || utf8.RuneCountInString(reason) > MaxCardDisputeReason {
		return CardDispute{}, fmt.Errorf("%w: give a reason of up to %d characters", ErrCardDispute, MaxCardDisputeReason)
	}
	partner, err := s.paymentPartner(ctx, workspaceID, paymentID)
	if err != nil {
		return CardDispute{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := lockCardPayment(ctx, tx, partner, paymentID, workspaceID)
	if err != nil {
		return CardDispute{}, err
	}
	var d CardDispute
	err = tx.QueryRow(ctx, `INSERT INTO money_card_disputes (id, partner, payment_id, workspace_id, agent_id, card_id, merchant, amount_minor, currency,
		reason, opened_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (partner, payment_id) DO NOTHING RETURNING `+cardDisputeCols,
		"cdsp_"+uuid.NewString(), p.partner, p.id, p.workspaceID, p.agentID, p.cardID, p.merchant, p.amountMinor, p.currency, reason, by).Scan(d.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT `+cardDisputeCols+` FROM money_card_disputes WHERE partner = $1 AND payment_id = $2`, p.partner, p.id).Scan(d.fields()...)
	}
	if err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	if d.Status != CardDisputeOpened {
		return d, nil
	}
	return s.sendCardDispute(ctx, d)
}

// sendCardDispute hands opened dispute d to its card's issuer, and keeps that the issuer has it.
func (s *DualTokenStore) sendCardDispute(ctx context.Context, d CardDispute) (CardDispute, error) {
	if s.moneyLayer == nil {
		return d, fmt.Errorf("%w: no money layer takes card disputes on this server", ErrCardDispute)
	}
	provider, err := s.moneyLayer.Disputes(d.Partner)
	if err != nil {
		return d, err
	}
	var card string
	if err := s.pool.QueryRow(ctx, `SELECT partner_ref FROM money_cards WHERE id = $1 UNION ALL SELECT partner_ref FROM money_reusable_cards WHERE id = $1
		LIMIT 1`, d.CardID).Scan(&card); err != nil {
		return d, fmt.Errorf("economy: card dispute: %w", err)
	}
	ref, err := provider.Open(ctx, money.CardDispute{ID: d.ID, Card: card, Payment: d.PaymentID, Reason: d.Reason,
		Amount: money.Amount{Minor: d.AmountMinor, Asset: d.Currency}})
	if err != nil {
		return d, err
	}
	err = s.pool.QueryRow(ctx, `UPDATE money_card_disputes SET status = $2, partner_ref = $3, updated_at = now() WHERE id = $1 AND status = $4
		RETURNING `+cardDisputeCols, d.ID, CardDisputeWithIssuer, ref, CardDisputeOpened).Scan(d.fields()...)
	if errors.Is(err, pgx.ErrNoRows) { // handed over at the same moment
		return s.cardDispute(ctx, d.WorkspaceID, d.ID)
	}
	if err != nil {
		return d, fmt.Errorf("economy: card dispute: %w", err)
	}
	return d, nil
}

// ReceiveCardDisputeOutcome takes partner's outcome o of a dispute it took: won, the payment's card account is credited
// what is left of it, once; lost, nothing moves. The same outcome again answers the dispute as it is; the other,
// ErrCardDisputeDecided.
func (s *DualTokenStore) ReceiveCardDisputeOutcome(ctx context.Context, partner string, o money.DisputeOutcome) (CardDispute, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var paymentID string
	err = tx.QueryRow(ctx, `SELECT payment_id FROM money_card_disputes WHERE partner = $1 AND partner_ref = $2 AND partner_ref <> ''`,
		partner, o.Ref).Scan(&paymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CardDispute{}, fmt.Errorf("%w: %s took no dispute %q", ErrCardDisputeNotFound, partner, o.Ref)
	}
	if err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	p, err := lockCardPayment(ctx, tx, partner, paymentID, "") // every credit of this payment waits its turn here
	if err != nil {
		return CardDispute{}, err
	}
	var d CardDispute
	if err := tx.QueryRow(ctx, `SELECT `+cardDisputeCols+` FROM money_card_disputes WHERE partner = $1 AND payment_id = $2 FOR UPDATE`,
		partner, paymentID).Scan(d.fields()...); err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	status := CardDisputeLost
	if o.Won {
		status = CardDisputeWon
	}
	switch d.Status {
	case status:
		return d, nil
	case CardDisputeWon, CardDisputeLost:
		return d, fmt.Errorf("%w: it was %s", ErrCardDisputeDecided, d.Status)
	}
	var credited int64
	var entry string
	if o.Won {
		if credited, entry, err = s.creditCardPayment(ctx, tx, p, d.AmountMinor, "card_dispute_won", "card_dispute_won:"+d.ID); err != nil {
			return CardDispute{}, err
		}
	}
	if err := tx.QueryRow(ctx, `UPDATE money_card_disputes SET status = $2, credited_minor = $3, entry_id = $4, decided_at = now(), updated_at = now()
		WHERE id = $1 RETURNING `+cardDisputeCols, d.ID, status, credited, entry).Scan(d.fields()...); err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CardDispute{}, fmt.Errorf("economy: card dispute: %w", err)
	}
	return d, nil
}

// ReceiveCardRefund takes partner's merchant refund r: the payment's card account is credited r, up to what is left of
// the payment, once. Sent again under the same id, the same refund is answered and nothing more moves.
func (s *DualTokenStore) ReceiveCardRefund(ctx context.Context, partner string, r money.CardRefund) (CardPaymentRefund, error) {
	if err := r.Valid(); err != nil {
		return CardPaymentRefund{}, fmt.Errorf("%w: %w", ErrCardDispute, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CardPaymentRefund{}, fmt.Errorf("economy: card refund: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := lockCardPayment(ctx, tx, partner, r.Payment, "") // every credit of this payment waits its turn here
	if err != nil {
		return CardPaymentRefund{}, err
	}
	var out CardPaymentRefund
	err = tx.QueryRow(ctx, `SELECT `+cardRefundCols+` FROM money_card_refunds WHERE partner = $1 AND id = $2`, partner, r.ID).Scan(out.fields()...)
	if err == nil {
		if out.PaymentID != r.Payment {
			return CardPaymentRefund{}, fmt.Errorf("%w: refund %q is of another payment", ErrCardDispute, r.ID)
		}
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CardPaymentRefund{}, fmt.Errorf("economy: card refund: %w", err)
	}
	if !strings.EqualFold(r.Amount.Asset, p.currency) {
		return CardPaymentRefund{}, fmt.Errorf("%w: the payment was in %s, not %s", ErrCardDispute, p.currency, r.Amount.Asset)
	}
	credited, entry, err := s.creditCardPayment(ctx, tx, p, r.Amount.Minor, "card_refund", "card_refund:"+partner+":"+r.ID)
	if err != nil {
		return CardPaymentRefund{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO money_card_refunds (partner, id, payment_id, workspace_id, agent_id, card_id, merchant, amount_minor, currency,
		credited_minor, entry_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+cardRefundCols, partner, r.ID, p.id, p.workspaceID,
		p.agentID, p.cardID, p.merchant, r.Amount.Minor, p.currency, credited, entry).Scan(out.fields()...); err != nil {
		return CardPaymentRefund{}, fmt.Errorf("economy: card refund: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CardPaymentRefund{}, fmt.Errorf("economy: card refund: %w", err)
	}
	return out, nil
}

// SandboxCardDisputeOutcome has the Sandbox decide workspaceID's dispute id, as an issuer would.
func (s *DualTokenStore) SandboxCardDisputeOutcome(ctx context.Context, workspaceID, id string, won bool) (CardDispute, error) {
	d, err := s.cardDispute(ctx, workspaceID, id)
	if err != nil {
		return d, err
	}
	switch {
	case s.sandboxDisputes == nil:
		return d, fmt.Errorf("%w: the Sandbox's card disputes are off on this server", ErrCardDispute)
	case d.Partner != s.sandboxDisputes.Partner:
		return d, fmt.Errorf("%w: the card is %s's, not the Sandbox's", ErrCardDispute, d.Partner)
	case d.partnerRef == "":
		return d, fmt.Errorf("%w: the issuer has not taken the dispute yet", ErrCardDispute)
	}
	if err := s.sandboxTestPayment(ctx, workspaceID, d.PaymentID); err != nil {
		return d, err
	}
	if err := s.sandboxDisputes.Decide(ctx, d.partnerRef, won); err != nil {
		return d, err
	}
	return s.cardDispute(ctx, workspaceID, id)
}

// SandboxCardRefund has the Sandbox's merchant refund amountMinor of workspaceID's card payment paymentID, under key: the
// same key again is the same refund.
func (s *DualTokenStore) SandboxCardRefund(ctx context.Context, workspaceID, paymentID string, amountMinor int64, key string) (CardPaymentRefund, error) {
	if s.sandboxDisputes == nil {
		return CardPaymentRefund{}, fmt.Errorf("%w: the Sandbox's card refunds are off on this server", ErrCardDispute)
	}
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 200 {
		return CardPaymentRefund{}, fmt.Errorf("%w: a refund needs an idempotency key of up to 200 characters", ErrCardDispute)
	}
	partner, err := s.paymentPartner(ctx, workspaceID, paymentID)
	if err != nil {
		return CardPaymentRefund{}, err
	}
	if partner != s.sandboxDisputes.Partner {
		return CardPaymentRefund{}, fmt.Errorf("%w: the card is %s's, not the Sandbox's", ErrCardDispute, partner)
	}
	if err := s.sandboxTestPayment(ctx, workspaceID, paymentID); err != nil {
		return CardPaymentRefund{}, err
	}
	var currency string
	if err := s.pool.QueryRow(ctx, `SELECT currency FROM money_card_authorizations WHERE partner = $1 AND id = $2`, partner, paymentID).
		Scan(&currency); err != nil {
		return CardPaymentRefund{}, fmt.Errorf("economy: card refund: %w", err)
	}
	// The refund's id holds the workspace, the payment and the key, so no other workspace's key can name it.
	id := "sbx_re_" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(workspaceID+"\x00"+paymentID+"\x00"+key)).String()
	if err := s.sandboxDisputes.Refund(ctx, money.CardRefund{ID: id, Payment: paymentID, Amount: money.Amount{Minor: amountMinor, Asset: currency}}); err != nil {
		return CardPaymentRefund{}, err
	}
	var out CardPaymentRefund
	if err := s.pool.QueryRow(ctx, `SELECT `+cardRefundCols+` FROM money_card_refunds WHERE partner = $1 AND id = $2 AND workspace_id = $3`,
		partner, id, workspaceID).Scan(out.fields()...); err != nil {
		return CardPaymentRefund{}, fmt.Errorf("economy: card refund: %w", err)
	}
	return out, nil
}

// sandboxTestPayment refuses a Sandbox outcome or refund on a card payment that was not test money: the owner asks for
// those, so they never credit live money.
func (s *DualTokenStore) sandboxTestPayment(ctx context.Context, workspaceID, paymentID string) error {
	var live bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		WHERE e.workspace_id = $1 AND e.kind = 'card_charge' AND e.partner_ref = $2 AND p.funding <> $3)`, workspaceID, paymentID, FundingTest).
		Scan(&live); err != nil {
		return fmt.Errorf("economy: card payment: %w", err)
	}
	if live {
		return fmt.Errorf("%w: the Sandbox moves only test money, and this payment was not", ErrCardDispute)
	}
	return nil
}

func (s *DualTokenStore) cardDispute(ctx context.Context, workspaceID, id string) (CardDispute, error) {
	var d CardDispute
	err := s.pool.QueryRow(ctx, `SELECT `+cardDisputeCols+` FROM money_card_disputes WHERE workspace_id = $1 AND id = $2`, workspaceID, id).
		Scan(d.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrCardDisputeNotFound
	}
	if err != nil {
		return d, fmt.Errorf("economy: card dispute: %w", err)
	}
	return d, nil
}

// CardDisputes is workspaceID's card disputes, newest first, at most 200.
func (s *DualTokenStore) CardDisputes(ctx context.Context, workspaceID string) ([]CardDispute, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardDisputeCols+` FROM money_card_disputes WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 200`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: card disputes: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardDispute, error) {
		var d CardDispute
		return d, r.Scan(d.fields()...)
	})
}

// CardRefunds is workspaceID's card refunds, newest first, at most 200.
func (s *DualTokenStore) CardRefunds(ctx context.Context, workspaceID string) ([]CardPaymentRefund, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardRefundCols+` FROM money_card_refunds WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 200`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: card refunds: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardPaymentRefund, error) {
		var out CardPaymentRefund
		return out, r.Scan(out.fields()...)
	})
}

// CardExportRow is one line of the card export: a payment, a dispute or a refund.
type CardExportRow struct {
	Type          string // payment, dispute or refund
	At            time.Time
	ID            string
	PaymentID     string
	CardID        string
	AgentID       string
	Merchant      string
	AmountMinor   int64
	Currency      string
	Status        string // a dispute's
	CreditedMinor int64  // what a dispute or refund credited
	Reason        string // a dispute's
}

// CardExport is workspaceID's card payments, disputes and refunds made in [from, to), oldest first.
func (s *DualTokenStore) CardExport(ctx context.Context, workspaceID string, from, to time.Time) ([]CardExportRow, error) {
	if !to.After(from) || to.Sub(from) > MaxCardExportDays*24*time.Hour {
		return nil, fmt.Errorf("%w: an export is from one day to a later one, up to %d days", ErrCardDispute, MaxCardExportDays)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT 'payment', a.created_at, a.id, a.id, a.card_id, COALESCE(c.agent_id, r.agent_id, ''), a.merchant, a.amount_minor, a.currency, '', 0::bigint, ''
		  FROM money_card_authorizations a
		  LEFT JOIN money_cards c ON c.id = a.card_id AND c.workspace_id = a.workspace_id
		  LEFT JOIN money_reusable_cards r ON r.id = a.card_id AND r.workspace_id = a.workspace_id
		 WHERE a.workspace_id = $1 AND a.approved AND a.created_at >= $2 AND a.created_at < $3
		UNION ALL
		SELECT 'dispute', created_at, id, payment_id, card_id, agent_id, merchant, amount_minor, currency, status, credited_minor, reason
		  FROM money_card_disputes WHERE workspace_id = $1 AND created_at >= $2 AND created_at < $3
		UNION ALL
		SELECT 'refund', created_at, id, payment_id, card_id, agent_id, merchant, amount_minor, currency, '', credited_minor, ''
		  FROM money_card_refunds WHERE workspace_id = $1 AND created_at >= $2 AND created_at < $3
		ORDER BY 2, 1, 3`, workspaceID, from, to)
	if err != nil {
		return nil, fmt.Errorf("economy: card export: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardExportRow, error) {
		var x CardExportRow
		return x, r.Scan(&x.Type, &x.At, &x.ID, &x.PaymentID, &x.CardID, &x.AgentID, &x.Merchant, &x.AmountMinor, &x.Currency, &x.Status,
			&x.CreditedMinor, &x.Reason)
	})
}

// MinorDecimal is amount minor in currency as a plain decimal, as a spreadsheet reads it: "45.00".
func MinorDecimal(minor int64, currency string) string {
	s := minorString(minor, currency)
	return s[strings.IndexByte(s, ' ')+1:]
}
