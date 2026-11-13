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

// FXRates is where the Test FX partner reads its rates (B30.29): how many units of a currency one US dollar buys —
// *ecbrate.Book, the ECB reference rate. USDC is asked for as the dollar.
type FXRates interface {
	PerUSD(ctx context.Context, currency string) (*big.Rat, error)
}

// TestFXPartner is test mode for conversions: it converts no money. It quotes at Rates — the fixed test rates when
// none is set — and executes by the amount sold (see the package comment).
type TestFXPartner struct {
	book  testBook
	Rates FXRates
}

// Name is "test".
func (*TestFXPartner) Name() string { return "test" }

// Quote prices a conversion at Rates, or the fixed test rates.
func (p *TestFXPartner) Quote(ctx context.Context, req FXQuoteRequest) (FXQuote, error) {
	if err := checkMoney(req.Sell); err != nil {
		return FXQuote{}, err
	}
	if err := checkConversion(req.Sell, req.Buy); err != nil {
		return FXQuote{}, err
	}
	r, err := p.rate(ctx, req.Sell.Currency, req.Buy)
	if err != nil {
		return FXQuote{}, err
	}
	rate, buy, err := testConvert(req.Sell, req.Buy, r)
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

// rate is buy per sell in whole units from Rates; nil, the fixed test rates, when none is set.
func (p *TestFXPartner) rate(ctx context.Context, sell, buy string) (*big.Rat, error) {
	if p.Rates == nil {
		return nil, nil
	}
	perUSD := map[string]*big.Rat{}
	for _, c := range []string{sell, buy} {
		asked := c
		if c == "USDC" {
			asked = "USD"
		}
		v, err := p.Rates.PerUSD(ctx, asked)
		if err != nil {
			return nil, fmt.Errorf("the rate for %s: %w", c, err)
		}
		if v == nil || v.Sign() <= 0 {
			return nil, fmt.Errorf("the rate for %s is not a positive number", c)
		}
		perUSD[c] = v
	}
	return new(big.Rat).Quo(perUSD[buy], perUSD[sell]), nil
}

// checkConversion refuses a conversion into a currency the Test partners do not hold, or into the one sold.
func checkConversion(sell Money, buyCurrency string) error {
	if _, ok := currencies[buyCurrency]; !ok || buyCurrency == sell.Currency {
		return fmt.Errorf("%w: a conversion buys another of GBP, EUR, USD or USDC, not %q", ErrInvalid, buyCurrency)
	}
	return nil
}

// testConvert is what sell buys in another currency at rate r, buy per sell in whole units (nil: the fixed test
// rates) — rounded down to its minor unit — and the rate for display.
func testConvert(sell Money, buyCurrency string, r *big.Rat) (rate string, buy Money, err error) {
	if err := checkConversion(sell, buyCurrency); err != nil {
		return "", Money{}, err
	}
	if r == nil {
		r = new(big.Rat).Quo(testUSDPerUnit[sell.Currency], testUSDPerUnit[buyCurrency])
	}
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
