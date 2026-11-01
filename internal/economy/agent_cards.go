package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// agent_cards.go — B19.12: AGENT CARDS IN TEST MODE, THROUGH STRIPE ISSUING.
//
// An agent may hold one virtual card. Each purchase with it is an authorisation request that Lens approves
// or declines in real time: under the agent's row lock, the agent's spending rules (B19.2) judge it as a
// payment — active hours, limits per payment, day and month, the approval amount, a pause — and its balance
// must cover it. Approved, it debits the agent in ONE transaction with its record: an lxc_ledger row of
// type LXCTypeAgentCard off the workspace's balance, and an agent_postings entry of kind 'card' (agent →
// spend), so the agent's balance, the workspace's and the rules' day and month all count it. EVERY request —
// approved or declined, a card no agent holds included — is a row of agent_card_authorizations (migration
// 0156), append-only.

// LXCTypeAgentCard marks an agent's card purchase in lxc_ledger — LXC that left for a merchant, which no
// revenue sum over type 'spend' may count as Talyvor's.
const LXCTypeAgentCard = "agent_card"

// ErrAgentHasCard: the agent already holds a card.
var ErrAgentHasCard = errors.New("economy: this agent already holds a card")

// ErrNoAgentCard: the agent holds no card.
var ErrNoAgentCard = errors.New("economy: this agent holds no card")

// ErrAgentCardFrozen is why a purchase on a frozen card is declined (B17.111).
var ErrAgentCardFrozen = errors.New("the card is frozen: unfreeze it to buy with it")

// AgentCard is an agent's virtual card, as Stripe issued it. Livemode is always false (class RED).
type AgentCard struct {
	ID                 string    `json:"id"`
	AgentID            string    `json:"agent_id"`
	StripeCardholderID string    `json:"stripe_cardholder_id"`
	Last4              string    `json:"last4"`
	ExpMonth           int       `json:"exp_month"`
	ExpYear            int       `json:"exp_year"`
	Currency           string    `json:"currency"`
	Livemode           bool      `json:"livemode"`
	Frozen             bool      `json:"frozen"` // B17.111: every purchase on it is declined
	CreatedAt          time.Time `json:"created_at"`
}

// SaveAgentCard records the card Stripe issued to agentID.
func (s *DualTokenStore) SaveAgentCard(ctx context.Context, workspaceID string, c AgentCard) (AgentCard, error) {
	if c.Livemode {
		return c, errors.New("economy: an agent card must be a test-mode card")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, c.AgentID); err != nil {
		return c, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO agent_cards (id, workspace_id, agent_id, stripe_cardholder_id, last4, exp_month, exp_year, currency)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (agent_id) DO NOTHING RETURNING created_at`,
		c.ID, workspaceID, c.AgentID, c.StripeCardholderID, c.Last4, c.ExpMonth, c.ExpYear, c.Currency).Scan(&c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrAgentHasCard
	}
	if err != nil {
		return c, fmt.Errorf("economy: save agent card: %w", err)
	}
	return c, tx.Commit(ctx)
}

// GetAgentCard reads the card agentID holds; ErrAgentNotFound for an agent not in workspaceID, ErrNoAgentCard
// for one with no card.
func (s *DualTokenStore) GetAgentCard(ctx context.Context, workspaceID, agentID string) (AgentCard, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return AgentCard{}, fmt.Errorf("economy: agent card: %w", err)
	}
	if !exists {
		return AgentCard{}, ErrAgentNotFound
	}
	c := AgentCard{AgentID: agentID}
	err := s.pool.QueryRow(ctx, `SELECT id, stripe_cardholder_id, last4, exp_month, exp_year, currency, livemode, frozen, created_at
		FROM agent_cards WHERE agent_id = $1`, agentID).Scan(&c.ID, &c.StripeCardholderID, &c.Last4, &c.ExpMonth, &c.ExpYear,
		&c.Currency, &c.Livemode, &c.Frozen, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNoAgentCard
	}
	return c, err
}

// SetAgentCardFrozen freezes or unfreezes the card agentID holds (B17.111). It takes the agent's row lock, as a
// purchase does, so a purchase being decided finishes first and every later one sees the card as it is now.
func (s *DualTokenStore) SetAgentCardFrozen(ctx context.Context, workspaceID, agentID string, frozen bool) (AgentCard, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentCard{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return AgentCard{}, err
	}
	if !frozen {
		if err := refuseIfLockedDown(ctx, tx, workspaceID); err != nil { // B41.116
			return AgentCard{}, err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_cards SET frozen = $3 WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID, frozen)
	if err != nil {
		return AgentCard{}, fmt.Errorf("economy: freeze agent card: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return AgentCard{}, ErrNoAgentCard
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentCard{}, err
	}
	return s.GetAgentCard(ctx, workspaceID, agentID)
}

// CardAuthorization is one authorisation request Stripe sent for a card.
type CardAuthorization struct {
	EventID             string // evt_…: the request, and the row's key
	AuthorizationID     string // iauth_…
	CardID              string
	AmountMinor         int64 // pending_request.amount, in the card's currency
	Currency            string
	MerchantAmountMinor int64
	MerchantCurrency    string
	MerchantName        string
	MerchantCategory    string
	MerchantID          string // the merchant's network id: with the amount, what an approval is for
	Livemode            bool
	At                  time.Time
	// Priced: the amount's USD value at the day's ECB rate. Refusal, when set, declines the request before
	// the agent is judged (live mode, no rate, a currency Lens cannot price).
	RateDate          *time.Time
	ECBUSDPerEUR      string
	ECBCurrencyPerEUR string
	USDMicros         int64
	Refusal           string
}

// CardDecision is what Lens answered.
type CardDecision struct {
	Approved    bool   `json:"approved"`
	Reason      string `json:"reason"`
	ApprovalID  string `json:"approval_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	WorkspaceID string `json:"-"`
	AmountULXC  int64  `json:"amount_ulxc"`
	testFunded  int64  // B22.1: the test-funded credits an approved purchase took
}

// ULXCPerUSDMicro is how many µLXC one µUSD buys at the peg (10 at $0.10).
var ULXCPerUSDMicro = int64(math.Round(1 / LXCUSDValue))

const cardApprovedReason = "within the agent's rules and balance"

// AuthorizeAgentCard decides one authorisation request and records it. A request already decided (Stripe
// sent the same event again) returns the decision on record and moves nothing.
func (s *DualTokenStore) AuthorizeAgentCard(ctx context.Context, a CardAuthorization) (CardDecision, error) {
	if d, ok, err := s.recordedCardDecision(ctx, a.EventID); err != nil || ok {
		return d, err
	}
	var d CardDecision
	err := s.pool.QueryRow(ctx, `SELECT workspace_id, agent_id FROM agent_cards WHERE id = $1`, a.CardID).Scan(&d.WorkspaceID, &d.AgentID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s.declineCard(ctx, a, d, "the card is not one Talyvor issued to an agent")
	case err != nil:
		return d, fmt.Errorf("economy: card's agent: %w", err)
	case a.Livemode:
		return s.declineCard(ctx, a, d, "agent cards take no real money until a licensed partner issues them (class RED)")
	case a.Refusal != "":
		return s.declineCard(ctx, a, d, a.Refusal)
	case a.AmountMinor <= 0:
		return s.declineCard(ctx, a, d, "the request carries no amount")
	}
	d.AmountULXC = a.USDMicros * ULXCPerUSDMicro
	decided, err := s.approveCard(ctx, a, d)
	if err != nil { // still a row: declined, with what went wrong
		return s.declineCard(ctx, a, d, "Talyvor could not decide this request: "+err.Error())
	}
	return decided, nil
}

// approveCard judges a priced request for the agent holding its card, debits it if approved, and records it.
func (s *DualTokenStore) approveCard(ctx context.Context, a CardAuthorization, d CardDecision) (CardDecision, error) {

	fp := sha256.Sum256([]byte("card\x00" + a.CardID + "\x00" + a.MerchantID + "\x00" + a.Currency + "\x00" + strconv.FormatInt(a.AmountMinor, 10)))
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]), At: a.At,
		Payee: Payee{Kind: "merchant", ID: a.MerchantID, Name: a.MerchantName}})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return d, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, d.WorkspaceID, d.AgentID); err != nil {
		return d, err
	}
	var frozen bool
	if err := tx.QueryRow(ctx, `SELECT frozen FROM agent_cards WHERE id = $1`, a.CardID).Scan(&frozen); err != nil {
		return d, err
	}
	if frozen {
		_ = tx.Rollback(ctx)
		return s.declineCard(ctx, a, d, ErrAgentCardFrozen.Error())
	}
	bal, err := accountBalance(ctx, tx, d.WorkspaceID, agentAccount(d.AgentID))
	if err != nil {
		return d, err
	}
	if bal < d.AmountULXC {
		_ = tx.Rollback(ctx)
		return s.declineCard(ctx, a, d, fmt.Sprintf("the agent holds %s LXC and this purchase costs %s LXC", lxcString(bal), lxcString(d.AmountULXC)))
	}
	// B22.1: cards are RED — test-funded credits only, until the operator clears them for real money.
	if d.testFunded, err = spendForCapability(ctx, tx, d.WorkspaceID, CapabilityAgentCard, d.AmountULXC); err != nil {
		if !errors.Is(err, ErrCapabilityNotCleared) {
			return d, err
		}
		_ = tx.Rollback(ctx)
		return s.declineCard(ctx, a, d, err.Error())
	}
	if err := enforceAgentRules(ctx, tx, d.WorkspaceID, d.AgentID, d.AmountULXC, a.AuthorizationID); err != nil {
		if !errors.Is(err, ErrAgentRule) && !errors.Is(err, ErrApprovalRequired) {
			return d, err
		}
		err = s.refusedMovement(ctx, tx, err) // files the approval a request above the approval amount needs
		var need *ApprovalNeededError
		if errors.As(err, &need) {
			d.ApprovalID = need.ApprovalID
		} else if !errors.Is(err, ErrAgentRule) {
			return d, err
		}
		_ = tx.Rollback(ctx)
		return s.declineCard(ctx, a, d, err.Error())
	}
	wsBal, minted, wsSpent, err := readLXCBalance(ctx, tx, d.WorkspaceID)
	if err != nil {
		return d, err
	}
	if wsBal < d.AmountULXC { // cannot happen while Σ agent balances ≤ the workspace's; refused if it does
		_ = tx.Rollback(ctx)
		return s.declineCard(ctx, a, d, "the workspace's balance does not cover this purchase")
	}
	if err := insertLXCLedger(ctx, tx, d.WorkspaceID, -d.AmountULXC, wsBal-d.AmountULXC, LXCTypeAgentCard,
		"agent card purchase: "+a.MerchantName, map[string]interface{}{"agent_id": d.AgentID, "authorization_id": a.AuthorizationID}); err != nil {
		return d, err
	}
	if err := writeLXCBalance(ctx, tx, d.WorkspaceID, wsBal-d.AmountULXC, minted, wsSpent+d.AmountULXC); err != nil {
		return d, err
	}
	if err := postEntry(ctx, tx, d.WorkspaceID, "card", a.AuthorizationID,
		leg{agentAccount(d.AgentID), -d.AmountULXC}, leg{"spend", d.AmountULXC}); err != nil {
		return d, err
	}
	d.Approved, d.Reason = true, cardApprovedReason
	if err := insertCardAuthorization(ctx, tx, a, d); err != nil {
		return d, err
	}
	if err := tx.Commit(ctx); err != nil {
		if d2, ok, rerr := s.recordedCardDecision(ctx, a.EventID); rerr == nil && ok {
			return d2, nil // a concurrent delivery of the same event decided it first
		}
		return d, err
	}
	return d, nil
}

func (s *DualTokenStore) declineCard(ctx context.Context, a CardAuthorization, d CardDecision, reason string) (CardDecision, error) {
	d.Approved, d.Reason, d.testFunded = false, reason, 0 // a declined request took nothing
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return d, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertCardAuthorization(ctx, tx, a, d); err != nil {
		if d2, ok, rerr := s.recordedCardDecision(ctx, a.EventID); rerr == nil && ok {
			return d2, nil
		}
		return d, err
	}
	return d, tx.Commit(ctx)
}

func insertCardAuthorization(ctx context.Context, tx pgx.Tx, a CardAuthorization, d CardDecision) error {
	var usdMicros, ulxc any
	if a.Refusal == "" && a.USDMicros > 0 {
		usdMicros, ulxc = a.USDMicros, a.USDMicros*ULXCPerUSDMicro
	}
	_, err := tx.Exec(ctx, `INSERT INTO agent_card_authorizations (id, authorization_id, card_id, workspace_id, agent_id, approved,
		reason, approval_id, amount_minor, currency, merchant_amount_minor, merchant_currency, merchant_name, merchant_category,
		rate_date, ecb_usd_per_eur, ecb_currency_per_eur, amount_usd_micros, amount_ulxc, livemode, created_at, test_funded_ulxc)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::numeric, $17::numeric, $18, $19, $20, $21, $22)`,
		a.EventID, a.AuthorizationID, a.CardID, d.WorkspaceID, d.AgentID, d.Approved, d.Reason, d.ApprovalID, a.AmountMinor,
		a.Currency, a.MerchantAmountMinor, a.MerchantCurrency, a.MerchantName, a.MerchantCategory, a.RateDate,
		nullIfEmpty(a.ECBUSDPerEUR), nullIfEmpty(a.ECBCurrencyPerEUR), usdMicros, ulxc, a.Livemode, a.At, d.testFunded)
	if err != nil {
		return fmt.Errorf("economy: record card authorisation: %w", err)
	}
	return nil
}

func (s *DualTokenStore) recordedCardDecision(ctx context.Context, eventID string) (CardDecision, bool, error) {
	var d CardDecision
	var ulxc *int64
	err := s.pool.QueryRow(ctx, `SELECT approved, reason, approval_id, agent_id, workspace_id, amount_ulxc
		FROM agent_card_authorizations WHERE id = $1`, eventID).Scan(&d.Approved, &d.Reason, &d.ApprovalID, &d.AgentID, &d.WorkspaceID, &ulxc)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("economy: card authorisation on record: %w", err)
	}
	if ulxc != nil {
		d.AmountULXC = *ulxc
	}
	return d, true, nil
}

// CardAuthorizationRecord is one row of an agent's card authorisations, as the card's screen shows it.
type CardAuthorizationRecord struct {
	ID                string     `json:"id"`
	AuthorizationID   string     `json:"authorization_id"`
	Approved          bool       `json:"approved"`
	Reason            string     `json:"reason"`
	ApprovalID        string     `json:"approval_id,omitempty"`
	AmountMinor       int64      `json:"amount_minor"`
	Currency          string     `json:"currency"`
	MerchantName      string     `json:"merchant_name"`
	MerchantCategory  string     `json:"merchant_category"`
	RateDate          *time.Time `json:"rate_date,omitempty"`
	ECBUSDPerEUR      *string    `json:"ecb_usd_per_eur,omitempty"`
	ECBCurrencyPerEUR *string    `json:"ecb_currency_per_eur,omitempty"`
	AmountUSDMicros   *int64     `json:"amount_usd_micros,omitempty"`
	AmountULXC        *int64     `json:"amount_ulxc,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// ListAgentCardAuthorizations reads an agent's card authorisations, newest first.
func (s *DualTokenStore) ListAgentCardAuthorizations(ctx context.Context, workspaceID, agentID string, limit int) ([]CardAuthorizationRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, authorization_id, approved, reason, approval_id, amount_minor, currency, merchant_name,
		merchant_category, rate_date, ecb_usd_per_eur::text, ecb_currency_per_eur::text, amount_usd_micros, amount_ulxc, created_at
		FROM agent_card_authorizations WHERE workspace_id = $1 AND agent_id = $2 ORDER BY created_at DESC, id LIMIT $3`,
		workspaceID, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: card authorisations: %w", err)
	}
	defer rows.Close()
	out := []CardAuthorizationRecord{}
	for rows.Next() {
		var r CardAuthorizationRecord
		if err := rows.Scan(&r.ID, &r.AuthorizationID, &r.Approved, &r.Reason, &r.ApprovalID, &r.AmountMinor, &r.Currency,
			&r.MerchantName, &r.MerchantCategory, &r.RateDate, &r.ECBUSDPerEUR, &r.ECBCurrencyPerEUR, &r.AmountUSDMicros,
			&r.AmountULXC, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
