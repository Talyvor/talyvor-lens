package partners

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// AccountPartner holds money in currency accounts for Talyvor's customers, gives them account details others can
// pay into, and moves money in and out: the capabilities currency_accounts, account_details, payments_in,
// payments_out, pay_by_bank and payouts_to_people.
type AccountPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// OpenAccount opens an account in one currency for a holder. Idempotent on req.ID.
	OpenAccount(ctx context.Context, req AccountRequest) (Account, error)
	// AccountDetails is what someone needs to pay into an open account.
	AccountDetails(ctx context.Context, accountRef string) (AccountDetails, error)
	// SendPayment pays req.Amount out of an account to a payee. Idempotent on req.ID.
	SendPayment(ctx context.Context, req PaymentRequest) (Result, error)
	// PaymentStatus says where a payment — sent, or in by pay-by-bank — stands now.
	PaymentStatus(ctx context.Context, paymentRef string) (Result, error)
	// StatementLines is every line on an account's statement at or after since, oldest first.
	StatementLines(ctx context.Context, accountRef string, since time.Time) ([]StatementLine, error)
	// PayByBank asks a payer to pay req.Amount into an account from their own bank: the payer authorises it at
	// the returned URL. Idempotent on req.ID.
	PayByBank(ctx context.Context, req PayByBankRequest) (PayByBank, error)
	// CheckPayee asks the payee's bank whether the name is the account's: confirmation of payee (B30.16).
	CheckPayee(ctx context.Context, p Payee) (PayeeCheck, error)
	// QuoteInternational prices paying req.Amount out of an account to a payee in another currency, through the
	// international route (B30.22): the partner's cost, the rate, what arrives, and until when a payment may be made
	// at it (SendPayment with the quote's Ref). Idempotent on req.ID.
	QuoteInternational(ctx context.Context, req InternationalQuoteRequest) (InternationalQuote, error)
}

// AccountRequest asks for an account in one currency.
type AccountRequest struct {
	ID       string `json:"id"`     // the caller's id for the account
	Holder   string `json:"holder"` // the holder's name, as the account details show it
	Currency string `json:"currency"`
}

// Account is an account with a partner: open once its status is completed.
type Account struct {
	Result
	Currency string `json:"currency"`
}

// AccountDetails is what someone needs to pay into an account: a sort code and account number for pounds, an
// IBAN and BIC for euros, a routing and account number for dollars.
type AccountDetails struct {
	AccountRef    string `json:"account_ref"`
	Holder        string `json:"holder"`
	Currency      string `json:"currency"`
	SortCode      string `json:"sort_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	IBAN          string `json:"iban,omitempty"`
	BIC           string `json:"bic,omitempty"`
	RoutingNumber string `json:"routing_number,omitempty"`
}

// Payee is who a payment goes to, and the details it is paid by.
type Payee struct {
	Name          string `json:"name"`
	SortCode      string `json:"sort_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	IBAN          string `json:"iban,omitempty"`
	BIC           string `json:"bic,omitempty"`
	RoutingNumber string `json:"routing_number,omitempty"`
}

// The answers to a confirmation of payee.
const (
	PayeeExactMatch = "exact_match" // the name is the account holder's
	PayeeCloseMatch = "close_match" // the name is close to the holder's, which SuggestedName is
	PayeeNoMatch    = "no_match"    // the name is not the account holder's
)

// PayeeCheck is what the payee's bank said of a payee's name and account.
type PayeeCheck struct {
	Result        string `json:"result"`
	SuggestedName string `json:"suggested_name,omitempty"`
}

// PaymentRequest pays money out of an account. With QuoteRef set it is an international payment at that quote
// (QuoteInternational): Amount is the quote's Sell, the partner takes its cost on top, converts, and pays the payee what
// the quote said arrives.
type PaymentRequest struct {
	ID         string `json:"id"` // the caller's id for the payment
	AccountRef string `json:"account_ref"`
	Amount     Money  `json:"amount"`
	Payee      Payee  `json:"payee"`
	Reference  string `json:"reference"` // what the payee sees
	QuoteRef   string `json:"quote_ref,omitempty"`
}

// InternationalQuoteRequest asks what paying Amount out of an account to a payee whose account is in another
// currency costs, and what arrives.
type InternationalQuoteRequest struct {
	ID         string `json:"id"` // the caller's id for the quote
	AccountRef string `json:"account_ref"`
	Amount     Money  `json:"amount"`           // what is sent, in the account's currency, before the partner's cost
	Deliver    string `json:"deliver_currency"` // the payee's currency
	Country    string `json:"country"`          // the payee's, ISO 3166-1 alpha-2
	Payee      Payee  `json:"payee"`
}

// InternationalQuote prices an international payment: the partner's cost on top of Sell, in Sell's currency; the
// rate, Delivers per Sell in whole units, as a decimal string for display; what the payee receives, rounded down to
// its minor unit; and until when the payment may be made at it.
type InternationalQuote struct {
	Ref       string    `json:"partner_ref"`
	Sell      Money     `json:"sell"`
	Cost      Money     `json:"cost"`
	Delivers  Money     `json:"delivers"`
	Rate      string    `json:"rate"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PayByBankRequest asks a payer to pay into an account from their own bank.
type PayByBankRequest struct {
	ID         string `json:"id"` // the caller's id for the payment
	AccountRef string `json:"account_ref"`
	Amount     Money  `json:"amount"`
	Payer      string `json:"payer"`
}

// PayByBank is a pay-by-bank payment: where the payer authorises it, and where it stands.
type PayByBank struct {
	Result
	AuthoriseURL string `json:"authorise_url"`
}

// StatementLine is one line of an account's statement. Amount reads as a money posting does: positive money in,
// negative money out.
type StatementLine struct {
	AccountRef  string    `json:"account_ref"`
	PaymentRef  string    `json:"payment_ref"`
	Amount      Money     `json:"amount"`
	Description string    `json:"description"`
	At          time.Time `json:"at"`
}

// TestAccountPartner is test mode for accounts: it holds and moves no money. Accounts open by the holder's name,
// payments and pay-by-bank answer by their amount (see the package comment), and every payment that moved shows
// on the account's statement — a returned one as the money going and the money coming back.
type TestAccountPartner struct {
	book  testBook
	lines map[string][]StatementLine
}

// Name is "test".
func (*TestAccountPartner) Name() string { return "test" }

// OpenAccount opens an account by the holder's name.
func (p *TestAccountPartner) OpenAccount(_ context.Context, req AccountRequest) (Account, error) {
	if _, ok := currencies[req.Currency]; !ok {
		return Account{}, fmt.Errorf("%w: a currency is GBP, EUR, USD or USDC, not %q", ErrInvalid, req.Currency)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("acct", req.ID, req, byName(req.Holder))
	if err != nil {
		return Account{}, err
	}
	return Account{Result: op.result, Currency: req.Currency}, nil
}

// AccountDetails makes up details for an open account: they reach no bank. Pounds get sort code 00-00-00; euros
// an IBAN whose bank code is TEST; dollars routing number 000000000.
func (p *TestAccountPartner) AccountDetails(_ context.Context, accountRef string) (AccountDetails, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	acct, err := p.open(accountRef)
	if err != nil {
		return AccountDetails{}, err
	}
	req := acct.request.(AccountRequest)
	d := AccountDetails{AccountRef: accountRef, Holder: req.Holder, Currency: req.Currency}
	number := testDigits(accountRef, 8)
	switch req.Currency {
	case "GBP":
		d.SortCode, d.AccountNumber = "00-00-00", number
	case "EUR":
		d.IBAN, d.BIC = testIBAN("TEST000000"+number), "TESTGB00"
	case "USD":
		d.RoutingNumber, d.AccountNumber = "000000000", number
	default:
		return AccountDetails{}, fmt.Errorf("%w: a %s account has no bank details; a stablecoin address is the StablecoinPartner's", ErrInvalid, req.Currency)
	}
	return d, nil
}

// testIBAN is a GB-format IBAN for bban with a valid check: it checks as an IBAN and reaches no bank.
func testIBAN(bban string) string {
	digits := ""
	for _, r := range bban + "GB00" {
		if r >= 'A' && r <= 'Z' {
			digits += fmt.Sprint(int(r-'A') + 10)
		} else {
			digits += string(r)
		}
	}
	n, _ := new(big.Int).SetString(digits, 10)
	check := 98 - new(big.Int).Mod(n, big.NewInt(97)).Int64()
	return fmt.Sprintf("GB%02d%s", check, bban)
}

// open is an account this partner opened and has not withdrawn. The caller holds p.book.mu.
func (p *TestAccountPartner) open(accountRef string) (*testOp, error) {
	acct, err := p.book.get("acct", accountRef)
	if err != nil {
		return nil, err
	}
	if st := acct.status().Status; st != StatusCompleted {
		return nil, fmt.Errorf("%w: account %s is %s, not open", ErrInvalid, accountRef, st)
	}
	return acct, nil
}

// SendPayment moves no money: it answers by the amount, and puts what moved on the statement. An international
// payment (req.QuoteRef) is at its quote — the same account and amount, within the quote's life — and its statement
// line takes the partner's cost too.
func (p *TestAccountPartner) SendPayment(_ context.Context, req PaymentRequest) (Result, error) {
	if err := checkMoney(req.Amount); err != nil {
		return Result{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if err := p.sameCurrency(req.AccountRef, req.Amount); err != nil {
		return Result{}, err
	}
	out, what := req.Amount.Minor, "Payment to "+req.Payee.Name
	if req.QuoteRef != "" {
		q, err := p.book.get("iq", req.QuoteRef)
		if err != nil {
			return Result{}, err
		}
		quote := q.request.(InternationalQuoteRequest)
		if quote.AccountRef != req.AccountRef || quote.Amount != req.Amount {
			return Result{}, fmt.Errorf("%w: quote %s is for %s from another account or amount", ErrInvalid, req.QuoteRef, quote.Amount)
		}
		if _, retry := p.book.ops[testRef("pay", req.ID)]; !retry && p.book.clock().After(q.at.Add(testQuoteLife)) {
			return Result{}, fmt.Errorf("%w: quote %s has expired", ErrInvalid, req.QuoteRef)
		}
		out, what = out+testIntlCost, "International payment to "+req.Payee.Name
	}
	op, fresh, err := p.book.record("pay", req.ID, req, byAmount(req.Amount))
	if err != nil {
		return Result{}, err
	}
	if fresh {
		p.post(req.AccountRef, op, -out, req.Amount.Currency, what)
	}
	return op.result, nil
}

// testIntlCost is the Test partner's cost for an international payment, in minor units of the sending currency: a
// test figure, never a partner's price.
const testIntlCost = 100

// QuoteInternational prices an international payment at the Test rates (testUSDPerUnit) and cost.
func (p *TestAccountPartner) QuoteInternational(_ context.Context, req InternationalQuoteRequest) (InternationalQuote, error) {
	if err := checkMoney(req.Amount); err != nil {
		return InternationalQuote{}, err
	}
	rate, delivers, err := testConvert(req.Amount, req.Deliver)
	if err != nil {
		return InternationalQuote{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if err := p.sameCurrency(req.AccountRef, req.Amount); err != nil {
		return InternationalQuote{}, err
	}
	op, _, err := p.book.record("iq", req.ID, req, func(string) (Result, Status) {
		return Result{Status: StatusCompleted}, StatusCompleted
	})
	if err != nil {
		return InternationalQuote{}, err
	}
	return InternationalQuote{Ref: op.result.Ref, Sell: req.Amount, Cost: Money{testIntlCost, req.Amount.Currency}, Delivers: delivers,
		Rate: rate, ExpiresAt: op.at.Add(testQuoteLife)}, nil
}

// PayByBank moves no money: it answers by the amount, and puts what arrived on the statement. Its URL is on a
// reserved domain that resolves nowhere.
func (p *TestAccountPartner) PayByBank(_ context.Context, req PayByBankRequest) (PayByBank, error) {
	if err := checkMoney(req.Amount); err != nil {
		return PayByBank{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if err := p.sameCurrency(req.AccountRef, req.Amount); err != nil {
		return PayByBank{}, err
	}
	op, fresh, err := p.book.record("pbb", req.ID, req, byAmount(req.Amount))
	if err != nil {
		return PayByBank{}, err
	}
	if fresh {
		p.post(req.AccountRef, op, req.Amount.Minor, req.Amount.Currency, "Payment from "+req.Payer+" by bank")
	}
	return PayByBank{Result: op.result, AuthoriseURL: "https://pay-by-bank.test.invalid/authorise/" + op.result.Ref}, nil
}

// sameCurrency refuses money for an account that is not open or holds another currency. The caller holds
// p.book.mu.
func (p *TestAccountPartner) sameCurrency(accountRef string, m Money) error {
	acct, err := p.open(accountRef)
	if err != nil {
		return err
	}
	if cur := acct.request.(AccountRequest).Currency; cur != m.Currency {
		return fmt.Errorf("%w: account %s holds %s, not %s", ErrInvalid, accountRef, cur, m.Currency)
	}
	return nil
}

// post puts a payment on the statement: a completed one once, a returned one twice — out and back — and a pending
// or failed one not at all. The caller holds p.book.mu.
func (p *TestAccountPartner) post(accountRef string, op *testOp, minor int64, currency, description string) {
	if op.result.Status != StatusCompleted {
		return
	}
	if p.lines == nil {
		p.lines = map[string][]StatementLine{}
	}
	line := StatementLine{AccountRef: accountRef, PaymentRef: op.result.Ref, Amount: Money{minor, currency}, Description: description, At: op.at}
	p.lines[accountRef] = append(p.lines[accountRef], line)
	if op.later == StatusReturned {
		line.Amount.Minor, line.Description = -minor, "Returned: "+description
		p.lines[accountRef] = append(p.lines[accountRef], line)
	}
}

// testCloseWord is the test word that makes a payee's name a close match.
var testCloseWord = regexp.MustCompile(`(?i)TESTCLOSE`)

// CheckPayee answers by the name: one containing TESTNOMATCH matches no account; TESTCLOSE is a close match, whose
// holder is the name without the word; any other name matches exactly.
func (*TestAccountPartner) CheckPayee(_ context.Context, p Payee) (PayeeCheck, error) {
	switch {
	case strings.Contains(strings.ToUpper(p.Name), "TESTNOMATCH"):
		return PayeeCheck{Result: PayeeNoMatch}, nil
	case testCloseWord.MatchString(p.Name):
		return PayeeCheck{Result: PayeeCloseMatch, SuggestedName: strings.Join(strings.Fields(testCloseWord.ReplaceAllString(p.Name, " ")), " ")}, nil
	}
	return PayeeCheck{Result: PayeeExactMatch}, nil
}

// PaymentStatus is where a payment stands now: a returned one reads returned from the first read after it was
// sent.
func (p *TestAccountPartner) PaymentStatus(_ context.Context, paymentRef string) (Result, error) {
	if r, err := p.book.status("pay", paymentRef); err == nil {
		return r, nil
	}
	return p.book.status("pbb", paymentRef)
}

// StatementLines is the account's statement from since.
func (p *TestAccountPartner) StatementLines(_ context.Context, accountRef string, since time.Time) ([]StatementLine, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if _, err := p.book.get("acct", accountRef); err != nil {
		return nil, err
	}
	out := []StatementLine{}
	for _, l := range p.lines[accountRef] {
		if !l.At.Before(since) {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}
