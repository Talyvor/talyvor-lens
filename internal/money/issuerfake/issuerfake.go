// Package issuerfake is a local card issuer for tests (B46.10): it issues, freezes and closes cards as money.Cards,
// and a merchant's charge on one of its cards is asked of Lens in real time, as a real issuer's authorisation request
// is, and approved or declined as Lens answers. It never calls a partner. The card-issuer adapter itself waits for a
// signed issuer.
package issuerfake

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/talyvor/lens/internal/money"
)

// Name is the fake issuer's adapter name.
const Name = "issuerfake"

// Decide is Lens's answer to an authorisation request: approved, or declined with the reason.
type Decide func(ctx context.Context, partner string, a money.Authorization) (approved bool, reason string, err error)

// Issuer is the fake issuer.
type Issuer struct {
	decide Decide

	mu    sync.Mutex
	cards map[string]*money.Card
	keys  map[string]money.Card // what each idempotency key answered
	auths int
}

// New is an issuer that asks decide about every charge.
func New(decide Decide) *Issuer {
	return &Issuer{decide: decide, cards: map[string]*money.Card{}, keys: map[string]money.Card{}}
}

// Adapter is the issuer as a money-layer adapter offering cards.
func (i *Issuer) Adapter() *money.Adapter { return &money.Adapter{Name: Name, Cards: i} }

// Card is the card ref as the issuer holds it now.
func (i *Issuer) Card(ref string) (money.Card, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	c, ok := i.cards[ref]
	if !ok {
		return money.Card{}, false
	}
	return *c, true
}

// merchants is how a real issuer would report a charge at each of these merchants (B53.1): its category code, country,
// channel and phone wallet. Any other merchant reads as an online charge in GB under 5999, miscellaneous retail.
var merchants = map[string]money.CardDetails{
	"pret a manger":   {MCC: "5814", Country: "GB", Channel: money.ChannelContactless, Wallet: money.WalletApplePay},
	"tesco":           {MCC: "5411", Country: "GB", Channel: money.ChannelContactless, Wallet: money.WalletGooglePay},
	"shell":           {MCC: "5541", Country: "GB", Channel: money.ChannelInPerson, Wallet: money.WalletNone},
	"tfl travel":      {MCC: "4111", Country: "GB", Channel: money.ChannelContactless, Wallet: money.WalletSamsungPay},
	"british airways": {MCC: "3009", Country: "GB", Channel: money.ChannelOnline, Wallet: money.WalletNone},
	"marriott lisbon": {MCC: "3509", Country: "PT", Channel: money.ChannelInPerson, Wallet: money.WalletApplePay},
	"hertz":           {MCC: "3357", Country: "PT", Channel: money.ChannelOnline, Wallet: money.WalletNone},
	"openai":          {MCC: "5734", Country: "US", Channel: money.ChannelOnline, Wallet: money.WalletNone},
	"aws":             {MCC: "7372", Country: "US", Channel: money.ChannelOnline, Wallet: money.WalletNone},
	"william hill":    {MCC: "7995", Country: "GB", Channel: money.ChannelOnline, Wallet: money.WalletNone}, // gambling (B53.2)
}

// Details is what the issuer reports about a charge at merchant.
func Details(merchant string) money.CardDetails {
	if d, ok := merchants[strings.ToLower(strings.Join(strings.Fields(merchant), " "))]; ok {
		return d
	}
	return money.CardDetails{MCC: "5999", Country: "GB", Channel: money.ChannelOnline, Wallet: money.WalletNone}
}

// Charge is merchant charging amount to the card ref: the issuer asks Lens and approves the charge only if Lens does.
// Every charge is asked, whatever state the issuer holds the card in, so Lens's answer alone decides it.
func (i *Issuer) Charge(ctx context.Context, ref, merchant string, amount money.Amount) (bool, string, error) {
	i.mu.Lock()
	i.auths++
	a := money.Authorization{ID: "iauth_" + strconv.Itoa(i.auths), Card: ref, Merchant: merchant, Amount: amount, CardDetails: Details(merchant)}
	i.mu.Unlock()
	return i.decide(ctx, Name, a)
}

func (i *Issuer) once(key string, fn func() (money.Card, error)) (money.Card, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if c, ok := i.keys[key]; ok {
		return c, nil
	}
	c, err := fn()
	if err == nil {
		i.keys[key] = c
	}
	return c, err
}

// Issue issues an active card locked to req's merchant and capped.
func (i *Issuer) Issue(_ context.Context, req money.IssueCard) (money.Card, error) {
	return i.once("issue:"+req.Key, func() (money.Card, error) {
		ref := fmt.Sprintf("ic_%04d", len(i.cards)+1)
		c := &money.Card{Ref: ref, Wallet: req.Wallet, Last4: ref[len(ref)-4:], Merchant: req.Merchant, Cap: req.Cap, State: money.CardActive,
			FormFactor: money.CardVirtual}
		i.cards[ref] = c
		return *c, nil
	})
}

// Lock locks a card to another merchant and cap.
func (i *Issuer) Lock(_ context.Context, req money.LockCard) (money.Card, error) {
	return i.once("lock:"+req.Key, func() (money.Card, error) {
		c, ok := i.cards[req.Ref]
		if !ok {
			return money.Card{}, money.ErrNotFound
		}
		c.Merchant, c.Cap = req.Merchant, req.Cap
		return *c, nil
	})
}

func (i *Issuer) set(key, ref, state string) (money.Card, error) {
	return i.once(state+":"+key, func() (money.Card, error) {
		c, ok := i.cards[ref]
		if !ok {
			return money.Card{}, money.ErrNotFound
		}
		c.State = state
		return *c, nil
	})
}

// Freeze freezes a card.
func (i *Issuer) Freeze(_ context.Context, key, ref string) (money.Card, error) {
	return i.set(key, ref, money.CardFrozen)
}

// Close closes a card for good.
func (i *Issuer) Close(_ context.Context, key, ref string) (money.Card, error) {
	return i.set(key, ref, money.CardClosed)
}
