package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/passkey"
)

// card_challenges.go — B59.22: 3-D SECURE. AN ONLINE CARD CHALLENGE WAITS FOR THE OWNER.
//
// A merchant online may ask an agent's card's issuer to have the cardholder confirm a payment. The issuer sends the
// challenge (ReceiveCardChallenge); Lens files it, once under the issuer's id, as an approval of its own kind
// (ApprovalCardChallenge: the merchant, the amount and the card) and pushes it to the owner. Only the owner's signed
// answer decides it (DecideCardChallenge) — a passkey or the phone's key over the approval's challenge, whether or not
// the workspace signs its other approvals — never an unsigned tap, an approver agent or a batch. The answer goes back
// through the issuer's ThreeDSProvider. No answer by the issuer's deadline is a no (RunCardChallengeTimeouts), and an
// answer the issuer did not take is sent again there.

// ApprovalCardChallenge is the kind of approval a 3-D Secure challenge is.
const ApprovalCardChallenge = "card_challenge"

// The states of a card challenge.
const (
	CardChallengePending  = "pending"
	CardChallengeApproved = "approved"
	CardChallengeDeclined = "declined"
	CardChallengeTimedOut = "timed_out" // no answer by the issuer's deadline: declined
)

var (
	// ErrCardChallenge: a challenge Lens cannot take — on no card of Lens's, or past its deadline.
	ErrCardChallenge = errors.New("economy: invalid card challenge")
	// ErrNotCardChallenge: the approval is not a 3-D Secure challenge, so it is decided as any other.
	ErrNotCardChallenge = errors.New("economy: not a card challenge")
	// ErrCardChallengeUnsigned: a card challenge answered without the owner's signature over it.
	ErrCardChallengeUnsigned = errors.New("A card check is answered with a passkey or the phone's key, signed on this approval.") //nolint:staticcheck // ST1005: shown to a person as this sentence
)

// CardChallenge is an issuer's 3-D Secure challenge on an agent's card, and where it stands.
type CardChallenge struct {
	Partner     string     `json:"partner"`
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	AgentID     string     `json:"agent_id"`
	CardID      string     `json:"card_id"`
	Merchant    string     `json:"merchant"`
	AmountMinor int64      `json:"amount_minor"`
	Currency    string     `json:"currency"`
	ApprovalID  string     `json:"approval_id"`
	Status      string     `json:"status"`
	Answered    bool       `json:"answered"` // the issuer was told
	ExpiresAt   time.Time  `json:"expires_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

const cardChallengeCols = `partner, id, workspace_id, agent_id, card_id, merchant, amount_minor, currency, approval_id, status, answered,
	expires_at, decided_at, created_at`

func (c *CardChallenge) fields() []any {
	return []any{&c.Partner, &c.ID, &c.WorkspaceID, &c.AgentID, &c.CardID, &c.Merchant, &c.AmountMinor, &c.Currency, &c.ApprovalID,
		&c.Status, &c.Answered, &c.ExpiresAt, &c.DecidedAt, &c.CreatedAt}
}

// SetSandboxThreeDS sets the Sandbox's 3-D Secure, which sends its challenges here; SandboxCardChallenge asks it.
func (s *DualTokenStore) SetSandboxThreeDS(t *money.SandboxThreeDS) {
	t.Intake = func(ctx context.Context, partner string, c money.ThreeDSChallenge) error {
		_, err := s.ReceiveCardChallenge(ctx, partner, c)
		return err
	}
	s.sandboxThreeDS = t
}

// ReceiveCardChallenge takes partner's 3-D Secure challenge c on one of its cards that Lens issued an agent, and files
// it as an approval for the owner, due by c's deadline. Sent again under the same id, the same challenge is answered.
func (s *DualTokenStore) ReceiveCardChallenge(ctx context.Context, partner string, c money.ThreeDSChallenge) (CardChallenge, error) {
	if err := c.Valid(); err != nil {
		return CardChallenge{}, fmt.Errorf("%w: %w", ErrCardChallenge, err)
	}
	c.Merchant = strings.Join(strings.Fields(c.Merchant), " ")
	c.Amount.Asset = strings.ToUpper(c.Amount.Asset)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('card_challenge:' || $1 || ':' || $2, 0))`, partner, c.ID); err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	if ch, err := cardChallenge(ctx, tx, `partner = $1 AND id = $2`, partner, c.ID); !errors.Is(err, ErrApprovalNotFound) {
		return ch, err
	}
	if !c.ExpiresAt.After(time.Now()) {
		return CardChallenge{}, fmt.Errorf("%w: it is past its deadline", ErrCardChallenge)
	}
	ch := CardChallenge{Partner: partner, ID: c.ID, Merchant: c.Merchant, AmountMinor: c.Amount.Minor, Currency: c.Amount.Asset, ExpiresAt: c.ExpiresAt}
	err = tx.QueryRow(ctx, `SELECT id, workspace_id, agent_id FROM money_cards WHERE partner = $1 AND partner_ref = $2
		UNION ALL SELECT id, workspace_id, agent_id FROM money_reusable_cards WHERE partner = $1 AND partner_ref = $2 AND agent_id <> ''
		LIMIT 1`, partner, c.Card).Scan(&ch.CardID, &ch.WorkspaceID, &ch.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CardChallenge{}, fmt.Errorf("%w: Lens issued no agent such a card", ErrCardChallenge)
	}
	if err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	value, err := s.moneyValue(ctx, money.Op{Route: money.Route{Workspace: ch.WorkspaceID}, Amount: c.Amount, Key: "card_challenge:" + c.ID})
	if err != nil {
		return CardChallenge{}, err
	}
	fp := sha256.Sum256([]byte("card_challenge\x00" + partner + "\x00" + c.ID))
	ch.ApprovalID = "apr_" + uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, kind, payee_kind, payee_id, payee_name, memo,
			expires_at, paid_from_kind, paid_from_account_id, paid_from_currency, paid_from_amount_minor)
		VALUES ($1, $2, $3, $4, $5, $6, 'merchant', $7, $8, $9, $10, 'card', $11, $12, $13)`,
		ch.ApprovalID, ch.WorkspaceID, ch.AgentID, hex.EncodeToString(fp[:]), value, ApprovalCardChallenge, "merchant:"+ch.Merchant, ch.Merchant,
		"Confirm "+minorString(ch.AmountMinor, ch.Currency)+" by card at "+ch.Merchant, ch.ExpiresAt, ch.CardID, ch.Currency, ch.AmountMinor); err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO money_card_challenges (partner, id, workspace_id, agent_id, card_id, merchant, amount_minor, currency,
		approval_id, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+cardChallengeCols, partner, ch.ID, ch.WorkspaceID, ch.AgentID,
		ch.CardID, ch.Merchant, ch.AmountMinor, ch.Currency, ch.ApprovalID, ch.ExpiresAt).Scan(ch.fields()...); err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	s.notifyApproval(ch.WorkspaceID, ch.ApprovalID)
	return ch, nil
}

// SandboxCardChallenge has the Sandbox's 3-D Secure challenge a payment of amount at merchant on workspaceID's card
// cardID, as its issuer would, and answers the challenge as filed. Only on a card the Sandbox issued.
func (s *DualTokenStore) SandboxCardChallenge(ctx context.Context, workspaceID, cardID, merchant string, amount money.Amount) (CardChallenge, error) {
	if s.sandboxThreeDS == nil {
		return CardChallenge{}, fmt.Errorf("%w: the Sandbox's 3-D Secure is off on this server", ErrCardChallenge)
	}
	var partner, ref string
	err := s.pool.QueryRow(ctx, `SELECT partner, partner_ref FROM money_cards WHERE workspace_id = $1 AND id = $2
		UNION ALL SELECT partner, partner_ref FROM money_reusable_cards WHERE workspace_id = $1 AND id = $2 AND agent_id <> '' LIMIT 1`,
		workspaceID, cardID).Scan(&partner, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return CardChallenge{}, fmt.Errorf("%w: no agent's card %q in this workspace", ErrCardChallenge, cardID)
	}
	if err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	if partner != s.sandboxThreeDS.Partner {
		return CardChallenge{}, fmt.Errorf("%w: the card is %s's, not the Sandbox's", ErrCardChallenge, partner)
	}
	c, err := s.sandboxThreeDS.Challenge(ctx, money.ThreeDSChallenge{ID: "3ds_" + uuid.NewString(), Card: ref, Merchant: merchant, Amount: amount})
	if err != nil {
		return CardChallenge{}, err
	}
	return cardChallenge(ctx, s.pool, `partner = $1 AND id = $2`, partner, c.ID)
}

// DecideCardChallenge decides the card challenge filed as approvalID in workspaceID with the owner's assertion a, by
// one of the workspace's passkeys or device keys over that approval's challenge (ApprovalChallenge), which it spends;
// unsigned, ErrCardChallengeUnsigned. The answer then goes to the issuer. ErrNotCardChallenge when approvalID is
// another kind; ErrApprovalNotFound when it is no longer pending.
func (s *DualTokenStore) DecideCardChallenge(ctx context.Context, workspaceID, approvalID string, approve bool, a *passkey.Assertion) (AgentApproval, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentApproval{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ch, err := cardChallenge(ctx, tx, `approval_id = $1 AND workspace_id = $2 FOR UPDATE`, approvalID, workspaceID)
	if errors.Is(err, ErrApprovalNotFound) {
		return AgentApproval{}, ErrNotCardChallenge
	}
	if err != nil {
		return AgentApproval{}, err
	}
	if a == nil {
		return AgentApproval{}, ErrCardChallengeUnsigned
	}
	if err := s.spendAssertion(ctx, tx, workspaceID, approvalPurpose(approvalID, nil), *a); err != nil {
		return AgentApproval{}, err
	}
	if ch.Status != CardChallengePending || !ch.ExpiresAt.After(time.Now()) {
		return AgentApproval{}, ErrApprovalNotFound
	}
	status, approval := CardChallengeDeclined, "denied"
	if approve {
		status, approval = CardChallengeApproved, "used" // answered yes, it has done what it was for
	}
	if _, err := tx.Exec(ctx, `UPDATE money_card_challenges SET status = $3, decided_at = now() WHERE partner = $1 AND id = $2`,
		ch.Partner, ch.ID, status); err != nil {
		return AgentApproval{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	ap, err := scanAgentApproval(tx.QueryRow(ctx, `UPDATE agent_approvals SET status = $3, decided_at = now(),
		deny_reason = CASE WHEN $3 = 'denied' THEN $4 ELSE '' END
		WHERE id = $1 AND workspace_id = $2 AND status = 'pending' AND kind = '`+ApprovalCardChallenge+`' RETURNING `+agentApprovalColumns,
		approvalID, workspaceID, approval, denyReasonFrom(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return ap, ErrApprovalNotFound
	}
	if err != nil {
		return ap, fmt.Errorf("economy: card challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ap, fmt.Errorf("economy: card challenge: %w", err)
	}
	// The owner's answer stands; one the issuer does not take now is sent again by RunCardChallengeTimeouts.
	if err := s.answerCardChallenge(context.WithoutCancel(ctx), ch.Partner, ch.ID, approve); err != nil {
		slog.Warn("card challenge: the issuer did not take the answer", slog.String("challenge", ch.ID), slog.String("err", err.Error()))
	}
	return ap, nil
}

// answerCardChallenge tells partner's 3-D Secure the answer to challenge id, and keeps that it was told.
func (s *DualTokenStore) answerCardChallenge(ctx context.Context, partner, id string, approved bool) error {
	if s.moneyLayer == nil {
		return ErrAgentWalletsOff
	}
	p, err := s.moneyLayer.ThreeDS(partner)
	if err != nil {
		return err
	}
	if err := p.Answer(ctx, id, approved); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_card_challenges SET answered = true WHERE partner = $1 AND id = $2`, partner, id); err != nil {
		return fmt.Errorf("economy: card challenge: %w", err)
	}
	return nil
}

// RunCardChallengeTimeouts declines each card challenge still unanswered at its deadline, at now, its approval expired,
// and tells the issuers every answer they have not yet taken. It answers how many it declined.
func (s *DualTokenStore) RunCardChallengeTimeouts(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("economy: card challenge timeouts: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `UPDATE money_card_challenges SET status = $1, decided_at = now() WHERE status = $2 AND expires_at <= $3
		RETURNING approval_id`, CardChallengeTimedOut, CardChallengePending, now)
	if err != nil {
		return 0, fmt.Errorf("economy: card challenge timeouts: %w", err)
	}
	approvals, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("economy: card challenge timeouts: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_approvals SET status = 'expired', expires_at = LEAST(expires_at, now()) WHERE id = ANY($1) AND status = 'pending'`,
		approvals); err != nil {
		return 0, fmt.Errorf("economy: card challenge timeouts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("economy: card challenge timeouts: %w", err)
	}
	rows, err = s.pool.Query(ctx, `SELECT `+cardChallengeCols+` FROM money_card_challenges WHERE status <> $1 AND NOT answered
		ORDER BY decided_at LIMIT 200`, CardChallengePending)
	if err != nil {
		return len(approvals), fmt.Errorf("economy: card challenge answers: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardChallenge, error) {
		var c CardChallenge
		return c, r.Scan(c.fields()...)
	})
	if err != nil {
		return len(approvals), fmt.Errorf("economy: card challenge answers: %w", err)
	}
	var errs []error
	for _, c := range due { // one issuer that will not take an answer does not keep the others from theirs
		if err := s.answerCardChallenge(ctx, c.Partner, c.ID, c.Status == CardChallengeApproved); err != nil {
			errs = append(errs, fmt.Errorf("card challenge %s: %w", c.ID, err))
		}
	}
	return len(approvals), errors.Join(errs...)
}

// CardChallenges is workspaceID's card challenges, newest first.
func (s *DualTokenStore) CardChallenges(ctx context.Context, workspaceID string) ([]CardChallenge, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardChallengeCols+` FROM money_card_challenges WHERE workspace_id = $1
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: card challenges: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardChallenge, error) {
		var c CardChallenge
		return c, r.Scan(c.fields()...)
	})
	if err != nil {
		return nil, fmt.Errorf("economy: card challenges: %w", err)
	}
	return list, nil
}

// cardChallenge reads the one card challenge where holds; ErrApprovalNotFound when there is none.
func cardChallenge(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, where string, args ...any) (CardChallenge, error) {
	var c CardChallenge
	err := q.QueryRow(ctx, `SELECT `+cardChallengeCols+` FROM money_card_challenges WHERE `+where, args...).Scan(c.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return CardChallenge{}, ErrApprovalNotFound
	}
	if err != nil {
		return CardChallenge{}, fmt.Errorf("economy: card challenge: %w", err)
	}
	return c, nil
}
