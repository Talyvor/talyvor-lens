package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/money"
)

// card_spends.go — B59.21: APPROVAL BEFORE THE CARD. A CARD PROGRAMME IS REALTIME OR PREAPPROVED.
//
// Some issuers decide a card payment on the card's balance and limit alone and never ask Lens. So each card programme
// — the issuer the money layer routes an agent's cards to — has a mode. Realtime: the issuer asks Lens at each payment
// (AuthorizeCardCharge), today's path, unchanged. Preapproved, the default: the agent asks first (RequestCardSpend),
// naming the merchant and the amount, and Lens checks the agent's rules. Anything outside them waits for the owner's
// approval, filed in the approvals inbox under the request's own fingerprint and decided there with the owner's key;
// the agent asks again with the same key once it is approved. Only then does Lens act through the issuer: a one-time
// card for that amount, locked to the merchant (a batch of one, B46.10, its 24 hours and its expiry included), or the
// standing card the agent names — a subscription's — has its limit raised by it until CardSpendLife has passed, when
// RunCardSpendExpiries lowers it back. Either way the card can spend no more than what was approved.

// The modes of a card programme.
const (
	CardProgrammeRealtime    = "realtime"
	CardProgrammePreapproved = "preapproved"
)

// The states of a card spend.
const (
	CardSpendAsked            = "asked" // filed; the agent's rules refused it, or have not yet judged it
	CardSpendAwaitingApproval = "awaiting_approval"
	CardSpendIssuing          = "issuing" // approved; the issuer not yet answered
	CardSpendActive           = "active"  // the one-time card issued, or the standing card's limit raised
	CardSpendExpired          = "expired" // the standing card's raise past its time, and its limit lowered back
	CardSpendFailed           = "failed"  // the issuer refused the one-time card

	// CardSpendLife is how long a standing card's raise lasts unused: a one-time card's own life, until Nicolai sets one.
	CardSpendLife = MoneyCardLife
)

var (
	// ErrCardSpend: a card spend asked for without its merchant, amount, currency or key.
	ErrCardSpend = errors.New("economy: invalid card spend")
	// ErrCardSpendNotFound: no such card spend in this workspace, or not the agent's.
	ErrCardSpendNotFound = errors.New("economy: no such card spend")
	// ErrCardProgrammeRealtime: the agent's card programme asks Lens at each payment, so nothing is approved first.
	ErrCardProgrammeRealtime = errors.New("economy: this card programme is realtime: the issuer asks Talyvor at each payment, so the agent pays with its card and nothing is approved first")
)

// CardSpend is an agent's ask to pay a merchant by card, on a preapproved programme.
type CardSpend struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	AgentID        string     `json:"agent_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	Merchant       string     `json:"merchant"`
	AmountMinor    int64      `json:"amount_minor"`
	Currency       string     `json:"currency"`
	Funding        string     `json:"funding"`
	StandingCardID string     `json:"standing_card_id,omitempty"` // "" for a one-time card
	Status         string     `json:"status"`
	ApprovalID     string     `json:"approval_id,omitempty"`
	Detail         string     `json:"detail,omitempty"`
	Card           *MoneyCard `json:"card,omitempty"`       // the one-time card, once issued
	ExpiresAt      *time.Time `json:"expires_at,omitempty"` // when a standing card's raise ends
	CreatedAt      time.Time  `json:"created_at"`

	batchID       string
	raisedToMinor int64
}

// SetCardProgrammeModes sets each card programme's mode by its issuer's adapter name (LENS_CARD_PROGRAMMES); a
// programme not named is preapproved.
func (s *DualTokenStore) SetCardProgrammeModes(modes map[string]string) { s.cardProgrammes = modes }

// CardProgrammeMode is the mode of partner's card programme: realtime only where it is set so.
func (s *DualTokenStore) CardProgrammeMode(partner string) string {
	if s.cardProgrammes[partner] == CardProgrammeRealtime {
		return CardProgrammeRealtime
	}
	return CardProgrammePreapproved
}

func (in *CardSpend) normalise() error {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Merchant = strings.Join(strings.Fields(in.Merchant), " ")
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.StandingCardID = strings.TrimSpace(in.StandingCardID)
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrCardSpend, msg) }
	switch {
	case in.AgentID == "":
		return bad("a card spend is an agent's: name the agent (agent_id), or ask with the agent's own key")
	case in.Merchant == "" || len(in.Merchant) > 100:
		return bad("a card spend names its merchant, up to 100 characters")
	case in.AmountMinor <= 0 || in.AmountMinor > 1<<40:
		return bad("amount_minor is a positive number of pence or cents")
	case in.Currency != CurrencyGBP && in.Currency != CurrencyEUR && in.Currency != CurrencyUSD:
		return bad(fmt.Sprintf("cards are in GBP, EUR or USD, not %q", in.Currency))
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return bad(fmt.Sprintf("funding is test or live, not %q", in.Funding))
	case in.IdempotencyKey == "" || len(in.IdempotencyKey) > 200:
		return bad("a card spend carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 200 characters")
	}
	return nil
}

func (sp CardSpend) same(in CardSpend) bool {
	return sp.AgentID == in.AgentID && sp.Merchant == in.Merchant && sp.AmountMinor == in.AmountMinor && sp.Currency == in.Currency &&
		sp.Funding == in.Funding && sp.StandingCardID == in.StandingCardID
}

// RequestCardSpend is in.AgentID's ask to pay in.Merchant in.AmountMinor by card in workspaceID, under
// in.IdempotencyKey: on a one-time card, or by raising in.StandingCardID's limit. Within the agent's rules the card is
// issued or the limit raised at once; outside them it waits for the owner (an *ApprovalNeededError naming the
// approval), and the same key asked again once the owner approved carries it out. The same key answers the same spend;
// with another merchant, amount or card, ErrIdempotencyKeyReused. On a realtime programme, ErrCardProgrammeRealtime.
func (s *DualTokenStore) RequestCardSpend(ctx context.Context, workspaceID string, in CardSpend) (CardSpend, error) {
	if s.moneyLayer == nil {
		return CardSpend{}, ErrAgentWalletsOff
	}
	if err := in.normalise(); err != nil {
		return CardSpend{}, err
	}
	if in.StandingCardID != "" {
		card, err := s.reusableCard(ctx, workspaceID, in.AgentID, in.StandingCardID, "")
		if err != nil {
			return CardSpend{}, err
		}
		if card.Currency != in.Currency {
			return CardSpend{}, fmt.Errorf("%w: the standing card pays in %s", ErrCardSpend, card.Currency)
		}
		in.Funding = card.Funding
	}
	partner, err := s.moneyLayer.Partner(ctx, money.CapCards, cardRoute(workspaceID, in.AgentID))
	if err != nil {
		return CardSpend{}, err
	}
	if s.CardProgrammeMode(partner) == CardProgrammeRealtime {
		return CardSpend{}, ErrCardProgrammeRealtime
	}
	sp, err := s.cardSpend(ctx, workspaceID, "", "", in.IdempotencyKey)
	switch {
	case err == nil && !sp.same(in):
		return CardSpend{}, ErrIdempotencyKeyReused
	case errors.Is(err, ErrCardSpendNotFound):
		if _, err := s.payingAccount(ctx, workspaceID, OutsidePayment{AgentID: in.AgentID, Currency: in.Currency}); err != nil {
			return CardSpend{}, err
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO money_card_spends (id, workspace_id, agent_id, idempotency_key, merchant, amount_minor, currency,
			funding, standing_card_id, status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, "mcs_"+uuid.NewString(), workspaceID, in.AgentID,
			in.IdempotencyKey, in.Merchant, in.AmountMinor, in.Currency, in.Funding, in.StandingCardID, CardSpendAsked); err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23505" { // the same key, asked at the same moment, is read below
				return CardSpend{}, fmt.Errorf("economy: card spend: %w", err)
			}
		}
		return s.RequestCardSpend(ctx, workspaceID, in)
	case err != nil:
		return CardSpend{}, err
	}
	switch sp.Status {
	case CardSpendAsked, CardSpendAwaitingApproval:
		if err := s.judgeCardSpend(ctx, sp); err != nil {
			return sp, err
		}
	case CardSpendIssuing:
	default:
		return sp, nil
	}
	if sp, err = s.cardSpend(ctx, workspaceID, "", sp.ID, ""); err != nil || sp.Status != CardSpendIssuing {
		return sp, err
	}
	ctx = context.WithoutCancel(ctx) // a caller that goes away does not leave the card half issued
	if sp.StandingCardID != "" {
		if _, err := s.moveStandingLimit(ctx, sp, sp.AmountMinor, CardSpendIssuing, CardSpendActive); err != nil {
			return sp, err
		}
	} else if err := s.issueOneTimeCard(ctx, sp); err != nil {
		return sp, err
	}
	return s.cardSpend(ctx, workspaceID, "", sp.ID, "")
}

// judgeCardSpend lets the agent's rules judge sp, as any money leaving the agent is: within them sp is approved
// (issuing) and its value counts toward the agent's spend; above its approval amount sp waits for the owner, under its
// own fingerprint, so the owner's approval is of this spend alone; any other refusal leaves it asked, with the reason.
func (s *DualTokenStore) judgeCardSpend(ctx context.Context, sp CardSpend) error {
	key := "card_spend:" + sp.ID
	amount := money.Amount{Minor: sp.AmountMinor, Asset: sp.Currency}
	value, err := s.moneyValue(ctx, money.Op{Route: money.Route{Workspace: sp.WorkspaceID}, Amount: amount, Key: key})
	if err != nil {
		return err
	}
	from, err := s.payingAccount(ctx, sp.WorkspaceID, OutsidePayment{AgentID: sp.AgentID, Currency: sp.Currency})
	if err != nil {
		return err
	}
	fp := sha256.Sum256([]byte("card_spend\x00" + sp.ID))
	memo := minorString(sp.AmountMinor, sp.Currency) + " by card at " + sp.Merchant
	if sp.StandingCardID != "" {
		memo += ", on the agent's standing card"
	}
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]),
		Payee: Payee{Kind: "merchant", ID: "merchant:" + sp.Merchant, Name: sp.Merchant}, Memo: memo,
		PaidFrom: PaidFrom{Kind: "account", AccountID: from.id, Currency: sp.Currency, AmountMinor: sp.AmountMinor, funding: sp.Funding}})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: card spend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, sp.WorkspaceID, sp.AgentID); err != nil {
		return err
	}
	var need *ApprovalNeededError
	switch err := enforceAgentRules(ctx, tx, sp.WorkspaceID, sp.AgentID, value, key); {
	case errors.As(err, &need):
		_ = tx.Rollback(ctx)
		filed := s.fileApproval(ctx, need)
		if errors.As(filed, &need) && need.ApprovalID != "" {
			if _, err := s.pool.Exec(ctx, `UPDATE money_card_spends SET status = $2, approval_id = $3, detail = '', updated_at = now()
				WHERE id = $1 AND status IN ($4, $2)`, sp.ID, CardSpendAwaitingApproval, need.ApprovalID, CardSpendAsked); err != nil {
				return fmt.Errorf("economy: card spend: %w", err)
			}
		}
		return filed
	case errors.Is(err, ErrAgentRule):
		_ = tx.Rollback(ctx)
		if _, uerr := s.pool.Exec(ctx, `UPDATE money_card_spends SET detail = $2, updated_at = now() WHERE id = $1 AND status = $3`,
			sp.ID, strings.TrimPrefix(err.Error(), ErrAgentRule.Error()+": "), CardSpendAsked); uerr != nil {
			return errors.Join(err, uerr)
		}
		return err
	case err != nil:
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE money_card_spends SET status = $2, detail = '', updated_at = now() WHERE id = $1 AND status IN ($3, $4)`,
		sp.ID, CardSpendIssuing, CardSpendAsked, CardSpendAwaitingApproval); err != nil {
		return fmt.Errorf("economy: card spend: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("economy: card spend: %w", err)
	}
	return nil
}

// issueOneTimeCard issues sp's one-time card: a batch of one card, locked to sp's merchant and capped at sp's amount,
// approved by the rules or the owner's approval that let sp through. Its cap is held as any card batch's is, and it
// closes, its cap given back, if unused for a day (RunCardExpiries). Asked again, the same batch is answered.
func (s *DualTokenStore) issueOneTimeCard(ctx context.Context, sp CardSpend) error {
	b, err := s.RequestCardBatch(ctx, sp.WorkspaceID, CardBatch{AgentID: sp.AgentID, IdempotencyKey: "card_spend:" + sp.ID, Purpose: "Pay " + sp.Merchant,
		Currency: sp.Currency, Funding: sp.Funding, Cards: []MoneyCard{{Merchant: sp.Merchant, CapMinor: sp.AmountMinor}}})
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_card_spends SET batch_id = $2, updated_at = now() WHERE id = $1`, sp.ID, b.ID); err != nil {
		return fmt.Errorf("economy: card spend: %w", err)
	}
	by := "agent rules"
	if sp.ApprovalID != "" {
		by = "approval " + sp.ApprovalID
	}
	if b, err = s.ApproveCardBatch(ctx, sp.WorkspaceID, b.ID, by); err != nil {
		return err
	}
	status, detail := "", ""
	switch c := b.Cards[0]; c.Status {
	case MoneyCardIssuing:
		return nil // the issuer did not answer: asked again, it is asked again under the card's own id
	case MoneyCardFailed:
		status, detail = CardSpendFailed, c.Detail
	default:
		status = CardSpendActive
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_card_spends SET status = $2, detail = $3, updated_at = now() WHERE id = $1 AND status = $4`,
		sp.ID, status, detail, CardSpendIssuing); err != nil {
		return fmt.Errorf("economy: card spend: %w", err)
	}
	return nil
}

// moveStandingLimit moves sp's standing card's limit by delta — up by sp's amount as it is approved, down by it as it
// expires — at the issuer and in Lens, and sp from one state to the next, once: one move per card at a time, under the
// card's lock, and only while sp is still in from. The new limit is written before the issuer is asked, so the money
// layer's limits see it (issuingReusableCard), and taken back if the issuer refuses. A raise records the limit it set,
// so asked again after the issuer answered it is not raised twice; a lowered limit stays at least the card's limit per
// payment. A card no longer active or frozen keeps its limit, and sp moves all the same. moved is false when sp had
// already left from.
func (s *DualTokenStore) moveStandingLimit(ctx context.Context, sp CardSpend, delta int64, from, to string) (moved bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("economy: card spend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('card_limit:' || $1, 0))`, sp.StandingCardID); err != nil {
		return false, fmt.Errorf("economy: card spend: %w", err)
	}
	var status string
	var raisedTo int64
	// Read under the card's lock, which every move of sp takes; not locked as a row, so the raise below is recorded at once.
	if err := tx.QueryRow(ctx, `SELECT status, raised_to_minor FROM money_card_spends WHERE id = $1`, sp.ID).Scan(&status, &raisedTo); err != nil {
		return false, fmt.Errorf("economy: card spend: %w", err)
	}
	if status != from {
		return false, nil
	}
	card, err := s.reusableCard(ctx, sp.WorkspaceID, sp.AgentID, sp.StandingCardID, "")
	if err != nil {
		return false, err
	}
	live := card.Status == ReusableCardActive || card.Status == ReusableCardFrozen
	if delta > 0 && card.Status != ReusableCardActive {
		return false, fmt.Errorf("%w: the standing card is %s, so its limit is not raised", ErrReusableCard, card.Status)
	}
	if live {
		moved := card
		// Raised in Lens already, by this raise asked before: the issuer is asked again for the same limit.
		written := delta > 0 && raisedTo != 0 && card.DailyLimitMinor == raisedTo
		if !written {
			moved.DailyLimitMinor = max(card.DailyLimitMinor+delta, card.PerPaymentLimitMinor)
			if delta > 0 {
				if _, err := s.pool.Exec(ctx, `UPDATE money_card_spends SET raised_to_minor = $2 WHERE id = $1`, sp.ID, moved.DailyLimitMinor); err != nil {
					return false, fmt.Errorf("economy: card spend: %w", err)
				}
			}
			if err := s.writeReusableCardControls(ctx, moved, &card); err != nil {
				return false, err
			}
		}
		if err := s.lockReusableCardCap(ctx, moved); err != nil {
			if written {
				return false, err
			}
			if werr := s.writeReusableCardControls(context.WithoutCancel(ctx), card, &moved); werr != nil {
				return false, errors.Join(err, werr)
			}
			return false, fmt.Errorf("economy: the issuer kept the standing card's limit at %s: %w", minorString(card.DailyLimitMinor, card.Currency), err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE money_card_spends SET status = $2, updated_at = now(),
		expires_at = CASE WHEN $2 = $3 THEN now() + $4 * interval '1 second' ELSE expires_at END WHERE id = $1`,
		sp.ID, to, CardSpendActive, CardSpendLife.Seconds()); err != nil {
		return false, fmt.Errorf("economy: card spend: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("economy: card spend: %w", err)
	}
	return true, nil
}

// RunCardSpendExpiries lowers back the limit of each standing card whose raise is past its time at now, and marks the
// raise expired. It answers how many it lowered.
func (s *DualTokenStore) RunCardSpendExpiries(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardSpendCols+` FROM money_card_spends WHERE status = $1 AND standing_card_id <> '' AND expires_at <= $2
		ORDER BY expires_at LIMIT 200`, CardSpendActive, now)
	if err != nil {
		return 0, fmt.Errorf("economy: card spend expiries: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardSpend, error) {
		var sp CardSpend
		return sp, r.Scan(sp.fields()...)
	})
	if err != nil {
		return 0, fmt.Errorf("economy: card spend expiries: %w", err)
	}
	n := 0
	for _, sp := range due {
		moved, err := s.moveStandingLimit(ctx, sp, -sp.AmountMinor, CardSpendActive, CardSpendExpired)
		if err != nil {
			return n, err
		}
		if moved {
			n++
		}
	}
	return n, nil
}

const cardSpendCols = `id, workspace_id, agent_id, idempotency_key, merchant, amount_minor, currency, funding, standing_card_id, status,
	approval_id, detail, expires_at, created_at, batch_id, raised_to_minor`

func (sp *CardSpend) fields() []any {
	return []any{&sp.ID, &sp.WorkspaceID, &sp.AgentID, &sp.IdempotencyKey, &sp.Merchant, &sp.AmountMinor, &sp.Currency, &sp.Funding,
		&sp.StandingCardID, &sp.Status, &sp.ApprovalID, &sp.Detail, &sp.ExpiresAt, &sp.CreatedAt, &sp.batchID, &sp.raisedToMinor}
}

// withCard reads sp's one-time card into it, once its batch is filed.
func (s *DualTokenStore) withCard(ctx context.Context, sp CardSpend) (CardSpend, error) {
	if sp.batchID == "" {
		return sp, nil
	}
	b, err := s.cardBatch(ctx, sp.WorkspaceID, "", sp.batchID, "")
	if err != nil {
		return sp, err
	}
	if len(b.Cards) > 0 && b.Cards[0].Status != MoneyCardIssuing {
		sp.Card = &b.Cards[0]
	}
	return sp, nil
}

// cardSpend reads workspaceID's card spend id, or the one under key — with agentID set, only that agent's.
func (s *DualTokenStore) cardSpend(ctx context.Context, workspaceID, agentID, id, key string) (CardSpend, error) {
	var sp CardSpend
	err := s.pool.QueryRow(ctx, `SELECT `+cardSpendCols+` FROM money_card_spends WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
		AND (($3 <> '' AND id = $3) OR ($3 = '' AND idempotency_key = $4))`, workspaceID, agentID, id, key).Scan(sp.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return CardSpend{}, ErrCardSpendNotFound
	}
	if err != nil {
		return CardSpend{}, fmt.Errorf("economy: card spend: %w", err)
	}
	return s.withCard(ctx, sp)
}

// CardSpend is workspaceID's card spend id — with agentID set, only that agent's.
func (s *DualTokenStore) CardSpend(ctx context.Context, workspaceID, agentID, id string) (CardSpend, error) {
	return s.cardSpend(ctx, workspaceID, agentID, id, "")
}

// CardSpends is workspaceID's card spends, newest first — with agentID set, only that agent's.
func (s *DualTokenStore) CardSpends(ctx context.Context, workspaceID, agentID string) ([]CardSpend, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardSpendCols+` FROM money_card_spends WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: card spends: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardSpend, error) {
		var sp CardSpend
		return sp, r.Scan(sp.fields()...)
	})
	if err != nil {
		return nil, fmt.Errorf("economy: card spends: %w", err)
	}
	for i := range list {
		if list[i], err = s.withCard(ctx, list[i]); err != nil {
			return nil, err
		}
	}
	return list, nil
}
