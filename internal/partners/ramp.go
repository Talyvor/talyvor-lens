package partners

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// RampPartner turns money into USDC and USDC back into money: the on- and off-ramp an agent's wallet is topped up and
// cashed out through (B48.8). It buys with a card, a bank transfer or money paid in from a wallet, and sells to a
// bank account.
type RampPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Quote prices selling req.Sell for req.Buy by req.Method: the rate, the partner's fee and what it buys, until when,
	// and where the money it sells is paid in when it comes from a wallet.
	Quote(ctx context.Context, req RampQuoteRequest) (RampQuote, error)
	// Execute buys or sells at a quote, paying what it buys to req.To. Idempotent on req.ID.
	Execute(ctx context.Context, req RampExecution) (Result, error)
	// Return gives back money paid in at a quote the partner has not executed — to the card or bank it was charged to,
	// or the wallet that paid it in. Idempotent on req.ID.
	Return(ctx context.Context, req RampReturn) (Result, error)
}

// The ways a ramp is paid.
const (
	RampCard         = "card"          // the owner's card, charged by the partner
	RampBankTransfer = "bank_transfer" // a transfer from the owner's own bank to the partner
	RampWallet       = "wallet"        // money one of the owner's wallets pays in to the partner
)

// RampQuoteRequest asks what selling an amount by a method buys.
type RampQuoteRequest struct {
	ID     string `json:"id"` // the caller's id for the quote
	Sell   Money  `json:"sell"`
	Buy    string `json:"buy_currency"`
	Method string `json:"method"`
}

// RampQuote is a price: the partner's Fee is taken from Sell, and Buy is what the rest buys at Rate, rounded down.
type RampQuote struct {
	Ref       string    `json:"partner_ref"`
	Sell      Money     `json:"sell"`
	Fee       Money     `json:"fee"`
	Buy       Money     `json:"buy"`
	Rate      string    `json:"rate"`
	ExpiresAt time.Time `json:"expires_at"`
	// PayIn is where a wallet pays Sell in when Method is wallet: an address for USDC, the partner's name for money.
	PayInName    string `json:"pay_in_name,omitempty"`
	PayInAddress string `json:"pay_in_address,omitempty"`
}

// RampExecution buys or sells at a quote. To is who gets what it buys: a wallet's address for USDC, a bank account
// for money.
type RampExecution struct {
	ID        string `json:"id"` // the caller's id for the execution
	QuoteRef  string `json:"quote_ref"`
	ToAddress string `json:"to_address,omitempty"`
	ToPayee   Payee  `json:"to_payee,omitempty"`
}

// RampReturn gives back Amount, paid in at a quote that was not executed.
type RampReturn struct {
	ID       string `json:"id"` // the caller's id for the return
	QuoteRef string `json:"quote_ref"`
	Amount   Money  `json:"amount"`
}

// testRampFeeBasisPoints is the Test ramp's fee, a share of what is sold: a test figure, never a partner's price.
var testRampFeeBasisPoints = map[string]int64{RampCard: 300, RampBankTransfer: 100, RampWallet: 50}

// TestRampPartner is test mode for the ramp: it buys and sells nothing. It quotes at the Test FX rates less its test
// fee, and executes by the amount sold (see the package comment).
type TestRampPartner struct {
	book testBook
}

// Name is "test".
func (*TestRampPartner) Name() string { return "test" }

// Quote prices a buy or a sell at the Test rates and fee.
func (p *TestRampPartner) Quote(_ context.Context, req RampQuoteRequest) (RampQuote, error) {
	if err := checkMoney(req.Sell); err != nil {
		return RampQuote{}, err
	}
	bp, ok := testRampFeeBasisPoints[req.Method]
	if !ok {
		return RampQuote{}, fmt.Errorf("%w: a ramp is paid by card, bank_transfer or wallet, not %q", ErrInvalid, req.Method)
	}
	if (req.Sell.Currency == "USDC") == (req.Buy == "USDC") {
		return RampQuote{}, fmt.Errorf("%w: a ramp buys USDC with money or sells it for money", ErrInvalid)
	}
	fee := Money{Minor: req.Sell.Minor * bp / 10_000, Currency: req.Sell.Currency}
	if fee.Minor >= req.Sell.Minor {
		return RampQuote{}, fmt.Errorf("%w: %s is all fee", ErrInvalid, req.Sell)
	}
	rate, buy, err := testConvert(Money{Minor: req.Sell.Minor - fee.Minor, Currency: req.Sell.Currency}, req.Buy)
	if err != nil {
		return RampQuote{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("rampq", req.ID, req, func(string) (Result, Status) {
		return Result{Status: StatusCompleted}, StatusCompleted
	})
	if err != nil {
		return RampQuote{}, err
	}
	q := RampQuote{Ref: op.result.Ref, Sell: req.Sell, Fee: fee, Buy: buy, Rate: rate, ExpiresAt: op.at.Add(testQuoteLife)}
	if req.Method == RampWallet {
		q.PayInName = "Test ramp"
		if req.Sell.Currency == "USDC" {
			sum := sha256.Sum256([]byte("talyvor-test-ramp\x00" + op.result.Ref))
			q.PayInAddress = "0x" + hex.EncodeToString(sum[:20])
		}
	}
	return q, nil
}

// Execute buys or sells nothing: it answers by the amount the quote sells. A quote past its life is refused.
func (p *TestRampPartner) Execute(_ context.Context, req RampExecution) (Result, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	q, err := p.book.get("rampq", req.QuoteRef)
	if err != nil {
		return Result{}, err
	}
	if _, retry := p.book.ops[testRef("ramp", req.ID)]; !retry && p.book.clock().After(q.at.Add(testQuoteLife)) {
		return Result{}, fmt.Errorf("%w: quote %s has expired", ErrInvalid, req.QuoteRef)
	}
	op, _, err := p.book.record("ramp", req.ID, req, byAmount(q.request.(RampQuoteRequest).Sell))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// Return gives back nothing: it answers by the amount returned, for a quote it gave and has not executed, and only
// what the quote sold.
func (p *TestRampPartner) Return(_ context.Context, req RampReturn) (Result, error) {
	if err := checkMoney(req.Amount); err != nil {
		return Result{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	q, err := p.book.get("rampq", req.QuoteRef)
	if err != nil {
		return Result{}, err
	}
	if sold := q.request.(RampQuoteRequest).Sell; sold != req.Amount {
		return Result{}, fmt.Errorf("%w: quote %s sold %s, not %s", ErrInvalid, req.QuoteRef, sold, req.Amount)
	}
	for _, op := range p.book.ops {
		if e, ok := op.request.(RampExecution); ok && e.QuoteRef == req.QuoteRef {
			return Result{}, fmt.Errorf("%w: quote %s was executed", ErrInvalid, req.QuoteRef)
		}
	}
	op, _, err := p.book.record("rampr", req.ID, req, byAmount(req.Amount))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}
