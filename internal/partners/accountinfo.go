package partners

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// AccountInfoPartner reads a person's account at another bank, with their consent: open banking's account
// information (B53.13). It moves no money. Each consent covers one account and runs out, so it is renewed; a bank may
// limit how often an account is read without the person there (EU banks: four times a day), so what it answers is as
// fresh as its last read, never instant.
type AccountInfoPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Consent asks the bank for read access to one of the person's accounts. Idempotent on req.ID.
	Consent(ctx context.Context, req ConsentRequest) (AccountConsent, error)
	// Renew extends a consent before it runs out.
	Renew(ctx context.Context, consentRef string) (AccountConsent, error)
	// Balance is the account's balance as the bank reports it now.
	Balance(ctx context.Context, consentRef string) (BankBalance, error)
	// Transactions is the account's booked transactions since since, oldest first.
	Transactions(ctx context.Context, consentRef string, since time.Time) ([]BankTransaction, error)
	// Revoke ends a consent: nothing is read under it again.
	Revoke(ctx context.Context, consentRef string) error
}

// ConsentRequest asks for read access to one account at a bank.
type ConsentRequest struct {
	ID      string `json:"id"` // the caller's id for the consent
	Bank    string `json:"bank"`
	Account string `json:"account"` // the person's name for the account at the bank, as they chose it there
}

// AccountConsent is read access to one account, until ExpiresAt.
type AccountConsent struct {
	Result
	Bank      string    `json:"bank"`
	Account   string    `json:"account"`
	Currency  string    `json:"currency"`
	ExpiresAt time.Time `json:"expires_at"`
}

// BankBalance is an account's balance at AsOf.
type BankBalance struct {
	Money
	AsOf time.Time `json:"as_of"`
}

// BankTransaction is one booked transaction: Amount.Minor is negative for money out, positive for money in.
type BankTransaction struct {
	ID       string    `json:"id"` // the bank's, unique in the account
	Amount   Money     `json:"amount"`
	Merchant string    `json:"merchant"`
	MCC      string    `json:"mcc,omitempty"` // when the bank passes the card's merchant category code on
	BookedAt time.Time `json:"booked_at"`
}

// testConsentDays is how long a Test consent lasts: the 90 days a UK account-information consent runs before the
// person confirms it again.
const testConsentDays = 90

// TestAccountInfoPartner is test mode for open banking: every bank has every account, in GBP, holding what was
// planted in it with Plant. A bank named with TESTFAIL refuses consent (see the package comment).
type TestAccountInfoPartner struct {
	mu       sync.Mutex
	consents map[string]*AccountConsent      // by ref
	planted  map[[2]string][]BankTransaction // bank, account → its transactions, oldest first
	ids      map[string]ConsentRequest       // by the caller's id, to answer a retry
}

// Name is "test".
func (*TestAccountInfoPartner) Name() string { return "test" }

// Plant puts transactions in bank's account, as the bank would book them.
func (p *TestAccountInfoPartner) Plant(bank, account string, txs ...BankTransaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.planted == nil {
		p.planted = map[[2]string][]BankTransaction{}
	}
	key := [2]string{bank, account}
	p.planted[key] = append(p.planted[key], txs...)
	slices.SortStableFunc(p.planted[key], func(a, b BankTransaction) int { return a.BookedAt.Compare(b.BookedAt) })
}

// Consent grants read access for testConsentDays, unless the bank's name contains TESTFAIL.
func (p *TestAccountInfoPartner) Consent(_ context.Context, req ConsentRequest) (AccountConsent, error) {
	if err := checkID(req.ID); err != nil {
		return AccountConsent{}, err
	}
	if strings.TrimSpace(req.Bank) == "" || strings.TrimSpace(req.Account) == "" {
		return AccountConsent{}, fmt.Errorf("%w: a consent names the bank and the account", ErrInvalid)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.ids[req.ID]; ok && prev != req {
		return AccountConsent{}, ErrIDReused
	}
	ref := testRef("consent", req.ID)
	if c, ok := p.consents[ref]; ok {
		return *c, nil
	}
	now, _, detail := testNameOutcome(req.Bank)
	c := &AccountConsent{Result: Result{Ref: ref, Status: now, Detail: detail}, Bank: req.Bank, Account: req.Account, Currency: "GBP",
		ExpiresAt: time.Now().UTC().AddDate(0, 0, testConsentDays)}
	if p.consents == nil {
		p.consents, p.ids = map[string]*AccountConsent{}, map[string]ConsentRequest{}
	}
	p.ids[req.ID] = req
	if now != StatusCompleted {
		return *c, nil
	}
	p.consents[ref] = c
	return *c, nil
}

func (p *TestAccountInfoPartner) consent(ref string) (*AccountConsent, error) {
	c, ok := p.consents[ref]
	if !ok {
		return nil, ErrNotFound
	}
	if !time.Now().UTC().Before(c.ExpiresAt) {
		return nil, fmt.Errorf("%w: the consent ran out on %s", ErrInvalid, c.ExpiresAt.Format(time.DateOnly))
	}
	return c, nil
}

// Renew starts the consent's testConsentDays again.
func (p *TestAccountInfoPartner) Renew(_ context.Context, ref string) (AccountConsent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.consents[ref]
	if !ok {
		return AccountConsent{}, ErrNotFound
	}
	c.ExpiresAt = time.Now().UTC().AddDate(0, 0, testConsentDays)
	return *c, nil
}

// Balance is what was planted in the account, summed.
func (p *TestAccountInfoPartner) Balance(_ context.Context, ref string) (BankBalance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, err := p.consent(ref)
	if err != nil {
		return BankBalance{}, err
	}
	b := BankBalance{Money: Money{Currency: c.Currency}, AsOf: time.Now().UTC()}
	for _, tx := range p.planted[[2]string{c.Bank, c.Account}] {
		b.Minor += tx.Amount.Minor
	}
	return b, nil
}

// Transactions is what was planted in the account, booked at or after since.
func (p *TestAccountInfoPartner) Transactions(_ context.Context, ref string, since time.Time) ([]BankTransaction, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, err := p.consent(ref)
	if err != nil {
		return nil, err
	}
	out := []BankTransaction{}
	for _, tx := range p.planted[[2]string{c.Bank, c.Account}] {
		if !tx.BookedAt.Before(since) {
			out = append(out, tx)
		}
	}
	return out, nil
}

// Revoke forgets the consent.
func (p *TestAccountInfoPartner) Revoke(_ context.Context, ref string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.consents[ref]; !ok {
		return ErrNotFound
	}
	delete(p.consents, ref)
	return nil
}
