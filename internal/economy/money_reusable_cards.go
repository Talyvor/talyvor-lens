package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/money"
)

// money_reusable_cards.go — B53.2: THE OWNER'S OWN CARD, AND AN AGENT'S: REUSABLE, WITH LIMITS AND BLOCKED CATEGORIES.
//
// Beside an agent's single-use cards (B46.10), the owner or an admin issues a reusable card on the owner's own wallet
// (the company's account in a currency) or on one of their agents' wallets (B48.1). It pays any merchant, as often as
// its controls let it: a limit per payment and a limit a day (the UTC day), both set when it is issued; the merchant
// categories it pays only (allowed MCCs) or never (blocked MCCs), with gambling blocked in one tap; and frozen, which
// declines everything until it is unfrozen (B17.111's behaviour). The issuer asks Lens as each charge is made
// (AuthorizeCardCharge), and an approved charge moves what it cost from the card's account to the partner — nothing is
// held, so the account's balance is checked as the charge is made. The routes stay unmounted until a
// live issuer serves cards (cmd/lens reusableCardsOn).

var (
	// ErrReusableCard: a reusable card asked for, or changed, with limits, categories or a key that do not hold.
	ErrReusableCard = errors.New("economy: invalid reusable card")
	// ErrReusableCardNotFound: no such reusable card in this workspace, or not the agent's.
	ErrReusableCardNotFound = errors.New("economy: no such reusable card")
)

// The states of a reusable card.
const (
	ReusableCardIssuing = "issuing" // the issuer not yet answered
	ReusableCardActive  = "active"
	ReusableCardFrozen  = "frozen" // declines every charge until unfrozen
	ReusableCardFailed  = "failed" // the issuer refused it
)

// GamblingMCCs is the one-tap "block gambling" set: government-owned lotteries (7800, and 9406 outside the US),
// licensed online casinos (7801), licensed horse and dog racing (7802), and betting and casino gambling (7995).
var GamblingMCCs = []string{"7800", "7801", "7802", "7995", "9406"}

// ReusableCard is the owner's own card, or an agent's, and its controls.
type ReusableCard struct {
	ID                   string    `json:"id"`
	WorkspaceID          string    `json:"workspace_id"`
	AgentID              string    `json:"agent_id,omitempty"` // "" for the owner's own card
	IdempotencyKey       string    `json:"idempotency_key"`
	Currency             string    `json:"currency"`
	Funding              string    `json:"funding"`
	Status               string    `json:"status"`
	PerPaymentLimitMinor int64     `json:"per_payment_limit_minor"`
	DailyLimitMinor      int64     `json:"daily_limit_minor"`
	AllowedMCCs          []string  `json:"allowed_mccs"` // empty: every category not blocked
	BlockedMCCs          []string  `json:"blocked_mccs"`
	BlocksGambling       bool      `json:"blocks_gambling"` // asked for: blocks GamblingMCCs; read back: they are all blocked
	Partner              string    `json:"partner,omitempty"`
	Last4                string    `json:"last4,omitempty"`
	FormFactor           string    `json:"form_factor"`
	Detail               string    `json:"detail,omitempty"`
	CreatedAt            time.Time `json:"created_at"`

	accountID, walletRef, partnerRef string
}

// CardControls is what the owner changes on a reusable card; a nil field stays as it is. BlocksGambling true adds
// GamblingMCCs to the blocked categories, false takes them off.
type CardControls struct {
	Frozen               *bool     `json:"frozen,omitempty"`
	PerPaymentLimitMinor *int64    `json:"per_payment_limit_minor,omitempty"`
	DailyLimitMinor      *int64    `json:"daily_limit_minor,omitempty"`
	AllowedMCCs          *[]string `json:"allowed_mccs,omitempty"`
	BlockedMCCs          *[]string `json:"blocked_mccs,omitempty"`
	BlocksGambling       *bool     `json:"blocks_gambling,omitempty"`
}

// mccSet is codes trimmed, checked as four digits, without repeats, in order.
func mccSet(codes []string) ([]string, error) {
	out := []string{}
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if len(c) != 4 || strings.Trim(c, "0123456789") != "" {
			return nil, fmt.Errorf("%w: a merchant category is a four-digit code, not %q", ErrReusableCard, c)
		}
		out = append(out, c)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// controls checks c's limits and categories, sets its gambling block, and puts its categories in order.
func (c *ReusableCard) controls() error {
	if c.BlocksGambling {
		c.BlockedMCCs = append(c.BlockedMCCs, GamblingMCCs...)
	}
	var err error
	if c.AllowedMCCs, err = mccSet(c.AllowedMCCs); err != nil {
		return err
	}
	if c.BlockedMCCs, err = mccSet(c.BlockedMCCs); err != nil {
		return err
	}
	switch {
	case c.PerPaymentLimitMinor <= 0 || c.PerPaymentLimitMinor > 1<<40:
		return fmt.Errorf("%w: per_payment_limit_minor is a positive number of pence or cents", ErrReusableCard)
	case c.DailyLimitMinor < c.PerPaymentLimitMinor || c.DailyLimitMinor > 1<<40:
		return fmt.Errorf("%w: daily_limit_minor is at least the limit per payment", ErrReusableCard)
	}
	c.BlocksGambling = blocksAll(c.BlockedMCCs, GamblingMCCs)
	return nil
}

func blocksAll(blocked, codes []string) bool {
	for _, c := range codes {
		if !slices.Contains(blocked, c) {
			return false
		}
	}
	return true
}

func (c ReusableCard) same(in ReusableCard) bool {
	return c.AgentID == in.AgentID && c.Currency == in.Currency && c.Funding == in.Funding &&
		c.PerPaymentLimitMinor == in.PerPaymentLimitMinor && c.DailyLimitMinor == in.DailyLimitMinor
}

// IssueReusableCard issues in.AgentID's reusable card — the owner's own with no agent — in workspaceID under
// in.IdempotencyKey, with its limits and categories. The same key again answers the card it first issued, asking the
// issuer again if it had not answered; with other limits, ErrIdempotencyKeyReused.
func (s *DualTokenStore) IssueReusableCard(ctx context.Context, workspaceID string, in ReusableCard) (ReusableCard, error) {
	if s.moneyLayer == nil {
		return ReusableCard{}, ErrAgentWalletsOff
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	switch {
	case in.Currency != CurrencyGBP && in.Currency != CurrencyEUR && in.Currency != CurrencyUSD:
		return ReusableCard{}, fmt.Errorf("%w: cards are in GBP, EUR or USD, not %q", ErrReusableCard, in.Currency)
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return ReusableCard{}, fmt.Errorf("%w: funding is test or live, not %q", ErrReusableCard, in.Funding)
	case in.IdempotencyKey == "" || len(in.IdempotencyKey) > 200:
		return ReusableCard{}, fmt.Errorf("%w: a card carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 200 characters", ErrReusableCard)
	}
	if err := in.controls(); err != nil {
		return ReusableCard{}, err
	}
	c, err := s.reusableCard(ctx, workspaceID, "", "", in.IdempotencyKey)
	switch {
	case err == nil && !c.same(in):
		return ReusableCard{}, ErrIdempotencyKeyReused
	case err == nil && c.Status != ReusableCardIssuing:
		return c, nil
	case err == nil:
	case !errors.Is(err, ErrReusableCardNotFound):
		return ReusableCard{}, err
	default:
		if c, err = s.fileReusableCard(ctx, workspaceID, in); err != nil {
			return ReusableCard{}, err
		}
	}
	ctx = context.WithoutCancel(ctx) // a caller that goes away does not leave the card half issued
	rt := cardRoute(workspaceID, c.AgentID)
	partner, err := s.moneyLayer.Partner(ctx, money.CapCards, rt)
	if err != nil {
		return c, err
	}
	cards, err := s.moneyLayer.Cards(ctx, rt)
	if err != nil {
		return c, err
	}
	card, err := cards.Issue(ctx, money.IssueCard{Key: c.ID, Wallet: c.walletRef, Cap: money.Amount{Minor: c.DailyLimitMinor, Asset: c.Currency}})
	switch {
	case errors.Is(err, money.ErrPartnerDown):
		return c, err // still issuing: asking again under the same key asks the issuer again
	case err != nil:
		_, err = s.pool.Exec(ctx, `UPDATE money_reusable_cards SET status = $2, detail = $3, updated_at = now() WHERE id = $1 AND status = $4`,
			c.ID, ReusableCardFailed, err.Error(), ReusableCardIssuing)
	default:
		_, err = s.pool.Exec(ctx, `UPDATE money_reusable_cards SET status = $2, partner = $3, partner_ref = $4, last4 = $5, form_factor = $6,
			updated_at = now() WHERE id = $1 AND status = $7`, c.ID, ReusableCardActive, partner, card.Ref, card.Last4,
			money.KnownFormFactor(card.FormFactor), ReusableCardIssuing)
	}
	if err != nil {
		return c, fmt.Errorf("economy: reusable card: %w", err)
	}
	return s.reusableCard(ctx, workspaceID, "", c.ID, "")
}

// fileReusableCard records in as issuing, on the account it spends from; live money only once agent_card is cleared.
func (s *DualTokenStore) fileReusableCard(ctx context.Context, workspaceID string, in ReusableCard) (ReusableCard, error) {
	from, err := s.payingAccount(ctx, workspaceID, OutsidePayment{AgentID: in.AgentID, Currency: in.Currency})
	if err != nil {
		return ReusableCard{}, err
	}
	if in.Funding == FundingLive {
		capability, _ := CapabilityByKey(CapabilityAgentCard)
		refusal, err := capabilityLive(ctx, s.pool, workspaceID, capability)
		if err != nil {
			return ReusableCard{}, err
		}
		if refusal != nil {
			return ReusableCard{}, refusal
		}
	}
	id := "rcard_" + uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO money_reusable_cards (id, workspace_id, agent_id, idempotency_key, currency, funding, account_id,
		wallet_ref, status, per_payment_limit_minor, daily_limit_minor, allowed_mccs, blocked_mccs)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, id, workspaceID, in.AgentID, in.IdempotencyKey, in.Currency, in.Funding,
		from.id, from.partnerRef, ReusableCardIssuing, in.PerPaymentLimitMinor, in.DailyLimitMinor, in.AllowedMCCs, in.BlockedMCCs); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the same key, asked at the same moment
			return s.reusableCard(ctx, workspaceID, "", "", in.IdempotencyKey)
		}
		return ReusableCard{}, fmt.Errorf("economy: reusable card: %w", err)
	}
	return s.reusableCard(ctx, workspaceID, "", id, "")
}

// SetReusableCardControls changes workspaceID's reusable card id as c says: frozen or not, its limits, its categories.
func (s *DualTokenStore) SetReusableCardControls(ctx context.Context, workspaceID, id string, c CardControls) (ReusableCard, error) {
	card, err := s.reusableCard(ctx, workspaceID, "", id, "")
	if err != nil {
		return card, err
	}
	if c.Frozen != nil {
		if card.Status != ReusableCardActive && card.Status != ReusableCardFrozen {
			return card, fmt.Errorf("%w: the card is %s, so it is neither frozen nor unfrozen", ErrReusableCard, card.Status)
		}
		card.Status = map[bool]string{true: ReusableCardFrozen, false: ReusableCardActive}[*c.Frozen]
	}
	if c.PerPaymentLimitMinor != nil {
		card.PerPaymentLimitMinor = *c.PerPaymentLimitMinor
	}
	if c.DailyLimitMinor != nil {
		card.DailyLimitMinor = *c.DailyLimitMinor
	}
	if c.AllowedMCCs != nil {
		card.AllowedMCCs = *c.AllowedMCCs
	}
	if c.BlockedMCCs != nil {
		card.BlockedMCCs = *c.BlockedMCCs
	}
	card.BlocksGambling = false
	if c.BlocksGambling != nil {
		card.BlockedMCCs = slices.DeleteFunc(slices.Clone(card.BlockedMCCs), func(m string) bool { return slices.Contains(GamblingMCCs, m) })
		card.BlocksGambling = *c.BlocksGambling
	}
	if err := card.controls(); err != nil {
		return card, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_reusable_cards SET status = $3, per_payment_limit_minor = $4, daily_limit_minor = $5,
		allowed_mccs = $6, blocked_mccs = $7, updated_at = now() WHERE id = $1 AND workspace_id = $2`, card.ID, workspaceID, card.Status,
		card.PerPaymentLimitMinor, card.DailyLimitMinor, card.AllowedMCCs, card.BlockedMCCs); err != nil {
		return card, fmt.Errorf("economy: reusable card: %w", err)
	}
	return s.reusableCard(ctx, workspaceID, "", id, "")
}

// authorizeReusableCharge decides a's charge if it is on a reusable card, in tx: done is false for a card that is
// not one. Approved only while the card is active, its agent not paused, in its currency, within its limit per payment
// and what is left of its daily limit, in a category it pays, and while its account holds the charge.
func (s *DualTokenStore) authorizeReusableCharge(ctx context.Context, tx pgx.Tx, partner string, a money.Authorization) (done, approved bool, reason string, err error) {
	var c ReusableCard
	err = tx.QueryRow(ctx, `SELECT `+reusableCardCols+` FROM money_reusable_cards WHERE partner = $1 AND partner_ref = $2 FOR UPDATE`, partner, a.Card).
		Scan(c.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, "", nil
	}
	if err != nil {
		return true, false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	d := a.Normal()
	var paused error
	if c.AgentID != "" {
		paused = refuseIfPaused(ctx, tx, c.AgentID)
	}
	var spent, held int64
	from, err := s.payingAccount(ctx, c.WorkspaceID, OutsidePayment{AccountID: c.accountID, Currency: c.Currency})
	if err != nil {
		return true, false, "", err
	}
	// As a payment out does: every payment out of the account waits its turn.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{from.companyID, from.id}); err != nil {
		return true, false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_minor), 0)::bigint FROM money_card_authorizations WHERE card_id = $1 AND approved
		AND created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`, c.ID).Scan(&spent); err != nil {
		return true, false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		c.accountID, c.Funding).Scan(&held); err != nil {
		return true, false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	switch {
	case c.Status == ReusableCardFrozen:
		reason = "the card is frozen"
	case c.Status != ReusableCardActive:
		reason = "the card is not active"
	case paused != nil:
		reason = paused.Error()
	case a.Amount.Asset != c.Currency:
		reason = "the card pays in " + c.Currency
	case a.Amount.Minor <= 0 || a.Amount.Minor > c.PerPaymentLimitMinor:
		reason = "the charge is more than the card's limit of " + minorString(c.PerPaymentLimitMinor, c.Currency) + " a payment"
	case slices.Contains(c.BlockedMCCs, d.MCC) && slices.Contains(GamblingMCCs, d.MCC):
		reason = "the card is blocked from gambling"
	case slices.Contains(c.BlockedMCCs, d.MCC):
		reason = "the card is blocked in merchant category " + d.MCC
	case len(c.AllowedMCCs) > 0 && !slices.Contains(c.AllowedMCCs, d.MCC):
		reason = "the card does not pay in merchant category " + d.MCC
	case spent+a.Amount.Minor > c.DailyLimitMinor:
		reason = "the charge would pass the card's daily limit of " + minorString(c.DailyLimitMinor, c.Currency)
	case held < a.Amount.Minor:
		reason = "the account holds too little for the charge"
	default:
		approved = true
	}
	if _, err := tx.Exec(ctx, `INSERT INTO money_card_authorizations (partner, id, card_id, workspace_id, merchant, amount_minor, currency, approved, reason,
		mcc, merchant_country, channel, wallet) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, partner, a.ID, c.ID, c.WorkspaceID,
		a.Merchant, a.Amount.Minor, a.Amount.Asset, approved, reason, d.MCC, d.Country, d.Channel, d.Wallet); err != nil {
		return true, false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	if approved {
		if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: c.WorkspaceID, Capability: CapabilityPaymentsOut, Kind: "card_charge",
			IdempotencyKey: "card_charge:" + partner + ":" + a.ID, Funding: c.Funding, Memo: a.Merchant, Counterparty: a.Merchant, PartnerRef: a.ID,
			Card: &d, Postings: []MoneyPosting{{AccountID: c.accountID, AmountMinor: -a.Amount.Minor}, {AccountID: from.partnerID, AmountMinor: a.Amount.Minor}}},
			s.screener); err != nil {
			return true, false, "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return true, false, "", fmt.Errorf("economy: card charge: %w", err)
	}
	return true, approved, reason, nil
}

// issuingReusableCard is whether op is the money layer issuing a reusable card the owner asked for: the card issuing
// under op's key, for that agent (none for the owner's own), account and daily limit, at no one merchant.
func (s *DualTokenStore) issuingReusableCard(ctx context.Context, op money.Op) (bool, error) {
	if op.Capability != money.CapCards || op.Method != "issue" || !strings.HasPrefix(op.Key, "rcard_") || op.Payee != "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_reusable_cards WHERE id = $1 AND workspace_id = $2 AND agent_id = $3
		AND status = $4 AND daily_limit_minor = $5 AND currency = $6 AND wallet_ref = $7)`, op.Key, op.Workspace, op.Agent,
		ReusableCardIssuing, op.Amount.Minor, op.Amount.Asset, op.Ref).Scan(&ok)
	return ok, err
}

const reusableCardCols = `id, workspace_id, agent_id, idempotency_key, currency, funding, status, per_payment_limit_minor, daily_limit_minor,
	allowed_mccs, blocked_mccs, partner, last4, form_factor, detail, created_at, account_id, wallet_ref, partner_ref`

func (c *ReusableCard) fields() []any {
	return []any{&c.ID, &c.WorkspaceID, &c.AgentID, &c.IdempotencyKey, &c.Currency, &c.Funding, &c.Status, &c.PerPaymentLimitMinor,
		&c.DailyLimitMinor, &c.AllowedMCCs, &c.BlockedMCCs, &c.Partner, &c.Last4, &c.FormFactor, &c.Detail, &c.CreatedAt, &c.accountID,
		&c.walletRef, &c.partnerRef}
}

// reusableCard reads workspaceID's reusable card id, or the one under key — with agentID set, only that agent's.
func (s *DualTokenStore) reusableCard(ctx context.Context, workspaceID, agentID, id, key string) (ReusableCard, error) {
	var c ReusableCard
	err := s.pool.QueryRow(ctx, `SELECT `+reusableCardCols+` FROM money_reusable_cards WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
		AND (($3 <> '' AND id = $3) OR ($3 = '' AND idempotency_key = $4))`, workspaceID, agentID, id, key).Scan(c.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrReusableCardNotFound
	}
	if err != nil {
		return c, fmt.Errorf("economy: reusable card: %w", err)
	}
	c.BlocksGambling = blocksAll(c.BlockedMCCs, GamblingMCCs)
	return c, nil
}

// ReusableCards is workspaceID's reusable cards — with agentID set, only that agent's — newest first, at most 100.
func (s *DualTokenStore) ReusableCards(ctx context.Context, workspaceID, agentID string) ([]ReusableCard, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reusableCardCols+` FROM money_reusable_cards WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
		ORDER BY created_at DESC, id LIMIT 100`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: reusable cards: %w", err)
	}
	defer rows.Close()
	out := []ReusableCard{}
	for rows.Next() {
		var c ReusableCard
		if err := rows.Scan(c.fields()...); err != nil {
			return nil, err
		}
		c.BlocksGambling = blocksAll(c.BlockedMCCs, GamblingMCCs)
		out = append(out, c)
	}
	return out, rows.Err()
}
