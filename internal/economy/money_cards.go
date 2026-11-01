package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/screening"
)

// money_cards.go — B46.10: CARDS ON THE MONEY LAYER — SINGLE-USE, LOCKED TO ONE MERCHANT, CAPPED.
//
// An agent paying for one job — "book a trip", "build a website" — asks for a batch of cards: one per merchant, each
// capped. The batch moves nothing until the owner or an admin approves it, once, for every card in it. Approved, each
// card's cap is held out of the agent's account in the batch's currency, its merchant screened, and the card issued
// through the money layer's Cards (B46.3) — the adapter switched on once agent_card (RED) is cleared, the sandbox
// until then — under the card's own id, so approving again issues nothing twice. The money layer's gate asks
// MoneyLimits, which lets through only a card of an approved batch, for its own merchant, cap and account.
//
// Each card takes one charge. The issuer asks Lens as a charge is made (AuthorizeCardCharge): it is approved only at
// the card's merchant, in its currency, up to its cap, within 24 hours of issue, only once, and not while the agent is
// paused. The approved charge
// moves what it spent out of the hold to the partner, gives the rest back to the agent's account, and closes the card
// at the issuer. A card unused 24 hours after issue is closed and its whole cap given back (RunCardExpiries).

var (
	// ErrCardBatch: a batch without cards, with more than MaxCardsPerBatch, two for one merchant, or without a key.
	ErrCardBatch = errors.New("economy: invalid card batch")
	// ErrCardBatchNotFound: no such card batch in this workspace, or not the agent's.
	ErrCardBatchNotFound = errors.New("economy: no such card batch")
)

// The states of a card batch, and of a card in one.
const (
	CardBatchAwaitingApproval = "awaiting_approval"
	CardBatchIssuing          = "issuing"
	CardBatchIssued           = "issued"

	MoneyCardAwaitingApproval = "awaiting_approval"
	MoneyCardIssuing          = "issuing" // its cap held; the issuer not yet answered
	MoneyCardActive           = "active"
	MoneyCardUsed             = "used"    // its one charge approved; closed
	MoneyCardExpired          = "expired" // unused for 24 hours; closed
	MoneyCardFailed           = "failed"  // the issuer refused it; its cap given back

	MaxCardsPerBatch = 10
	MoneyCardLife    = 24 * time.Hour
)

// MoneyCard is one single-use card in a batch.
type MoneyCard struct {
	ID         string     `json:"id"`
	Line       int        `json:"line"`
	Merchant   string     `json:"merchant"`
	CapMinor   int64      `json:"cap_minor"`
	Currency   string     `json:"currency"`
	Status     string     `json:"status"`
	Partner    string     `json:"partner,omitempty"`
	Last4      string     `json:"last4,omitempty"`
	SpentMinor int64      `json:"spent_minor"`
	FormFactor string     `json:"form_factor"` // virtual, physical or unknown (B53.1)
	Detail     string     `json:"detail,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`

	accountID, walletRef, holdAccountID, partnerRef string
}

// CardBatch is an agent's cards for one job, approved together.
type CardBatch struct {
	ID             string      `json:"id"`
	WorkspaceID    string      `json:"workspace_id"`
	AgentID        string      `json:"agent_id"`
	IdempotencyKey string      `json:"idempotency_key"`
	Purpose        string      `json:"purpose"`
	Currency       string      `json:"currency"`
	Funding        string      `json:"funding"`
	TotalMinor     int64       `json:"total_minor"`
	Status         string      `json:"status"`
	Cards          []MoneyCard `json:"cards"`
	ApprovedBy     string      `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time  `json:"approved_at,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
}

// normalise trims and checks what a batch asks for, and totals its caps.
func (in *CardBatch) normalise() error {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Purpose = strings.Join(strings.Fields(in.Purpose), " ")
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrCardBatch, msg) }
	switch {
	case in.AgentID == "":
		return bad("cards are an agent's: name the agent (agent_id), or ask with the agent's own key")
	case in.Currency != CurrencyGBP && in.Currency != CurrencyEUR && in.Currency != CurrencyUSD:
		return bad(fmt.Sprintf("cards are in GBP, EUR or USD, not %q", in.Currency))
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return bad(fmt.Sprintf("funding is test or live, not %q", in.Funding))
	case in.IdempotencyKey == "" || len(in.IdempotencyKey) > 200:
		return bad("a batch carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 200 characters")
	case len(in.Purpose) > 140:
		return bad("a batch's purpose is up to 140 characters")
	case len(in.Cards) == 0 || len(in.Cards) > MaxCardsPerBatch:
		return bad(fmt.Sprintf("a batch has from 1 to %d cards, not %d", MaxCardsPerBatch, len(in.Cards)))
	}
	seen := map[string]bool{}
	in.TotalMinor = 0
	for i := range in.Cards {
		c := &in.Cards[i]
		c.Line, c.Currency = i+1, in.Currency
		c.Merchant = strings.Join(strings.Fields(c.Merchant), " ")
		switch m := strings.ToLower(c.Merchant); {
		case m == "" || len(m) > 100:
			return bad(fmt.Sprintf("card %d names its one merchant, up to 100 characters", c.Line))
		case seen[m]:
			return bad(fmt.Sprintf("one card per merchant: %s has two", c.Merchant))
		case c.CapMinor <= 0 || c.CapMinor > 1<<40:
			return bad(fmt.Sprintf("card %d's cap_minor is a positive number of pence or cents", c.Line))
		default:
			seen[m] = true
		}
		in.TotalMinor += c.CapMinor
	}
	return nil
}

func (b CardBatch) same(in CardBatch) bool {
	if b.AgentID != in.AgentID || b.Currency != in.Currency || b.Funding != in.Funding || b.Purpose != in.Purpose || len(b.Cards) != len(in.Cards) {
		return false
	}
	for i, c := range b.Cards {
		if c.Merchant != in.Cards[i].Merchant || c.CapMinor != in.Cards[i].CapMinor {
			return false
		}
	}
	return true
}

func cardRoute(workspaceID, agentID string) money.Route {
	return money.Route{Workspace: workspaceID, Agent: agentID, Capability: CapabilityAgentCard}
}

// sameMerchant is whether a charge's merchant is the one a card is locked to.
func sameMerchant(card, charge string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(card), " "), strings.Join(strings.Fields(charge), " "))
}

// RequestCardBatch files in.AgentID's batch of cards in workspaceID under in.IdempotencyKey, awaiting one approval.
// The same key again answers the batch it first filed; with other cards, ErrIdempotencyKeyReused.
func (s *DualTokenStore) RequestCardBatch(ctx context.Context, workspaceID string, in CardBatch) (CardBatch, error) {
	if s.moneyLayer == nil {
		return CardBatch{}, ErrAgentWalletsOff
	}
	if err := in.normalise(); err != nil {
		return CardBatch{}, err
	}
	prior, err := s.cardBatch(ctx, workspaceID, "", "", in.IdempotencyKey)
	switch {
	case err == nil && prior.same(in):
		return prior, nil
	case err == nil:
		return CardBatch{}, ErrIdempotencyKeyReused
	case !errors.Is(err, ErrCardBatchNotFound):
		return CardBatch{}, err
	}
	from, err := s.payingAccount(ctx, workspaceID, OutsidePayment{AgentID: in.AgentID, Currency: in.Currency})
	if err != nil {
		return CardBatch{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CardBatch{}, fmt.Errorf("economy: card batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id := "mcb_" + uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO money_card_batches (id, workspace_id, agent_id, idempotency_key, purpose, currency, funding, total_minor, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, id, workspaceID, in.AgentID, in.IdempotencyKey, in.Purpose, in.Currency, in.Funding,
		in.TotalMinor, CardBatchAwaitingApproval); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the same key, asked at the same moment: answer as that one did
			_ = tx.Rollback(ctx)
			return s.RequestCardBatch(ctx, workspaceID, in)
		}
		return CardBatch{}, fmt.Errorf("economy: card batch: %w", err)
	}
	for _, c := range in.Cards {
		if _, err := tx.Exec(ctx, `INSERT INTO money_cards (id, batch_id, workspace_id, agent_id, line, merchant, cap_minor, currency, account_id,
			wallet_ref, status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, "mcard_"+uuid.NewString(), id, workspaceID, in.AgentID,
			c.Line, c.Merchant, c.CapMinor, in.Currency, from.id, from.partnerRef, MoneyCardAwaitingApproval); err != nil {
			return CardBatch{}, fmt.Errorf("economy: card batch: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CardBatch{}, fmt.Errorf("economy: card batch: %w", err)
	}
	return s.cardBatch(ctx, workspaceID, "", id, "")
}

// ApproveCardBatch is by's one approval of workspaceID's batch id: every card's cap is held and every card issued,
// locked to its merchant and capped. Approved again, a batch already issued is answered as it stands, and a card the
// issuer did not answer is asked for again under its own id.
func (s *DualTokenStore) ApproveCardBatch(ctx context.Context, workspaceID, id, by string) (CardBatch, error) {
	if s.moneyLayer == nil {
		return CardBatch{}, ErrAgentWalletsOff
	}
	b, err := s.cardBatch(ctx, workspaceID, "", id, "")
	if err != nil || b.Status == CardBatchIssued {
		return b, err
	}
	if b.Status == CardBatchAwaitingApproval {
		if err := s.holdCardBatch(ctx, b, by); err != nil {
			return b, err
		}
		if b, err = s.cardBatch(ctx, workspaceID, "", id, ""); err != nil {
			return b, err
		}
	}
	ctx = context.WithoutCancel(ctx) // a caller that goes away mid-batch does not leave cards half issued
	rt := cardRoute(workspaceID, b.AgentID)
	partner, err := s.moneyLayer.Partner(ctx, money.CapCards, rt)
	if err != nil {
		return b, err
	}
	cards, err := s.moneyLayer.Cards(ctx, rt)
	if err != nil {
		return b, err
	}
	for _, c := range b.Cards {
		if c.Status != MoneyCardIssuing {
			continue
		}
		card, err := cards.Issue(ctx, money.IssueCard{Key: c.ID, Wallet: c.walletRef, Merchant: c.Merchant,
			Cap: money.Amount{Minor: c.CapMinor, Asset: c.Currency}})
		switch {
		case errors.Is(err, money.ErrPartnerDown):
			continue // still held: approving again asks again, under the same id
		case err != nil:
			// B50.1: refused because another approval at the same moment already issued it: that card stands.
			var status string
			if qerr := s.pool.QueryRow(ctx, `SELECT status FROM money_cards WHERE id = $1`, c.ID).Scan(&status); qerr != nil {
				return b, fmt.Errorf("economy: card batch: %w", qerr)
			}
			if status != MoneyCardIssuing {
				continue
			}
			if err := s.releaseCard(ctx, b, c, MoneyCardFailed, err.Error()); err != nil {
				return b, err
			}
			continue
		}
		if _, err := s.pool.Exec(ctx, `UPDATE money_cards SET status = $2, partner = $3, partner_ref = $4, last4 = $5,
			expires_at = now() + $6 * interval '1 second', form_factor = $8, updated_at = now() WHERE id = $1 AND status = $7`, c.ID, MoneyCardActive, partner,
			card.Ref, card.Last4, MoneyCardLife.Seconds(), MoneyCardIssuing, money.KnownFormFactor(card.FormFactor)); err != nil {
			return b, fmt.Errorf("economy: card batch: %w", err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_card_batches SET status = $2 WHERE id = $1 AND NOT EXISTS
		(SELECT 1 FROM money_cards WHERE batch_id = $1 AND status = $3)`, b.ID, CardBatchIssued, MoneyCardIssuing); err != nil {
		return b, fmt.Errorf("economy: card batch: %w", err)
	}
	return s.cardBatch(ctx, workspaceID, "", id, "")
}

// holdCardBatch records by's approval of b and holds every card's cap out of the agent's account, once.
func (s *DualTokenStore) holdCardBatch(ctx context.Context, b CardBatch, by string) error {
	if s.screener == nil {
		return fmt.Errorf("economy: card batch: no screener is set, so no card is issued")
	}
	from, err := s.payingAccount(ctx, b.WorkspaceID, OutsidePayment{AgentID: b.AgentID, Currency: b.Currency})
	if err != nil {
		return err
	}
	// B50.1: screened before the locks, as a payment out is.
	for _, c := range b.Cards {
		if err := s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: b.WorkspaceID, ID: "card_issue:" + c.ID, Capability: CapabilityPaymentsOut,
			Direction: "out", Counterparty: c.Merchant, AmountMinor: c.CapMinor, Currency: c.Currency, Funding: b.Funding}); err != nil {
			return err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: card batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE money_card_batches SET status = $2, approved_by = $3, approved_at = now() WHERE id = $1 AND status = $4`,
		b.ID, CardBatchIssuing, by, CardBatchAwaitingApproval)
	if err != nil {
		return fmt.Errorf("economy: card batch: %w", err)
	}
	if tag.RowsAffected() == 0 { // approved at the same moment: that approval holds the money
		return nil
	}
	// The owner's one approval of the batch's total stands in for the agent's rules, but not for a pause; and live
	// money goes on a card only once agent_card itself is cleared for it, not payments_out alone.
	if err := refuseIfPaused(ctx, tx, b.AgentID); err != nil {
		return err
	}
	if b.Funding == FundingLive {
		c, _ := CapabilityByKey(CapabilityAgentCard)
		refusal, err := capabilityLive(ctx, tx, b.WorkspaceID, c)
		if err != nil {
			return err
		}
		if refusal != nil {
			return refusal
		}
	}
	// As a payment out does: every payment out in this currency waits its turn, and a payment in waits for the balance.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{from.companyID, from.id}); err != nil {
		return fmt.Errorf("economy: card batch: %w", err)
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		from.id, b.Funding).Scan(&held); err != nil {
		return fmt.Errorf("economy: card batch: %w", err)
	}
	if held < b.TotalMinor {
		return fmt.Errorf("%w: the account holds %s of %s money, and these cards are capped at %s together", ErrAgentFunds,
			minorString(held, b.Currency), b.Funding, minorString(b.TotalMinor, b.Currency))
	}
	hold, err := holdAccount(ctx, tx, b.WorkspaceID, b.Currency)
	if err != nil {
		return err
	}
	if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: b.WorkspaceID, Capability: CapabilityPaymentsOut, Kind: "card_hold",
		IdempotencyKey: "card_hold:" + b.ID, Funding: b.Funding, Memo: b.Purpose, Postings: []MoneyPosting{
			{AccountID: from.id, AmountMinor: -b.TotalMinor}, {AccountID: hold, AmountMinor: b.TotalMinor}}}, s.screener); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE money_cards SET status = $2, hold_account_id = $3, updated_at = now() WHERE batch_id = $1 AND status = $4`,
		b.ID, MoneyCardIssuing, hold, MoneyCardAwaitingApproval); err != nil {
		return fmt.Errorf("economy: card batch: %w", err)
	}
	return tx.Commit(ctx)
}

// releaseCard ends c in status with detail and gives what it holds back to the agent's account, once.
func (s *DualTokenStore) releaseCard(ctx context.Context, b CardBatch, c MoneyCard, status, detail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: card: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE money_cards SET status = $2, detail = $3, updated_at = now() WHERE id = $1 AND status IN ($4, $5)`,
		c.ID, status, detail, MoneyCardIssuing, MoneyCardActive)
	if err != nil {
		return fmt.Errorf("economy: card: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: b.WorkspaceID, Capability: CapabilityPaymentsOut, Kind: "card_release",
		IdempotencyKey: "card_release:" + c.ID, Funding: b.Funding, Memo: c.Merchant, Postings: []MoneyPosting{
			{AccountID: c.holdAccountID, AmountMinor: -c.CapMinor}, {AccountID: c.accountID, AmountMinor: c.CapMinor}}}, s.screener); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// heldMoneyCard is whether op is the money layer issuing a card of a batch the owner approved, whose cap is held: the
// card issuing under op's key, for that agent, merchant, cap and account.
func (s *DualTokenStore) heldMoneyCard(ctx context.Context, op money.Op) (bool, error) {
	if op.Capability != money.CapCards || op.Method != "issue" || !strings.HasPrefix(op.Key, "mcard_") {
		return false, nil
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_cards WHERE id = $1 AND workspace_id = $2 AND agent_id = $3
		AND status = $4 AND cap_minor = $5 AND currency = $6 AND merchant = $7 AND wallet_ref = $8)`, op.Key, op.Workspace, op.Agent,
		MoneyCardIssuing, op.Amount.Minor, op.Amount.Asset, op.Payee, op.Ref).Scan(&held)
	return held, err
}

// AuthorizeCardCharge is Lens's answer to partner asking whether a charge on one of its cards may go: approved only
// at the card's merchant, in its currency, up to its cap, before it expires, and once. The approved charge spends from
// the card's hold, gives the rest back to the agent's account and closes the card. Every request is recorded, and
// asked again under the same id it is answered the same.
func (s *DualTokenStore) AuthorizeCardCharge(ctx context.Context, partner string, a money.Authorization) (bool, string, error) {
	var approved bool
	var reason string
	err := s.pool.QueryRow(ctx, `SELECT approved, reason FROM money_card_authorizations WHERE partner = $1 AND id = $2`, partner, a.ID).
		Scan(&approved, &reason)
	if err == nil {
		return approved, reason, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if done, approved, reason, err := s.authorizeReusableCharge(ctx, tx, partner, a); done { // B53.2
		return approved, reason, err
	}
	var c MoneyCard
	var ws, agentID, funding string
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT c.id, c.workspace_id, c.agent_id, c.merchant, c.cap_minor, c.currency, c.status, c.account_id, c.hold_account_id,
		COALESCE(c.expires_at, now()), b.funding FROM money_cards c JOIN money_card_batches b ON b.id = c.batch_id
		WHERE c.partner = $1 AND c.partner_ref = $2 FOR UPDATE OF c`, partner, a.Card).
		Scan(&c.ID, &ws, &agentID, &c.Merchant, &c.CapMinor, &c.Currency, &c.Status, &c.accountID, &c.holdAccountID, &expires, &funding)
	var paused error
	if err == nil {
		paused = refuseIfPaused(ctx, tx, agentID)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		reason = "Lens issued no such card"
	case err != nil:
		return false, "", fmt.Errorf("economy: card charge: %w", err)
	case c.Status == MoneyCardUsed:
		reason = "the card has been used: it pays once"
	case c.Status == MoneyCardExpired || (c.Status == MoneyCardActive && !time.Now().Before(expires)):
		reason = "the card has expired"
	case c.Status != MoneyCardActive:
		reason = "the card is not active"
	case paused != nil:
		reason = paused.Error()
	case !sameMerchant(c.Merchant, a.Merchant):
		reason = "the card pays only " + c.Merchant
	case a.Amount.Asset != c.Currency:
		reason = "the card pays in " + c.Currency
	case a.Amount.Minor <= 0 || a.Amount.Minor > c.CapMinor:
		reason = "the charge is more than the card's cap of " + minorString(c.CapMinor, c.Currency)
	default:
		approved = true
	}
	d := a.Normal() // the charge's CardDetails
	if _, err := tx.Exec(ctx, `INSERT INTO money_card_authorizations (partner, id, card_id, workspace_id, merchant, amount_minor, currency, approved, reason,
		mcc, merchant_country, channel, wallet) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, partner, a.ID, c.ID, ws, a.Merchant,
		a.Amount.Minor, a.Amount.Asset, approved, reason, d.MCC, d.Country, d.Channel, d.Wallet); err != nil {
		return false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	if approved {
		from, err := s.payingAccount(ctx, ws, OutsidePayment{AccountID: c.accountID, Currency: c.Currency})
		if err != nil {
			return false, "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE money_cards SET status = $2, spent_minor = $3, updated_at = now() WHERE id = $1`,
			c.ID, MoneyCardUsed, a.Amount.Minor); err != nil {
			return false, "", fmt.Errorf("economy: card charge: %w", err)
		}
		if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityPaymentsOut, Kind: "card_charge",
			IdempotencyKey: "card_charge:" + c.ID, Funding: funding, Memo: c.Merchant, Counterparty: c.Merchant, PartnerRef: a.ID, Card: &d,
			Postings: []MoneyPosting{{AccountID: c.holdAccountID, AmountMinor: -c.CapMinor}, {AccountID: from.partnerID, AmountMinor: a.Amount.Minor},
				{AccountID: c.accountID, AmountMinor: c.CapMinor - a.Amount.Minor}}}, s.screener); err != nil {
			return false, "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	if approved {
		s.closeIssuedCard(ctx, ws, c.ID, a.Card)
	}
	return approved, reason, nil
}

// closeIssuedCard closes card id at its issuer. Lens declines every later charge on it whether or not the issuer
// answers, so a failure is only logged.
func (s *DualTokenStore) closeIssuedCard(ctx context.Context, workspaceID, id, ref string) {
	var agentID string
	if err := s.pool.QueryRow(ctx, `SELECT agent_id FROM money_cards WHERE id = $1`, id).Scan(&agentID); err == nil {
		var cards money.Cards
		if cards, err = s.moneyLayer.Cards(ctx, cardRoute(workspaceID, agentID)); err == nil {
			_, err = cards.Close(ctx, "close:"+id, ref)
		}
		if err == nil {
			return
		}
	}
	slog.Warn("economy: a used or expired card was not closed at its issuer; Lens declines its charges", slog.String("card_id", id))
}

// RunCardExpiries closes every card unused 24 hours after issue and gives its cap back, up to 200 a run.
func (s *DualTokenStore) RunCardExpiries(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.batch_id, c.id FROM money_cards c WHERE c.status = $1 AND c.expires_at <= $2
		ORDER BY c.expires_at LIMIT 200`, MoneyCardActive, now)
	if err != nil {
		return 0, fmt.Errorf("economy: card expiries: %w", err)
	}
	type due struct{ batch, card string }
	var dues []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.batch, &d.card); err != nil {
			rows.Close()
			return 0, err
		}
		dues = append(dues, d)
	}
	rows.Close()
	n := 0
	for _, d := range dues {
		b, err := s.cardBatch(ctx, "", "", d.batch, "")
		if err != nil {
			return n, err
		}
		for _, c := range b.Cards {
			if c.ID != d.card {
				continue
			}
			if err := s.releaseCard(ctx, b, c, MoneyCardExpired, "unused for 24 hours"); err != nil {
				return n, err
			}
			s.closeIssuedCard(ctx, b.WorkspaceID, c.ID, c.partnerRef)
			n++
		}
	}
	return n, nil
}

const cardBatchCols = `id, workspace_id, agent_id, idempotency_key, purpose, currency, funding, total_minor, status, approved_by, approved_at, created_at`

// cardBatch reads workspaceID's batch id, or the one under key, with its cards — with agentID set, only that agent's;
// workspaceID "" reads batch id in any workspace.
func (s *DualTokenStore) cardBatch(ctx context.Context, workspaceID, agentID, id, key string) (CardBatch, error) {
	var b CardBatch
	err := s.pool.QueryRow(ctx, `SELECT `+cardBatchCols+` FROM money_card_batches WHERE ($1 = '' OR workspace_id = $1) AND ($2 = '' OR agent_id = $2)
		AND (($3 <> '' AND id = $3) OR ($3 = '' AND $1 <> '' AND idempotency_key = $4))`, workspaceID, agentID, id, key).
		Scan(&b.ID, &b.WorkspaceID, &b.AgentID, &b.IdempotencyKey, &b.Purpose, &b.Currency, &b.Funding, &b.TotalMinor, &b.Status,
			&b.ApprovedBy, &b.ApprovedAt, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrCardBatchNotFound
	}
	if err != nil {
		return b, fmt.Errorf("economy: card batch: %w", err)
	}
	b.Cards, err = s.moneyCards(ctx, b.ID)
	return b, err
}

func (s *DualTokenStore) moneyCards(ctx context.Context, batchID string) ([]MoneyCard, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, line, merchant, cap_minor, currency, status, partner, last4, spent_minor, form_factor, detail, expires_at,
		account_id, wallet_ref, hold_account_id, partner_ref FROM money_cards WHERE batch_id = $1 ORDER BY line`, batchID)
	if err != nil {
		return nil, fmt.Errorf("economy: cards: %w", err)
	}
	defer rows.Close()
	out := []MoneyCard{}
	for rows.Next() {
		var c MoneyCard
		if err := rows.Scan(&c.ID, &c.Line, &c.Merchant, &c.CapMinor, &c.Currency, &c.Status, &c.Partner, &c.Last4, &c.SpentMinor, &c.FormFactor, &c.Detail,
			&c.ExpiresAt, &c.accountID, &c.walletRef, &c.holdAccountID, &c.partnerRef); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CardBatch reads workspaceID's batch id with its cards — with agentID set, only that agent's.
func (s *DualTokenStore) CardBatch(ctx context.Context, workspaceID, agentID, id string) (CardBatch, error) {
	if id == "" {
		return CardBatch{}, ErrCardBatchNotFound
	}
	return s.cardBatch(ctx, workspaceID, agentID, id, "")
}

// CardBatches is workspaceID's batches with their cards — with agentID set, only that agent's — newest first, at most
// 100.
func (s *DualTokenStore) CardBatches(ctx context.Context, workspaceID, agentID string) ([]CardBatch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardBatchCols+` FROM money_card_batches WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
		ORDER BY created_at DESC, id LIMIT 100`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: card batches: %w", err)
	}
	out := []CardBatch{}
	for rows.Next() {
		var b CardBatch
		if err := rows.Scan(&b.ID, &b.WorkspaceID, &b.AgentID, &b.IdempotencyKey, &b.Purpose, &b.Currency, &b.Funding, &b.TotalMinor, &b.Status,
			&b.ApprovedBy, &b.ApprovedAt, &b.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Cards, err = s.moneyCards(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
