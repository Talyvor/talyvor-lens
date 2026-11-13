package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
)

// money_wallets.go — B59.51: THE OWNER'S WALLETS: WITHDRAW TO YOUR OWN BANK, AND CONVERT BETWEEN THE TWO SIDES.
//
// Withdraw pays a company currency account out to a bank the owner saved as their own (a payee saved with own), in
// that account's currency. It is a payment out (money_payments.go) with the reference "Withdrawal": signed by a
// passkey or the phone's device key over exactly that withdrawal once the workspace has one, under an idempotency
// key, through the account partner, screened, held and sent like any other, so payments_out (RED) keeps it to test
// money until it is cleared. It carries no fee.
//
// Convert sells one of the company's accounts on one side for its account on the other: a currency (GBP, EUR or USD)
// for USDC, or USDC for a currency. Only a hybrid workspace holds both sides, so any other is refused with a
// *MoneyKindRefusal on field convert. It is quoted first at the conversion partner's rate less the plan's FX margin
// (B56.23's money_conversions, with no agent), then converted once under an idempotency key, signed over that quote
// and key. Both legs are one money entry under fx, class RED, each currency summing to zero; live USDC also waits for
// stablecoins' clearance.

var (
	// ErrWithdrawal: a withdrawal from an account that is not the company's, or to a payee that is not the owner's
	// own bank in the account's currency.
	ErrWithdrawal = errors.New("economy: invalid withdrawal")
	// ErrConversionUnsigned: the workspace signs its conversions and this one carried no signature over its challenge.
	ErrConversionUnsigned = errors.New("economy: this workspace signs its conversions with a passkey or the phone app. Sign this conversion's challenge (POST /v1/money/convert/challenge)")
)

// WithdrawalReference is the reference a withdrawal is paid with.
const WithdrawalReference = "Withdrawal"

// Withdrawal asks to pay AmountMinor out of the company account AccountID to the owner's own bank PayeeID.
type Withdrawal struct {
	AccountID      string `json:"account_id"`
	PayeeID        string `json:"payee_id"`
	AmountMinor    int64  `json:"amount_minor"`
	Funding        string `json:"funding"`
	IdempotencyKey string `json:"idempotency_key"`
}

// withdrawalPayment is in as the payment out it makes, once the account is the company's and the payee is the
// owner's own bank in its currency.
func (s *DualTokenStore) withdrawalPayment(ctx context.Context, workspaceID string, in Withdrawal) (OutsidePayment, error) {
	var currency, purpose string
	err := s.pool.QueryRow(ctx, `SELECT currency, purpose FROM money_accounts WHERE id = $1 AND workspace_id = $2 AND agent_id IS NULL`,
		in.AccountID, workspaceID).Scan(&currency, &purpose)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return OutsidePayment{}, fmt.Errorf("%w: no account %q in this workspace", ErrMoneyAccountNotFound, in.AccountID)
	case err != nil:
		return OutsidePayment{}, fmt.Errorf("economy: withdrawal: %w", err)
	case purpose != MoneyCompany:
		return OutsidePayment{}, fmt.Errorf("%w: only one of the company's currency accounts is withdrawn", ErrWithdrawal)
	}
	payee, err := s.outsidePayee(ctx, s.pool, workspaceID, in.PayeeID, "")
	switch {
	case err != nil:
		return OutsidePayment{}, err
	case !payee.Own:
		return OutsidePayment{}, fmt.Errorf("%w: %s is not saved as your own bank", ErrWithdrawal, payee.Name)
	case payeeCurrency(payee) != currency:
		return OutsidePayment{}, fmt.Errorf("%w: your bank account %s takes %s, and this account holds %s", ErrWithdrawal, payee.Name,
			payeeCurrency(payee), currency)
	}
	return OutsidePayment{AccountID: in.AccountID, PayeeID: in.PayeeID, AmountMinor: in.AmountMinor, Currency: currency,
		Funding: in.Funding, Reference: WithdrawalReference, IdempotencyKey: in.IdempotencyKey}, nil
}

// WithdrawalChallenge issues the challenge withdrawing in is signed over, and the workspace's passkeys that may sign
// it; its device keys sign it too.
func (s *DualTokenStore) WithdrawalChallenge(ctx context.Context, workspaceID string, in Withdrawal) (string, []string, error) {
	p, err := s.withdrawalPayment(ctx, workspaceID, in)
	if err != nil {
		return "", nil, err
	}
	return s.PaymentChallenge(ctx, workspaceID, p)
}

// WithdrawToOwnBank pays in out to the owner's own bank, with a, the signature over WithdrawalChallenge's challenge for it. The
// same withdrawal again under its key answers the payment it made.
func (s *DualTokenStore) WithdrawToOwnBank(ctx context.Context, workspaceID string, in Withdrawal, a *passkey.Assertion) (OutsidePayment, error) {
	p, err := s.withdrawalPayment(ctx, workspaceID, in)
	if err != nil {
		return OutsidePayment{}, err
	}
	return s.PayOutsideSigned(ctx, workspaceID, p, a)
}

// QuoteSideConversion prices selling in.SellMinor of in.SellCurrency from the company's account in it for
// in.BuyCurrency, into its account in that, one of them USDC and the other a currency. The quote is kept, and
// converted at with ConvertSides.
func (s *DualTokenStore) QuoteSideConversion(ctx context.Context, workspaceID string, in Conversion) (Conversion, error) {
	kind, err := workspaceMoneyKind(ctx, s.pool, workspaceID)
	if err != nil {
		return Conversion{}, err
	}
	if kind != MoneyKindHybrid {
		return Conversion{}, &MoneyKindRefusal{Field: "convert", MoneyKind: kind}
	}
	in.SellCurrency, in.BuyCurrency = strings.ToUpper(strings.TrimSpace(in.SellCurrency)), strings.ToUpper(strings.TrimSpace(in.BuyCurrency))
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	fiat := func(c string) bool { return c == CurrencyGBP || c == CurrencyEUR || c == CurrencyUSD }
	switch {
	case !(fiat(in.SellCurrency) && in.BuyCurrency == CurrencyUSDC) && !(in.SellCurrency == CurrencyUSDC && fiat(in.BuyCurrency)):
		return Conversion{}, fmt.Errorf("%w: a conversion between the two sides sells GBP, EUR or USD for USDC, or USDC for one of them, not %q for %q",
			ErrConversion, in.SellCurrency, in.BuyCurrency)
	case in.SellMinor <= 0:
		return Conversion{}, fmt.Errorf("%w: sell_minor is a positive number of the currency's smallest unit", ErrConversion)
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return Conversion{}, fmt.Errorf("%w: funding is test or live, not %q", ErrConversion, in.Funding)
	case s.fxMargin == nil:
		return Conversion{}, fmt.Errorf("%w: no FX margin is set, so nothing is converted", ErrConversion)
	case s.accountPartners == nil:
		return Conversion{}, fmt.Errorf("%w: no conversion partner is set", ErrAccountNotOpened)
	}
	from, err := s.companyCurrencyAccount(ctx, workspaceID, in.SellCurrency)
	if err != nil {
		return Conversion{}, err
	}
	to, err := s.companyCurrencyAccount(ctx, workspaceID, in.BuyCurrency)
	if err != nil {
		return Conversion{}, err
	}
	in.WorkspaceID, in.AgentID, in.FromAccountID, in.ToAccountID = workspaceID, "", from, to
	return s.quoteConversion(ctx, in)
}

// companyCurrencyAccount is the company's open account in currency.
func (s *DualTokenStore) companyCurrencyAccount(ctx context.Context, workspaceID, currency string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id IS NULL AND purpose = 'company'
		AND currency = $2 AND status = 'open' ORDER BY created_at DESC LIMIT 1`, workspaceID, currency).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: the company has no open %s account to convert with. Open one at /v1/money/accounts", ErrMoneyAccountNotFound, currency)
	}
	if err != nil {
		return "", fmt.Errorf("economy: conversion: %w", err)
	}
	return id, nil
}

// sideConversionPurpose binds a challenge to converting at one quote under one key, so a signature for it converts
// nothing else.
func sideConversionPurpose(quoteID, key string) string {
	h := sha256.Sum256([]byte(quoteID + "\x00" + key))
	return "convert_sides:" + hex.EncodeToString(h[:])
}

// sideConversionKey checks key, a conversion's idempotency key.
func sideConversionKey(key string) (string, error) {
	if key = strings.TrimSpace(key); key == "" || len(key) > 200 {
		return "", fmt.Errorf("%w: a conversion carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 200 characters", ErrConversion)
	}
	return key, nil
}

// companyQuote is workspaceID's quote quoteID between the company's two sides.
func companyQuote(ctx context.Context, q pgxDB, workspaceID, quoteID, lock string) (Conversion, error) {
	return scanConversion(q.QueryRow(ctx, `SELECT `+conversionCols+` FROM money_conversions WHERE id = $1 AND workspace_id = $2 AND agent_id = ''`+lock,
		quoteID, workspaceID))
}

// SideConversionChallenge issues the challenge converting at quoteID under key is signed over, and the workspace's
// passkeys that may sign it; its device keys sign it too.
func (s *DualTokenStore) SideConversionChallenge(ctx context.Context, workspaceID, quoteID, key string) (string, []string, error) {
	key, err := sideConversionKey(key)
	if err != nil {
		return "", nil, err
	}
	c, err := companyQuote(ctx, s.pool, workspaceID, quoteID, "")
	if err != nil {
		return "", nil, err
	}
	if err := c.usable(); err != nil {
		return "", nil, err
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs := []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	challenge, err := s.newChallenge(ctx, workspaceID, sideConversionPurpose(quoteID, key))
	return challenge, credentialIDs, err
}

// ConvertSides converts at the company's quote quoteID, within its life, under key, with a, the signature over
// SideConversionChallenge's challenge for them. The same key again answers the conversion it made; another conversion
// under it is ErrIdempotencyKeyReused; a quote converted already, or expired, is ErrQuote.
func (s *DualTokenStore) ConvertSides(ctx context.Context, workspaceID, quoteID, key string, a *passkey.Assertion) (Conversion, error) {
	key, err := sideConversionKey(key)
	if err != nil {
		return Conversion{}, err
	}
	prior, err := scanConversion(s.pool.QueryRow(ctx, `SELECT `+conversionCols+` FROM money_conversions
		WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, key))
	switch {
	case err == nil && prior.ID == quoteID && prior.AgentID == "":
		return prior, nil
	case err == nil:
		return Conversion{}, ErrIdempotencyKeyReused
	case !errors.Is(err, ErrQuote):
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	c, err := companyQuote(ctx, s.pool, workspaceID, quoteID, "")
	if err != nil {
		return Conversion{}, err
	}
	if err := c.usable(); err != nil {
		return Conversion{}, err
	}
	if kind, err := workspaceMoneyKind(ctx, s.pool, workspaceID); err != nil {
		return Conversion{}, err
	} else if kind != MoneyKindHybrid {
		return Conversion{}, &MoneyKindRefusal{Field: "convert", MoneyKind: kind}
	}
	if c.Funding == FundingLive { // fx is cleared on the ledger; the USDC side waits for stablecoins too
		sc, _ := CapabilityByKey(CapabilityStablecoins)
		refusal, err := capabilityLive(ctx, s.pool, workspaceID, sc)
		if err != nil {
			return Conversion{}, err
		}
		if refusal != nil {
			return Conversion{}, refusal
		}
	}
	if s.accountPartners == nil {
		return Conversion{}, fmt.Errorf("%w: no conversion partner is set", ErrAccountNotOpened)
	}
	conv, err := s.accountPartners.FX(ctx, CapabilityFX)
	if err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Read again under its lock: the same key sent twice at once converts once, and the second answers the first.
	if c, err = companyQuote(ctx, tx, workspaceID, quoteID, " FOR UPDATE"); err != nil {
		return Conversion{}, err
	}
	if c.Status == ConversionConverted && c.IdempotencyKey == key {
		return c, nil
	}
	if err := c.usable(); err != nil {
		return Conversion{}, err
	}
	// The challenge is spent only with the conversion it signs for, so a refused one can be signed again.
	if err := s.authorizeSignedTx(ctx, tx, workspaceID, sideConversionPurpose(quoteID, key), a, ErrConversionUnsigned); err != nil {
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
	// As in Convert: the partner is asked inside the transaction, so nothing is posted unless it converted.
	res, err := conv.Execute(partners.WithWorkspace(ctx, workspaceID), partners.FXExecution{ID: c.ID, QuoteRef: c.partnerRef})
	switch {
	case err != nil:
		return Conversion{}, fmt.Errorf("economy: conversion: %w", err)
	case res.Status == partners.StatusPending:
		return Conversion{}, fmt.Errorf("economy: conversion: the partner has not converted %s yet. Ask again with the same key", c.ID)
	case res.Status != partners.StatusCompleted:
		return Conversion{}, fmt.Errorf("%w: the partner did not convert it (%s): %s", ErrConversion, res.Status, res.Detail)
	}
	c, err = scanConversion(tx.QueryRow(ctx, `UPDATE money_conversions SET status = $2, idempotency_key = $3, entry_id = $4,
		converted_at = $5 WHERE id = $1 RETURNING `+conversionCols, c.ID, ConversionConverted, key, e.ID, time.Now()))
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
