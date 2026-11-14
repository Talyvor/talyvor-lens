package economy

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// instant_transfer_quote.go — B59.14: THE INSTANT TRANSFER QUOTE, LOCKED FOR 30 SECONDS (launch pack 4.1, 4.3).
//
// An Instant transfer converts a currency to USDC inside the sender's account, moves the USDC to the beneficiary, and
// converts it to their currency on arrival or pays it into their bank. Only a hybrid workspace holds both sides, so
// any other is refused with a *MoneyKindRefusal on field instant_transfer.
//
// A quote shows what the owner sends, the fee, the rate, what the beneficiary gets and when it arrives, and is locked
// for 30 seconds. The fee is the plan's FX margin (LENS_FX_MARGIN_BPS, the fee over the rate on a conversion), taken
// in the currency sent and rounded up; each conversion rounds what it buys down, so what the beneficiary gets is what
// arrives. The money layer's quotes for both conversions are kept with it, and a send within its life converts at them.
//
// The route is picked for the owner. A bank account paid in the currency sent can be paid straight from it on the same
// rail, as fast and with no conversion; when that gives the beneficiary at least as much, the quote takes it, and the
// owner sees only the speed and the cost.

// ErrInstantTransfer: an Instant transfer asked for between currencies, to a recipient or of an amount it cannot be.
var ErrInstantTransfer = errors.New("economy: invalid Instant transfer")

// instantQuoteLife is how long an Instant transfer quote is locked for.
const instantQuoteLife = 30 * time.Second

// Who an Instant transfer pays (launch pack 4.2).
const (
	InstantToAccount = "talyvor_account" // a Talyvor account: lands in its balance
	InstantToWallet  = "wallet"          // an outside wallet: the USDC goes to its address
	InstantToBank    = "bank_account"    // an outside bank account: a payout partner pays the currency into it
)

// The routes a quote can take; neither is shown.
const (
	instantRoute = "instant" // converted, moved on chain, converted or paid out
	directRoute  = "direct"  // paid straight into the bank account on its rail
)

// InstantQuoteRequest asks what sending SendMinor of SendCurrency delivers in ReceiveCurrency to a recipient of kind
// To; a bank account names its Country.
type InstantQuoteRequest struct {
	SendMinor       int64  `json:"send_minor"`
	SendCurrency    string `json:"send_currency"`
	ReceiveCurrency string `json:"receive_currency"`
	To              string `json:"to"`
	Country         string `json:"country,omitempty"`
}

// InstantQuote is an Instant transfer's price, locked until ExpiresAt.
type InstantQuote struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	To              string `json:"to"`
	Country         string `json:"country,omitempty"`
	SendMinor       int64  `json:"send_minor"` // what leaves the sender, the fee included
	SendCurrency    string `json:"send_currency"`
	FeeBPS          int64  `json:"fee_bps"`
	FeeMinor        int64  `json:"fee_minor"` // Talyvor's, in SendCurrency, rounded up
	Rate            string `json:"rate"`      // ReceiveCurrency per SendCurrency after the fee, in whole units
	ReceiveMinor    int64  `json:"receive_minor"`
	ReceiveCurrency string `json:"receive_currency"`
	// ArrivesInSeconds is true when the money lands in seconds; otherwise ArrivesWithinSeconds is the bank rail's
	// longest time, shown before the owner confirms.
	ArrivesInSeconds     bool      `json:"arrives_in_seconds"`
	ArrivesWithinSeconds int64     `json:"arrives_within_seconds,omitempty"`
	ExpiresAt            time.Time `json:"expires_at"`
	CreatedAt            time.Time `json:"created_at"`

	route        string
	onchainMinor int64  // the USDC the on-chain leg moves
	sellRef      string // the money layer's quote into USDC
	buyRef       string // the money layer's quote out of USDC
}

// Live refuses q at at once its 30 seconds are over.
func (q InstantQuote) Live(at time.Time) error {
	if !q.ExpiresAt.After(at) {
		return fmt.Errorf("%w: quote %s expired at %s. Ask for a new one", ErrQuote, q.ID, q.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// QuoteInstantTransfer prices in for workspaceID, a hybrid workspace, and keeps the quote for 30 seconds.
func (s *DualTokenStore) QuoteInstantTransfer(ctx context.Context, workspaceID string, in InstantQuoteRequest) (InstantQuote, error) {
	kind, err := workspaceMoneyKind(ctx, s.pool, workspaceID)
	if err != nil {
		return InstantQuote{}, err
	}
	if kind != MoneyKindHybrid {
		return InstantQuote{}, &MoneyKindRefusal{Field: "instant_transfer", MoneyKind: kind}
	}
	in.SendCurrency, in.ReceiveCurrency = strings.ToUpper(strings.TrimSpace(in.SendCurrency)), strings.ToUpper(strings.TrimSpace(in.ReceiveCurrency))
	in.Country = strings.ToUpper(strings.TrimSpace(in.Country))
	fiat := func(c string) bool { return c == CurrencyGBP || c == CurrencyEUR || c == CurrencyUSD }
	switch {
	case !fiat(in.SendCurrency):
		return InstantQuote{}, fmt.Errorf("%w: an Instant transfer sends GBP, EUR or USD, not %q", ErrInstantTransfer, in.SendCurrency)
	case !fiat(in.ReceiveCurrency) && in.ReceiveCurrency != CurrencyUSDC:
		return InstantQuote{}, fmt.Errorf("%w: an Instant transfer delivers GBP, EUR, USD or USDC, not %q", ErrInstantTransfer, in.ReceiveCurrency)
	case in.SendMinor <= 0:
		return InstantQuote{}, fmt.Errorf("%w: send_minor is a positive number of pence or cents", ErrInstantTransfer)
	case in.To == InstantToWallet && in.ReceiveCurrency != CurrencyUSDC:
		return InstantQuote{}, fmt.Errorf("%w: an outside wallet receives USDC", ErrInstantTransfer)
	case in.To == InstantToBank && !fiat(in.ReceiveCurrency):
		return InstantQuote{}, fmt.Errorf("%w: a bank account receives GBP, EUR or USD", ErrInstantTransfer)
	case in.To == InstantToBank && len(in.Country) != 2:
		return InstantQuote{}, fmt.Errorf("%w: a bank account names its country, two letters", ErrInstantTransfer)
	case in.To != InstantToAccount && in.To != InstantToWallet && in.To != InstantToBank:
		return InstantQuote{}, fmt.Errorf("%w: to is talyvor_account, wallet or bank_account, not %q", ErrInstantTransfer, in.To)
	case s.fxMargin == nil:
		return InstantQuote{}, fmt.Errorf("%w: no FX margin is set, so nothing is quoted", ErrInstantTransfer)
	case s.moneyLayer == nil:
		return InstantQuote{}, fmt.Errorf("%w: no money layer is set", ErrAccountNotOpened)
	}
	if in.To != InstantToBank {
		in.Country = ""
	}
	bps, _, err := s.fxMargin(ctx, s.pool, workspaceID)
	switch {
	case err != nil:
		return InstantQuote{}, fmt.Errorf("economy: the FX margin of %s: %w", workspaceID, err)
	case bps < 0 || bps > feeDenominator:
		return InstantQuote{}, fmt.Errorf("economy: the FX margin of %s is %d basis points, outside 0 to %d", workspaceID, bps, feeDenominator)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	q := InstantQuote{ID: "itq_" + uuid.NewString(), WorkspaceID: workspaceID, To: in.To, Country: in.Country, SendMinor: in.SendMinor,
		SendCurrency: in.SendCurrency, FeeBPS: bps, FeeMinor: PlatformFee(in.SendMinor, bps), ReceiveCurrency: in.ReceiveCurrency,
		ArrivesInSeconds: true, ExpiresAt: now.Add(instantQuoteLife), CreatedAt: now, route: instantRoute}
	if q.FeeMinor >= q.SendMinor {
		return InstantQuote{}, fmt.Errorf("%w: %s leaves nothing to send after the fee", ErrInstantTransfer, Money(q.SendMinor, q.SendCurrency))
	}
	rt := money.Route{Workspace: workspaceID, Country: in.Country, Capability: CapabilityFX}
	conv, err := s.moneyLayer.Convert(ctx, rt)
	if err != nil {
		return InstantQuote{}, fmt.Errorf("economy: Instant transfer quote: %w", err)
	}
	in1, err := conv.Quote(ctx, money.ConvertQuoteRequest{Key: q.ID + ":in", Sell: money.Amount{Minor: q.SendMinor - q.FeeMinor, Asset: q.SendCurrency}, Buy: money.USDC})
	if err != nil {
		return InstantQuote{}, fmt.Errorf("economy: Instant transfer quote: %w", err)
	}
	q.onchainMinor, q.sellRef, q.ReceiveMinor = in1.Buy.Minor, in1.Ref, in1.Buy.Minor
	if q.ReceiveCurrency != CurrencyUSDC {
		out, err := conv.Quote(ctx, money.ConvertQuoteRequest{Key: q.ID + ":out", Sell: in1.Buy, Buy: q.ReceiveCurrency})
		if err != nil {
			return InstantQuote{}, fmt.Errorf("economy: Instant transfer quote: %w", err)
		}
		q.ReceiveMinor, q.buyRef = out.Buy.Minor, out.Ref
	}
	if q.To == InstantToBank {
		payouts, err := s.moneyLayer.BankPayouts(ctx, money.Route{Workspace: workspaceID, Country: in.Country, Capability: CapabilityPaymentsOut})
		if err != nil {
			return InstantQuote{}, fmt.Errorf("economy: Instant transfer quote: %w", err)
		}
		rail, err := payouts.Rail(ctx, q.ReceiveCurrency, q.Country)
		if err != nil {
			return InstantQuote{}, fmt.Errorf("economy: Instant transfer quote: %w", err)
		}
		q.ArrivesInSeconds, q.ArrivesWithinSeconds = false, rail.Seconds
		// Paid straight from the currency sent, the same rail is as fast and no conversion takes anything.
		if q.ReceiveCurrency == q.SendCurrency && q.SendMinor >= q.ReceiveMinor {
			q.route, q.FeeBPS, q.FeeMinor, q.ReceiveMinor, q.onchainMinor, q.sellRef, q.buyRef = directRoute, 0, 0, q.SendMinor, 0, "", ""
		}
	}
	if q.ReceiveMinor <= 0 {
		return InstantQuote{}, fmt.Errorf("%w: %s delivers nothing in %s", ErrInstantTransfer, Money(q.SendMinor, q.SendCurrency), q.ReceiveCurrency)
	}
	q.Rate = instantRate(q.SendMinor-q.FeeMinor, q.SendCurrency, q.ReceiveMinor, q.ReceiveCurrency)
	if _, err := s.pool.Exec(ctx, `INSERT INTO instant_transfer_quotes (id, workspace_id, recipient, country, send_minor, send_currency,
		fee_bps, fee_minor, receive_minor, receive_currency, rate, arrives_within_seconds, route, onchain_minor, sell_ref, buy_ref,
		expires_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		q.ID, workspaceID, q.To, q.Country, q.SendMinor, q.SendCurrency, q.FeeBPS, q.FeeMinor, q.ReceiveMinor, q.ReceiveCurrency, q.Rate,
		q.ArrivesWithinSeconds, q.route, q.onchainMinor, q.sellRef, q.buyRef, q.ExpiresAt, q.CreatedAt); err != nil {
		return InstantQuote{}, fmt.Errorf("economy: Instant transfer quote: %w", err)
	}
	return q, nil
}

// InstantTransferQuote is workspaceID's quote id, as it was given; ErrQuote when there is none.
func (s *DualTokenStore) InstantTransferQuote(ctx context.Context, workspaceID, id string) (InstantQuote, error) {
	var q InstantQuote
	err := s.pool.QueryRow(ctx, `SELECT id, workspace_id, recipient, country, send_minor, send_currency, fee_bps, fee_minor, receive_minor,
		receive_currency, rate, arrives_within_seconds, route, onchain_minor, sell_ref, buy_ref, expires_at, created_at
		FROM instant_transfer_quotes WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&q.ID, &q.WorkspaceID, &q.To, &q.Country,
		&q.SendMinor, &q.SendCurrency, &q.FeeBPS, &q.FeeMinor, &q.ReceiveMinor, &q.ReceiveCurrency, &q.Rate, &q.ArrivesWithinSeconds,
		&q.route, &q.onchainMinor, &q.sellRef, &q.buyRef, &q.ExpiresAt, &q.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstantQuote{}, ErrQuote
	}
	if err != nil {
		return InstantQuote{}, fmt.Errorf("economy: Instant transfer quote: %w", err)
	}
	q.ArrivesInSeconds = q.ArrivesWithinSeconds == 0
	return q, nil
}

// instantRate is receive per send in whole units, for display.
func instantRate(sendMinor int64, sendCurrency string, receiveMinor int64, receiveCurrency string) string {
	r := new(big.Rat).SetFrac(big.NewInt(receiveMinor), big.NewInt(sendMinor))
	r.Mul(r, new(big.Rat).SetFrac(pow10(MoneyCurrencies[sendCurrency]), pow10(MoneyCurrencies[receiveCurrency])))
	return r.FloatString(8)
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// InstantTransferHooks is what the money layer asks on every leg of an Instant transfer (B59.13): h's screening and
// on-chain details, with each leg's record kept in instant_transfer_legs rather than in memory.
func (s *DualTokenStore) InstantTransferHooks(h *partners.TestTransferHooks) partners.TransferHooks {
	return instantTransferHooks{h, s}
}

type instantTransferHooks struct {
	*partners.TestTransferHooks
	s *DualTokenStore
}

// RecordLeg keeps l; the same leg recorded again under its key with the same outcome keeps one row.
func (h instantTransferHooks) RecordLeg(ctx context.Context, l partners.LegRecord) error {
	_, err := h.s.pool.Exec(ctx, `INSERT INTO instant_transfer_legs (workspace_id, transfer_id, leg, method, key, partner, partner_ref,
		status, outcome, amount_minor, currency) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT DO NOTHING`,
		l.Workspace, l.Transfer, l.Leg, l.Method, l.Key, l.Partner, l.Ref, l.Status, l.Outcome, l.Amount.Minor, l.Amount.Currency)
	if err != nil {
		return fmt.Errorf("economy: an Instant transfer leg's record: %w", err)
	}
	return nil
}
