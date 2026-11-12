package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/talyvor/lens/internal/partners"
)

// B59.13 — the providers an Instant transfer chains: a conversion between a currency and the stablecoin, an on-chain
// move of the stablecoin, and a payout into an outside bank account. Each is a capability of the registry like any
// other, served by the sandbox until a partner's switch is on. Before a leg moves money the registry asks its transfer
// hooks (partners.TransferHooks) — the beneficiary and any address screened, the originator's and beneficiary's details
// carried on chain — and after it, however it ended, keeps the leg's record.

// ConvertProvider quotes and converts between a currency and the stablecoin.
type ConvertProvider interface {
	Quote(ctx context.Context, req ConvertQuoteRequest) (ConvertQuote, error)
	Convert(ctx context.Context, req Conversion) (Result, error)
}

// ConvertQuoteRequest asks what selling an amount buys in Buy: the stablecoin for a currency, or a currency for the
// stablecoin.
type ConvertQuoteRequest struct {
	Key  string `json:"key"`
	Sell Amount `json:"sell"`
	Buy  string `json:"buy_asset"`
}

// ConvertQuote is a provider's price for a conversion: Buy is what it pays, rounded down to its minor unit, and Rate is
// Buy per Sell in whole units, for display.
type ConvertQuote struct {
	Ref  string `json:"partner_ref"`
	Sell Amount `json:"sell"`
	Buy  Amount `json:"buy"`
	Rate string `json:"rate"`
}

// Conversion converts at a quote, out of the account From holding what it sells into the account To in what it buys.
type Conversion struct {
	Key      string `json:"key"`
	Transfer string `json:"transfer_id"` // the Instant transfer it is a leg of
	Quote    string `json:"quote_ref"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// OnchainProvider moves the stablecoin to an account or an address.
type OnchainProvider interface {
	Send(ctx context.Context, req OnchainSend) (Result, error)
}

// Party is one side of a transfer, as its details travel with an on-chain move.
type Party struct {
	Name    string `json:"name"`
	Country string `json:"country,omitempty"`
}

// OnchainSend moves the stablecoin out of From to the account To at the same provider, or to Address on Network.
type OnchainSend struct {
	Key         string `json:"key"`
	Transfer    string `json:"transfer_id"`
	From        string `json:"from"`
	To          string `json:"to,omitempty"`
	Network     string `json:"network,omitempty"`
	Address     string `json:"address,omitempty"`
	Amount      Amount `json:"amount"`
	Originator  Party  `json:"originator"`
	Beneficiary Party  `json:"beneficiary"`
}

// PayoutProvider pays a currency into an outside bank account, and says how long its rail takes.
type PayoutProvider interface {
	Rail(ctx context.Context, asset, country string) (Rail, error)
	Pay(ctx context.Context, req BankPayout) (Result, error)
}

// Rail is how a provider pays an asset into a bank account in a country: the rail's name, and the longest it takes.
type Rail struct {
	Name    string `json:"rail"`
	Seconds int64  `json:"settles_within_seconds"`
}

// BankPayout pays a currency out of From into a payee's bank account in Country.
type BankPayout struct {
	Key       string `json:"key"`
	Transfer  string `json:"transfer_id"`
	From      string `json:"from"`
	To        Payee  `json:"to"`
	Country   string `json:"country"`
	Amount    Amount `json:"amount"`
	Reference string `json:"reference"` // what the payee sees
}

// Convert is the conversion capability for rt.
func (r *Registry) Convert(ctx context.Context, rt Route) (ConvertProvider, error) {
	a, err := r.pickLeg(ctx, CapConvert, rt)
	if err != nil {
		return nil, err
	}
	return gatedConvert{r.gate(a, rt, CapConvert), a.Convert}, nil
}

// Onchain is the on-chain capability for rt.
func (r *Registry) Onchain(ctx context.Context, rt Route) (OnchainProvider, error) {
	a, err := r.pickLeg(ctx, CapOnchain, rt)
	if err != nil {
		return nil, err
	}
	return gatedOnchain{r.gate(a, rt, CapOnchain), a.Onchain}, nil
}

// BankPayouts is the bank payout capability for rt.
func (r *Registry) BankPayouts(ctx context.Context, rt Route) (PayoutProvider, error) {
	a, err := r.pickLeg(ctx, CapBankPayout, rt)
	if err != nil {
		return nil, err
	}
	return gatedBankPayouts{r.gate(a, rt, CapBankPayout), a.BankPayouts}, nil
}

// pickLeg is pick for a leg of an Instant transfer, which a registry hands out only with its transfer hooks.
func (r *Registry) pickLeg(ctx context.Context, c Capability, rt Route) (*Adapter, error) {
	if r.cfg.Hooks == nil {
		return nil, fmt.Errorf("%w: an Instant transfer's legs need the transfer hooks", ErrInvalid)
	}
	return r.pick(ctx, c, rt)
}

// leg runs fn as one leg of transfer through g: checked, judged and audited as every call is, with check — the leg's
// hooks — asked before fn, and the leg's record kept however it ended. A leg whose record cannot be kept answers that
// error; it is idempotent on its key, so asking again is safe.
func leg[T any](ctx context.Context, g gate, transfer string, c call, amount Amount, check func() error, fn func() (T, error)) (T, error) {
	res, err := func() (T, error) {
		var zero T
		if strings.TrimSpace(transfer) == "" {
			return zero, fmt.Errorf("%w: every leg names its Instant transfer", ErrInvalid)
		}
		return through(ctx, g, c, func() (T, error) {
			if err := check(); err != nil {
				return zero, err
			}
			return fn()
		})
	}()
	ref, status := refOf(res)
	rerr := g.r.cfg.Hooks.RecordLeg(ctx, partners.LegRecord{Workspace: g.rt.Workspace, Transfer: transfer, Leg: string(g.c),
		Method: c.method, Key: c.key, Partner: g.partner, Ref: ref, Status: status, Outcome: Outcome(err),
		Amount: partners.Money{Minor: amount.Minor, Currency: amount.Asset}})
	if err != nil {
		return res, err
	}
	if rerr != nil {
		return res, fmt.Errorf("money: the leg was made but its record was not kept: %w", rerr)
	}
	return res, nil
}

// screened is a screening as the money layer answers it: a hit refused, a close match held for review, and no answer
// the partner down.
func screened(s partners.Screening, err error) error {
	switch {
	case err != nil:
		return fromPartner(err)
	case s.Outcome == partners.ScreenHit:
		return fmt.Errorf("%w: %s", ErrRefused, s.Detail)
	case s.Outcome == partners.ScreenReview:
		return fmt.Errorf("%w: %s", ErrHeld, s.Detail)
	}
	return nil
}

func noHooks() error { return nil }

type gatedConvert struct {
	g gate
	a ConvertProvider
}

func (w gatedConvert) Quote(ctx context.Context, req ConvertQuoteRequest) (ConvertQuote, error) {
	if err := req.Sell.check(); err != nil {
		return ConvertQuote{}, err
	}
	return through(ctx, w.g, call{method: "quote", key: req.Key, write: true}, func() (ConvertQuote, error) { return w.a.Quote(ctx, req) })
}

// A conversion keeps the money in the owner's accounts, so the owner's limits are not asked.
func (w gatedConvert) Convert(ctx context.Context, req Conversion) (Result, error) {
	return leg(ctx, w.g, req.Transfer, call{method: "convert", key: req.Key, ref: req.Quote, write: true}, Amount{}, noHooks,
		func() (Result, error) { return w.a.Convert(ctx, req) })
}

type gatedOnchain struct {
	g gate
	a OnchainProvider
}

func (w gatedOnchain) Send(ctx context.Context, req OnchainSend) (Result, error) {
	h := w.g.r.cfg.Hooks
	to := Payee{Name: req.Beneficiary.Name, Network: req.Network, Address: req.Address}
	check := func() error {
		if req.Address != "" {
			if err := screened(h.ScreenAddress(ctx, req.Network, req.Address)); err != nil {
				return err
			}
		}
		if err := screened(h.ScreenBeneficiary(ctx, partners.NameScreen{Name: req.Beneficiary.Name, Country: req.Beneficiary.Country})); err != nil {
			return err
		}
		_, err := h.OnchainDetails(ctx, partners.TravelRuleTransfer{ID: req.Key, Network: req.Network, Asset: req.Amount.Asset,
			AmountMinor: req.Amount.Minor,
			Originator:  partners.TravelRuleParty{Name: req.Originator.Name, Country: req.Originator.Country, Account: req.From},
			Beneficiary: partners.TravelRuleParty{Name: req.Beneficiary.Name, Country: req.Beneficiary.Country, Account: req.To, Address: req.Address}})
		return fromPartner(err)
	}
	return leg(ctx, w.g, req.Transfer, call{method: "send", key: req.Key, ref: req.From, out: &req.Amount, payee: to.Name, to: to, write: true},
		req.Amount, check, func() (Result, error) { return w.a.Send(ctx, req) })
}

type gatedBankPayouts struct {
	g gate
	a PayoutProvider
}

func (w gatedBankPayouts) Rail(ctx context.Context, asset, country string) (Rail, error) {
	return through(ctx, w.g, call{method: "rail", ref: asset}, func() (Rail, error) { return w.a.Rail(ctx, asset, country) })
}

func (w gatedBankPayouts) Pay(ctx context.Context, req BankPayout) (Result, error) {
	check := func() error {
		return screened(w.g.r.cfg.Hooks.ScreenBeneficiary(ctx, partners.NameScreen{Name: req.To.Name, Country: req.Country}))
	}
	return leg(ctx, w.g, req.Transfer, call{method: "pay", key: req.Key, ref: req.From, out: &req.Amount, payee: req.To.Name, to: req.To, write: true},
		req.Amount, check, func() (Result, error) { return w.a.Pay(ctx, req) })
}

// sandboxRails is how the sandbox pays each currency into a bank account, and the longest each rail's rules let a
// payment take: Faster Payments two hours, SEPA Instant ten seconds, same-day ACH the business day.
var sandboxRails = map[string]Rail{
	GBP: {Name: "faster_payments", Seconds: 2 * 60 * 60},
	EUR: {Name: "sepa_instant", Seconds: 10},
	USD: {Name: "ach_same_day", Seconds: 24 * 60 * 60},
}

// failing is the error the sandbox was asked to fail c with, if it was. The caller holds s.mu.
func (s *sandbox) failing(c Capability) error {
	if err := s.fail[c]; err != nil {
		return fmt.Errorf("%w: the sandbox was asked to fail %s", err, c)
	}
	return nil
}

// FailAt makes the sandbox adapter a fail every later call of the Instant transfer leg c — CapConvert, CapOnchain or
// CapBankPayout — with err (ErrRefused, ErrPartnerDown, …), moving nothing, until it is called again with a nil err: a
// test fails a transfer at any leg on request. A leg asked again with the same key after it went through answers as it
// did, failing or not.
func FailAt(a *Adapter, c Capability, err error) error {
	var sc sandboxConvert
	ok := a != nil
	if ok {
		sc, ok = a.Convert.(sandboxConvert)
	}
	if !ok {
		return errors.New("money: only the sandbox fails on request")
	}
	if c != CapConvert && c != CapOnchain && c != CapBankPayout {
		return fmt.Errorf("%w: %s is not a leg of an Instant transfer", ErrInvalid, c)
	}
	sc.s.mu.Lock()
	defer sc.s.mu.Unlock()
	if err == nil {
		delete(sc.s.fail, c)
	} else {
		sc.s.fail[c] = err
	}
	return nil
}

type sandboxConvert struct{ s *sandbox }

// Quote prices a conversion at the Test FX partner's rates.
func (c sandboxConvert) Quote(ctx context.Context, req ConvertQuoteRequest) (ConvertQuote, error) {
	if (req.Sell.Asset == USDC) == (req.Buy == USDC) {
		return ConvertQuote{}, fmt.Errorf("%w: a conversion is between a currency and USDC, not %s and %q", ErrInvalid, req.Sell.Asset, req.Buy)
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return once(c.s, "convert_quote", req.Key, req, func() (ConvertQuote, error) {
		q, err := c.s.fx.Quote(ctx, partners.FXQuoteRequest{ID: "convert:" + req.Key, Sell: partners.Money{Minor: req.Sell.Minor, Currency: req.Sell.Asset}, Buy: req.Buy})
		if err != nil {
			return ConvertQuote{}, fromPartner(err)
		}
		cq := ConvertQuote{Ref: q.Ref, Sell: req.Sell, Buy: Amount{Minor: q.Buy.Minor, Asset: q.Buy.Currency}, Rate: q.Rate}
		c.s.quotes[q.Ref] = cq
		return cq, nil
	})
}

// Convert executes a quote it gave with the Test FX partner, and moves what it sells and buys between the two accounts.
func (c sandboxConvert) Convert(ctx context.Context, req Conversion) (Result, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return once(c.s, "convert", req.Key, req, func() (Result, error) {
		if err := c.s.failing(CapConvert); err != nil {
			return Result{}, err
		}
		q, ok := c.s.quotes[req.Quote]
		if !ok {
			return Result{}, fmt.Errorf("%w: no quote %q", ErrNotFound, req.Quote)
		}
		from, err := c.s.debitable(req.From, q.Sell)
		if err != nil {
			return Result{}, err
		}
		to, err := c.s.held(req.To)
		if err != nil {
			return Result{}, err
		}
		if to.asset != q.Buy.Asset {
			return Result{}, fmt.Errorf("%w: %s holds %s, not %s", ErrInvalid, req.To, to.asset, q.Buy.Asset)
		}
		r, err := c.s.fx.Execute(ctx, partners.FXExecution{ID: "convert:" + req.Key, QuoteRef: req.Quote})
		if err != nil {
			return Result{}, fromPartner(err)
		}
		if r.Status == partners.StatusFailed {
			return Result{}, fmt.Errorf("%w: %s", ErrRefused, r.Detail)
		}
		from.balance -= q.Sell.Minor
		to.balance += q.Buy.Minor
		return fromResult(r), nil
	})
}

type sandboxOnchain struct{ s *sandbox }

// Send moves USDC to another of the sandbox's accounts, or to an address through the Test stablecoin partner.
func (o sandboxOnchain) Send(ctx context.Context, req OnchainSend) (Result, error) {
	switch {
	case req.Amount.Asset != USDC:
		return Result{}, fmt.Errorf("%w: on chain moves USDC, not %s", ErrInvalid, req.Amount.Asset)
	case (req.To == "") == (req.Address == ""):
		return Result{}, fmt.Errorf("%w: an on-chain move goes to an account or to an address", ErrInvalid)
	case req.Address != "" && req.Network != SandboxNetwork:
		return Result{}, fmt.Errorf("%w: the sandbox's USDC is on %s, not %q", ErrInvalid, SandboxNetwork, req.Network)
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	return once(o.s, "onchain", req.Key, req, func() (Result, error) {
		if err := o.s.failing(CapOnchain); err != nil {
			return Result{}, err
		}
		if req.Address != "" {
			return o.s.send(ctx, "onchain:"+req.Key, req.From, Payee{Name: req.Beneficiary.Name, Network: req.Network, Address: req.Address}, req.Amount, "")
		}
		from, err := o.s.debitable(req.From, req.Amount)
		if err != nil {
			return Result{}, err
		}
		to, err := o.s.held(req.To)
		if err != nil {
			return Result{}, err
		}
		if to.asset != USDC || req.From == req.To {
			return Result{}, fmt.Errorf("%w: an on-chain move is between two USDC accounts", ErrInvalid)
		}
		from.balance -= req.Amount.Minor
		to.balance += req.Amount.Minor
		return Result{Ref: sandboxRef("onchain", req.Key), Status: StatusCompleted}, nil
	})
}

type sandboxBankPayouts struct{ s *sandbox }

// Rail is sandboxRails' rail for asset, in any country.
func (sandboxBankPayouts) Rail(_ context.Context, asset, _ string) (Rail, error) {
	r, ok := sandboxRails[asset]
	if !ok {
		return Rail{}, fmt.Errorf("%w: a bank account is paid in GBP, EUR or USD, not %q", ErrInvalid, asset)
	}
	return r, nil
}

// Pay pays through the Test account partner, as any payment out of the sandbox does.
func (p sandboxBankPayouts) Pay(ctx context.Context, req BankPayout) (Result, error) {
	if _, ok := sandboxRails[req.Amount.Asset]; !ok {
		return Result{}, fmt.Errorf("%w: a bank account is paid in GBP, EUR or USD, not %q", ErrInvalid, req.Amount.Asset)
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	return once(p.s, "bank_payout", req.Key, req, func() (Result, error) {
		if err := p.s.failing(CapBankPayout); err != nil {
			return Result{}, err
		}
		return p.s.send(ctx, "bank_payout:"+req.Key, req.From, req.To, req.Amount, req.Reference)
	})
}
