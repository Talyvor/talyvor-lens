package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
)

// money_conversions.go — B56.23: AN AGENT CONVERTS BETWEEN ITS CURRENCY ACCOUNTS AT ITS PLAN'S FX MARGIN.
//
// A conversion is quoted first (QuoteConversion): selling an amount from one of the agent's accounts in pounds, euros
// or dollars for another of its currencies, at the conversion partner's reference rate less the plan's FX margin —
// LENS_FX_MARGIN_BPS by plan, or an Enterprise contract's own (billing.FXMarginBPS, wired in cmd/lens). The margin is
// taken in the currency sold, rounded up to its minor unit as the platform fee is, and the partner converts the rest.
// The quote is kept, and converts once (Convert), within its life, under one idempotency key: the same key again
// answers the conversion it made.
//
// A conversion is judged by the agent's rules as a payment to itself, at what the amount sold is worth in LXC (as a
// payment out is valued). Both legs are one money entry under fx, class RED, so it moves test money only until fx is
// cleared: in the currency sold the amount leaves the agent's account, the margin goes to Talyvor's revenue and the
// rest to the partner's conversion account; in the currency bought, what it buys comes from the partner's conversion
// account into the agent's. Each currency sums to zero, and both legs are lines on the agent's account statements.

// ErrConversion: a conversion asked for between currencies, of an amount or with a funding it cannot have, or with
// no idempotency key.
var ErrConversion = errors.New("economy: invalid conversion")

// The states of a conversion.
const (
	ConversionQuoted    = "quoted"
	ConversionConverted = "converted"
)

// Conversion is a quote to convert between two of an agent's currency accounts, and the conversion made at it.
type Conversion struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	AgentID        string     `json:"agent_id"`
	FromAccountID  string     `json:"from_account_id"`
	ToAccountID    string     `json:"to_account_id"`
	SellMinor      int64      `json:"sell_minor"` // what leaves FromAccountID, the margin included
	SellCurrency   string     `json:"sell_currency"`
	MarginBPS      int64      `json:"margin_bps"`
	MarginMinor    int64      `json:"margin_minor"` // Talyvor's, in SellCurrency
	BuyMinor       int64      `json:"buy_minor"`    // what reaches ToAccountID
	BuyCurrency    string     `json:"buy_currency"`
	ReferenceRate  string     `json:"reference_rate"` // the partner's: BuyCurrency per SellCurrency, in whole units
	Rate           string     `json:"rate"`           // what the agent gets, the margin taken: BuyMinor for SellMinor
	Funding        string     `json:"funding"`
	Partner        string     `json:"partner"`
	Status         string     `json:"status"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	EntryID        string     `json:"entry_id,omitempty"`   // the money entry that moved both legs
	ValueULXC      int64      `json:"value_ulxc,omitempty"` // what the agent's rules counted it as
	ExpiresAt      time.Time  `json:"expires_at"`
	ConvertedAt    *time.Time `json:"converted_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`

	partnerRef string // the partner's reference for the quote
}

const conversionCols = `id, workspace_id, agent_id, from_account_id, to_account_id, sell_minor, sell_currency, margin_bps, margin_minor,
	buy_minor, buy_currency, reference_rate, rate, funding, partner, partner_ref, status, COALESCE(idempotency_key, ''), entry_id,
	value_ulxc, expires_at, converted_at, created_at`

func scanConversion(row pgx.Row) (Conversion, error) {
	var c Conversion
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.AgentID, &c.FromAccountID, &c.ToAccountID, &c.SellMinor, &c.SellCurrency, &c.MarginBPS,
		&c.MarginMinor, &c.BuyMinor, &c.BuyCurrency, &c.ReferenceRate, &c.Rate, &c.Funding, &c.Partner, &c.partnerRef, &c.Status,
		&c.IdempotencyKey, &c.EntryID, &c.ValueULXC, &c.ExpiresAt, &c.ConvertedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrQuote
	}
	return c, err
}

// SetFXMargin is what answers a workspace's FX margin in basis points (B56.23): billing.FXMarginBPS, wired in
// cmd/lens. Until it is set, no conversion is quoted.
func (s *DualTokenStore) SetFXMargin(r PlatformFeeResolver) { s.fxMargin = r }

// QuoteConversion prices selling in.SellMinor of in.SellCurrency from agentID's account in it for in.BuyCurrency,
// into its account in that: the reference rate, the plan's margin and what it buys. The quote is kept, and converted
// at with Convert.
func (s *DualTokenStore) QuoteConversion(ctx context.Context, workspaceID, agentID string, in Conversion) (Conversion, error) {
	in.SellCurrency, in.BuyCurrency = strings.ToUpper(strings.TrimSpace(in.SellCurrency)), strings.ToUpper(strings.TrimSpace(in.BuyCurrency))
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	held := func(c string) bool { return c == CurrencyGBP || c == CurrencyEUR || c == CurrencyUSD }
	switch {
	case !held(in.SellCurrency) || !held(in.BuyCurrency) || in.SellCurrency == in.BuyCurrency:
		return Conversion{}, fmt.Errorf("%w: a conversion sells one of GBP, EUR or USD for another, not %q for %q", ErrConversion, in.SellCurrency, in.BuyCurrency)
	case in.SellMinor <= 0:
		return Conversion{}, fmt.Errorf("%w: sell_minor is a positive number of pence or cents", ErrConversion)
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return Conversion{}, fmt.Errorf("%w: funding is test or live, not %q", ErrConversion, in.Funding)
	case s.fxMargin == nil:
		return Conversion{}, fmt.Errorf("%w: no FX margin is set, so nothing is converted", ErrConversion)
	case s.accountPartners == nil:
		return Conversion{}, fmt.Errorf("%w: no conversion partner is set", ErrAccountNotOpened)
	}
	from, err := s.agentCurrencyAccount(ctx, workspaceID, agentID, in.SellCurrency)
	if err != nil {
		return Conversion{}, err
	}
	to, err := s.agentCurrencyAccount(ctx, workspaceID, agentID, in.BuyCurrency)
	if err != nil {
		return Conversion{}, err
	}
	bps, err := s.fxMargin(ctx, s.pool, workspaceID)
	switch {
	case err != nil:
		return Conversion{}, fmt.Errorf("economy: the FX margin of %s: %w", workspaceID, err)
	case bps < 0 || bps > feeDenominator:
		return Conversion{}, fmt.Errorf("economy: the FX margin of %s is %d basis points, outside 0 to %d", workspaceID, bps, feeDenominator)
	}
	c := Conversion{ID: "mfx_" + uuid.NewString(), WorkspaceID: workspaceID, AgentID: agentID, FromAccountID: from, ToAccountID: to,
		SellMinor: in.SellMinor, SellCurrency: in.SellCurrency, MarginBPS: bps, MarginMinor: PlatformFee(in.SellMinor, bps),
		BuyCurrency: in.BuyCurrency, Funding: in.Funding, Status: ConversionQuoted}
	if c.MarginMinor >= c.SellMinor {
		return Conversion{}, fmt.Errorf("%w: %s leaves nothing to convert after the margin", ErrConversion, Money(c.SellMinor, c.SellCurrency))
	}
	conv, err := s.accountPartners.FX(ctx, CapabilityFX)
	if err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion quote: %w", err)
	}
	c.Partner = conv.Name()
	sell := partners.Money{Minor: c.SellMinor - c.MarginMinor, Currency: c.SellCurrency}
	pq, err := conv.Quote(partners.WithWorkspace(ctx, workspaceID), partners.FXQuoteRequest{ID: c.ID, Sell: sell, Buy: c.BuyCurrency})
	switch {
	case errors.Is(err, partners.ErrInvalid):
		return Conversion{}, fmt.Errorf("%w: %v", ErrConversion, err)
	case err != nil:
		return Conversion{}, fmt.Errorf("economy: conversion quote: %w", err)
	case pq.Sell != sell || pq.Buy.Currency != c.BuyCurrency || pq.Buy.Minor <= 0:
		return Conversion{}, fmt.Errorf("economy: conversion quote: the partner priced %s as selling %s for %s", sell, pq.Sell, pq.Buy)
	}
	// GBP, EUR and USD all count hundredths, so minor units divide as whole units do.
	c.BuyMinor, c.ReferenceRate, c.partnerRef, c.ExpiresAt = pq.Buy.Minor, pq.Rate, pq.Ref, pq.ExpiresAt
	c.Rate = big.NewRat(c.BuyMinor, c.SellMinor).FloatString(8)
	c, err = scanConversion(s.pool.QueryRow(ctx, `INSERT INTO money_conversions (id, workspace_id, agent_id, from_account_id, to_account_id,
		sell_minor, sell_currency, margin_bps, margin_minor, buy_minor, buy_currency, reference_rate, rate, funding, partner, partner_ref, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17) RETURNING `+conversionCols,
		c.ID, workspaceID, agentID, c.FromAccountID, c.ToAccountID, c.SellMinor, c.SellCurrency, c.MarginBPS, c.MarginMinor, c.BuyMinor,
		c.BuyCurrency, c.ReferenceRate, c.Rate, c.Funding, c.Partner, c.partnerRef, c.ExpiresAt))
	if err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion quote: %w", err)
	}
	return c, nil
}

// agentCurrencyAccount is agentID's open account in currency.
func (s *DualTokenStore) agentCurrencyAccount(ctx context.Context, workspaceID, agentID, currency string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2 AND purpose = 'agent'
		AND currency = $3 AND status = 'open' ORDER BY created_at DESC LIMIT 1`, workspaceID, agentID, currency).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: the agent has no open %s account to convert with", ErrMoneyAccountNotFound, currency)
	}
	if err != nil {
		return "", fmt.Errorf("economy: conversion: %w", err)
	}
	return id, nil
}

// Convert converts at agentID's quote quoteID, within its life, under key. The same key again answers the conversion
// it made; another conversion under it is ErrIdempotencyKeyReused; a quote converted already, or expired, is ErrQuote.
func (s *DualTokenStore) Convert(ctx context.Context, workspaceID, agentID, quoteID, key string) (Conversion, error) {
	if key = strings.TrimSpace(key); key == "" || len(key) > 200 {
		return Conversion{}, fmt.Errorf("%w: a conversion carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 200 characters", ErrConversion)
	}
	prior, err := scanConversion(s.pool.QueryRow(ctx, `SELECT `+conversionCols+` FROM money_conversions
		WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, key))
	switch {
	case err == nil && prior.ID == quoteID && prior.AgentID == agentID:
		return prior, nil
	case err == nil:
		return Conversion{}, ErrIdempotencyKeyReused
	case !errors.Is(err, ErrQuote):
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	c, err := scanConversion(s.pool.QueryRow(ctx, `SELECT `+conversionCols+` FROM money_conversions
		WHERE id = $1 AND workspace_id = $2 AND agent_id = $3`, quoteID, workspaceID, agentID))
	if err != nil {
		return Conversion{}, err
	}
	if err := c.usable(); err != nil {
		return Conversion{}, err
	}
	if s.accountPartners == nil {
		return Conversion{}, fmt.Errorf("%w: no conversion partner is set", ErrAccountNotOpened)
	}
	conv, err := s.accountPartners.FX(ctx, CapabilityFX)
	if err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	value, err := s.paymentValue(ctx, OutsidePayment{WorkspaceID: workspaceID, IdempotencyKey: "fx:" + c.ID, AmountMinor: c.SellMinor, Currency: c.SellCurrency})
	if err != nil {
		return Conversion{}, err
	}
	// An approval is for the conversion, not the quote: asked again after it, a new quote finds it.
	fp := sha256.Sum256([]byte("fx\x00" + agentID + "\x00" + c.FromAccountID + "\x00" + c.ToAccountID + "\x00" +
		strconv.FormatInt(c.SellMinor, 10) + "\x00" + c.Funding))
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]),
		Payee: Payee{Kind: "agent", ID: agentID, Name: "its own " + c.BuyCurrency + " account"},
		Memo:  Money(c.SellMinor, c.SellCurrency) + " converted to " + c.BuyCurrency + " (" + c.Funding + " money)"})

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return Conversion{}, err
	}
	// Read again under the agent's lock: the same key sent twice at once converts once, and the second answers the first.
	if c, err = scanConversion(tx.QueryRow(ctx, `SELECT `+conversionCols+` FROM money_conversions WHERE id = $1 FOR UPDATE`, c.ID)); err != nil {
		return Conversion{}, err
	}
	if c.Status == ConversionConverted && c.IdempotencyKey == key {
		return c, nil
	}
	if err := c.usable(); err != nil {
		return Conversion{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{c.FromAccountID, c.ToAccountID}); err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	var balance int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		c.FromAccountID, c.Funding).Scan(&balance); err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	if balance < c.SellMinor {
		return Conversion{}, fmt.Errorf("%w: the account holds %s of %s money, and this conversion sells %s", ErrAgentFunds,
			Money(balance, c.SellCurrency), c.Funding, Money(c.SellMinor, c.SellCurrency))
	}
	if err := enforceAgentRules(ctx, tx, workspaceID, agentID, value, c.ID); err != nil {
		return Conversion{}, s.refusedMovement(ctx, tx, err)
	}
	revenue, err := routeAccount(ctx, tx, workspaceID, c.SellCurrency, MoneyRevenue, "", "Talyvor fees")
	if err != nil {
		return Conversion{}, err
	}
	sold, err := routeAccount(ctx, tx, workspaceID, c.SellCurrency, MoneyPartner, "fx:conversion", "Conversions")
	if err != nil {
		return Conversion{}, err
	}
	bought, err := routeAccount(ctx, tx, workspaceID, c.BuyCurrency, MoneyPartner, "fx:conversion", "Conversions")
	if err != nil {
		return Conversion{}, err
	}
	// Posted before the partner is asked: a live conversion fx is not cleared for, or one the ledger refuses, never reaches it.
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityFX, Kind: "fx_conversion",
		IdempotencyKey: "fx_convert:" + c.ID, Funding: c.Funding, Counterparty: c.Partner, PartnerRef: c.partnerRef,
		Memo: Money(c.SellMinor, c.SellCurrency) + " to " + Money(c.BuyMinor, c.BuyCurrency) + " at " + c.Rate, Postings: []MoneyPosting{
			{AccountID: c.FromAccountID, AmountMinor: -c.SellMinor},
			{AccountID: revenue, AmountMinor: c.MarginMinor},
			{AccountID: sold, AmountMinor: c.SellMinor - c.MarginMinor},
			{AccountID: bought, AmountMinor: -c.BuyMinor, Currency: c.BuyCurrency},
			{AccountID: c.ToAccountID, AmountMinor: c.BuyMinor, Currency: c.BuyCurrency}}}, s.screener)
	if err != nil {
		return Conversion{}, err
	}
	// shortcut: the partner is asked inside the transaction, so nothing is posted unless it converted; upgrade to a
	// hold, then a completion read from the partner (as a payment out does), when a real partner answers pending.
	res, err := conv.Execute(partners.WithWorkspace(ctx, workspaceID), partners.FXExecution{ID: c.ID, QuoteRef: c.partnerRef})
	switch {
	case err != nil:
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	case res.Status == partners.StatusPending:
		return Conversion{}, fmt.Errorf("economy: conversion: the partner has not converted %s yet. Ask again with the same key", c.ID)
	case res.Status != partners.StatusCompleted:
		return Conversion{}, fmt.Errorf("%w: the partner did not convert it (%s): %s", ErrConversion, res.Status, res.Detail)
	}
	c, err = scanConversion(tx.QueryRow(ctx, `UPDATE money_conversions SET status = $2, idempotency_key = $3, entry_id = $4, value_ulxc = $5,
		converted_at = now() WHERE id = $1 RETURNING `+conversionCols, c.ID, ConversionConverted, key, e.ID, value))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the key, used at the same moment for another quote
			return Conversion{}, ErrIdempotencyKeyReused
		}
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	s.monitorMoved(ctx, e)
	return c, nil
}

// usable refuses converting at c when it has converted already, or has expired.
func (c Conversion) usable() error {
	switch {
	case c.Status == ConversionConverted:
		return fmt.Errorf("%w: quote %s has converted already, under another key", ErrQuote, c.ID)
	case !c.ExpiresAt.After(time.Now()):
		return fmt.Errorf("%w: quote %s expired at %s. Ask for a new one", ErrQuote, c.ID, c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}
