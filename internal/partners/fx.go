package partners

import (
	"context"
	"fmt"
	"math/big"
	"time"
)

// FXPartner converts money between currencies: the capability fx.
type FXPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Quote prices selling req.Sell for req.Buy: how much it buys, until when.
	Quote(ctx context.Context, req FXQuoteRequest) (FXQuote, error)
	// Execute converts at a quote. Idempotent on req.ID.
	Execute(ctx context.Context, req FXExecution) (Result, error)
	// Status says where a conversion stands now.
	Status(ctx context.Context, ref string) (Result, error)
}

// FXQuoteRequest asks what selling an amount buys in another currency.
type FXQuoteRequest struct {
	ID   string `json:"id"` // the caller's id for the quote
	Sell Money  `json:"sell"`
	Buy  string `json:"buy_currency"`
}

// FXQuote is a price for a conversion. Rate is buy per sell in whole units, as a decimal string for display; Buy
// is what the conversion pays, rounded down to its minor unit.
type FXQuote struct {
	Ref       string    `json:"partner_ref"`
	Sell      Money     `json:"sell"`
	Buy       Money     `json:"buy"`
	Rate      string    `json:"rate"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FXExecution converts at a quote.
type FXExecution struct {
	ID       string `json:"id"` // the caller's id for the conversion
	QuoteRef string `json:"quote_ref"`
}

// testUSDPerUnit is the Test FX partner's fixed rates: what one whole unit of each currency is in US dollars. They
// are test figures and never a market price.
var testUSDPerUnit = map[string]*big.Rat{"GBP": big.NewRat(5, 4), "EUR": big.NewRat(11, 10), "USD": big.NewRat(1, 1), "USDC": big.NewRat(1, 1)}

// testQuoteLife is how long a Test quote can be executed.
const testQuoteLife = time.Minute

// TestFXPartner is test mode for conversions: it converts no money. It quotes at fixed test rates, and executes by
// the amount sold (see the package comment).
type TestFXPartner struct {
	book testBook
}

// Name is "test".
func (*TestFXPartner) Name() string { return "test" }

// Quote prices a conversion at the Test rates.
func (p *TestFXPartner) Quote(_ context.Context, req FXQuoteRequest) (FXQuote, error) {
	if err := checkMoney(req.Sell); err != nil {
		return FXQuote{}, err
	}
	rate, buy, err := testConvert(req.Sell, req.Buy)
	if err != nil {
		return FXQuote{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("fxq", req.ID, req, func(string) (Result, Status) {
		return Result{Status: StatusCompleted}, StatusCompleted
	})
	if err != nil {
		return FXQuote{}, err
	}
	return FXQuote{Ref: op.result.Ref, Sell: req.Sell, Buy: buy, Rate: rate, ExpiresAt: op.at.Add(testQuoteLife)}, nil
}

// testConvert is what sell buys in another currency at the Test rates — rounded down to its minor unit — and the rate,
// buy per sell in whole units, for display.
func testConvert(sell Money, buyCurrency string) (rate string, buy Money, err error) {
	if _, ok := currencies[buyCurrency]; !ok || buyCurrency == sell.Currency {
		return "", Money{}, fmt.Errorf("%w: a conversion buys another of GBP, EUR, USD or USDC, not %q", ErrInvalid, buyCurrency)
	}
	r := new(big.Rat).Quo(testUSDPerUnit[sell.Currency], testUSDPerUnit[buyCurrency])
	// buy minor = sell minor × rate × 10^(buy decimals − sell decimals), rounded down.
	b := new(big.Rat).Mul(new(big.Rat).SetInt64(sell.Minor), r)
	b.Mul(b, new(big.Rat).SetFrac(pow10(currencies[buyCurrency]), pow10(currencies[sell.Currency])))
	buyMinor := new(big.Int).Quo(b.Num(), b.Denom())
	if !buyMinor.IsInt64() || buyMinor.Sign() <= 0 {
		return "", Money{}, fmt.Errorf("%w: %s buys nothing in %s", ErrInvalid, sell, buyCurrency)
	}
	return r.FloatString(8), Money{buyMinor.Int64(), buyCurrency}, nil
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// Execute converts nothing: it answers by the amount the quote sells. A quote past its life is refused.
func (p *TestFXPartner) Execute(_ context.Context, req FXExecution) (Result, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	q, err := p.book.get("fxq", req.QuoteRef)
	if err != nil {
		return Result{}, err
	}
	if _, retry := p.book.ops[testRef("fx", req.ID)]; !retry && p.book.clock().After(q.at.Add(testQuoteLife)) {
		return Result{}, fmt.Errorf("%w: quote %s has expired", ErrInvalid, req.QuoteRef)
	}
	op, _, err := p.book.record("fx", req.ID, req, byAmount(q.request.(FXQuoteRequest).Sell))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// Status is where a conversion stands now.
func (p *TestFXPartner) Status(_ context.Context, ref string) (Result, error) {
	return p.book.status("fx", ref)
}
