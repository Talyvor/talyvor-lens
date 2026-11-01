// Package issuerfake is a local card issuer for tests (B46.10): it issues, freezes and closes cards as money.Cards,
// and a merchant's charge on one of its cards is asked of Lens in real time, as a real issuer's authorisation request
// is, and approved or declined as Lens answers. It is a whole money.CardIssuer too (B53.10) — reusable and physical
// cards, and phone-wallet tokens with their events — and passes the contract every real card adapter must
// (moneytest.CardContract). It never calls a partner. The card-issuer adapter itself waits for a signed issuer.
package issuerfake

import (
	"context"
	"fmt"
	"slices"
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

	mu     sync.Mutex
	cards  map[string]*money.Card
	tokens map[string]*money.WalletToken
	order  []string // token refs, oldest first
	events []money.TokenEvent
	keys   map[string]any // what each idempotency key answered
	auths  int
}

// New is an issuer that asks decide about every charge.
func New(decide Decide) *Issuer {
	return &Issuer{decide: decide, cards: map[string]*money.Card{}, tokens: map[string]*money.WalletToken{}, keys: map[string]any{}}
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
	a := i.authorization(ref, merchant, amount)
	i.mu.Unlock()
	return i.decide(ctx, Name, a)
}

// ChargeToken is merchant charging amount through a phone-wallet token: a token that is not active is declined here,
// as the card network's token service does, and Lens is not asked; an active one's charge is asked of Lens with the
// token and its wallet, contactless.
func (i *Issuer) ChargeToken(ctx context.Context, token, merchant string, amount money.Amount) (bool, string, error) {
	i.mu.Lock()
	t, ok := i.tokens[token]
	if !ok || t.State != money.TokenActive {
		i.mu.Unlock()
		if !ok {
			return false, "no such phone-wallet token", nil
		}
		return false, "the phone-wallet token is " + t.State, nil
	}
	a := i.authorization(t.Card, merchant, amount)
	a.Token, a.Wallet, a.Channel = t.Ref, t.Wallet, money.ChannelContactless
	i.mu.Unlock()
	return i.decide(ctx, Name, a)
}

// authorization is a new authorisation request for a charge on card ref. The caller holds i.mu.
func (i *Issuer) authorization(ref, merchant string, amount money.Amount) money.Authorization {
	i.auths++
	return money.Authorization{ID: "iauth_" + strconv.Itoa(i.auths), Card: ref, Merchant: merchant, Amount: amount, CardDetails: Details(merchant)}
}

// once answers what key answered the first time, or fn's answer, kept under key unless it failed.
func once[T any](i *Issuer, key string, fn func() (T, error)) (T, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if v, ok := i.keys[key]; ok {
		return v.(T), nil
	}
	v, err := fn()
	if err == nil {
		i.keys[key] = v
	}
	return v, err
}

// Issue issues a card locked to req's merchant ("" is any merchant, a reusable card) and capped: virtual and active,
// or physical and inactive until it arrives and is activated.
func (i *Issuer) Issue(_ context.Context, req money.IssueCard) (money.Card, error) {
	return once(i, "issue:"+req.Key, func() (money.Card, error) {
		ref := fmt.Sprintf("ic_%04d", len(i.cards)+1)
		c := &money.Card{Ref: ref, Wallet: req.Wallet, Last4: ref[len(ref)-4:], Merchant: req.Merchant, Cap: req.Cap, State: money.CardActive,
			FormFactor: money.CardVirtual}
		if req.FormFactor == money.CardPhysical {
			c.State, c.FormFactor = money.CardInactive, money.CardPhysical
		}
		i.cards[ref] = c
		return *c, nil
	})
}

// card is the card ref names, unless it is closed. The caller holds i.mu.
func (i *Issuer) card(ref string) (*money.Card, error) {
	c, ok := i.cards[ref]
	if !ok {
		return nil, fmt.Errorf("%w: no card %q", money.ErrNotFound, ref)
	}
	if c.State == money.CardClosed {
		return nil, fmt.Errorf("%w: the card is closed", money.ErrRefused)
	}
	return c, nil
}

// Lock locks a card to another merchant and cap.
func (i *Issuer) Lock(_ context.Context, req money.LockCard) (money.Card, error) {
	return once(i, "lock:"+req.Key, func() (money.Card, error) {
		c, err := i.card(req.Ref)
		if err != nil {
			return money.Card{}, err
		}
		c.Merchant, c.Cap = req.Merchant, req.Cap
		return *c, nil
	})
}

// set moves card ref to state, if it is in one of from ("" from any).
func (i *Issuer) set(key, ref, state string, from ...string) (money.Card, error) {
	return once(i, state+":"+key, func() (money.Card, error) {
		c, err := i.card(ref)
		if err != nil {
			return money.Card{}, err
		}
		if len(from) > 0 && !slices.Contains(from, c.State) && c.State != state {
			return money.Card{}, fmt.Errorf("%w: the card is %s", money.ErrRefused, c.State)
		}
		c.State = state
		if state == money.CardClosed {
			for _, tr := range i.order {
				if t := i.tokens[tr]; t.Card == c.Ref && t.State != money.TokenDeleted {
					i.token(t, money.TokenDeleted, money.TokenEventDeleted)
				}
			}
		}
		return *c, nil
	})
}

// Freeze freezes a card.
func (i *Issuer) Freeze(_ context.Context, key, ref string) (money.Card, error) {
	return i.set(key, ref, money.CardFrozen)
}

// Unfreeze makes a frozen card active again.
func (i *Issuer) Unfreeze(_ context.Context, key, ref string) (money.Card, error) {
	return i.set("unfreeze:"+key, ref, money.CardActive, money.CardFrozen)
}

// Activate makes a physical card that has arrived active.
func (i *Issuer) Activate(_ context.Context, key, ref string) (money.Card, error) {
	return i.set("activate:"+key, ref, money.CardActive, money.CardInactive)
}

// Close closes a card for good, and deletes its phone-wallet tokens.
func (i *Issuer) Close(_ context.Context, key, ref string) (money.Card, error) {
	return i.set(key, ref, money.CardClosed)
}

// token moves t to state and sends Lens kind. The caller holds i.mu.
func (i *Issuer) token(t *money.WalletToken, state, kind string) {
	t.State = state
	i.events = append(i.events, money.TokenEvent{ID: fmt.Sprintf("itev_%04d", len(i.events)+1), Kind: kind, Token: *t})
}

// AddToWallet is the person adding card ref to their phone's wallet: the token is requested and, the card being
// active, activated.
func (i *Issuer) AddToWallet(_ context.Context, ref, wallet string) (money.WalletToken, error) {
	if wallet != money.WalletApplePay && wallet != money.WalletGooglePay && wallet != money.WalletSamsungPay {
		return money.WalletToken{}, fmt.Errorf("%w: a token is for Apple Pay, Google Pay or Samsung Pay, not %q", money.ErrInvalid, wallet)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	c, err := i.card(ref)
	if err != nil {
		return money.WalletToken{}, err
	}
	if c.State != money.CardActive {
		return money.WalletToken{}, fmt.Errorf("%w: the card is %s", money.ErrRefused, c.State)
	}
	t := &money.WalletToken{Ref: fmt.Sprintf("itok_%04d", len(i.tokens)+1), Card: ref, Wallet: wallet}
	i.tokens[t.Ref], i.order = t, append(i.order, t.Ref)
	i.token(t, money.TokenRequested, money.TokenEventRequested)
	i.token(t, money.TokenActive, money.TokenEventActivated)
	return *t, nil
}

// Tokens is card ref's phone-wallet tokens, oldest first.
func (i *Issuer) Tokens(_ context.Context, ref string) ([]money.WalletToken, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.cards[ref]; !ok {
		return nil, fmt.Errorf("%w: no card %q", money.ErrNotFound, ref)
	}
	var out []money.WalletToken
	for _, tr := range i.order {
		if t := i.tokens[tr]; t.Card == ref {
			out = append(out, *t)
		}
	}
	return out, nil
}

// setToken moves token to state, sending Lens kind, if it is in from; a deleted token is refused.
func (i *Issuer) setToken(key, token, state, kind, from string) (money.WalletToken, error) {
	return once(i, kind+":"+key, func() (money.WalletToken, error) {
		t, ok := i.tokens[token]
		switch {
		case !ok:
			return money.WalletToken{}, fmt.Errorf("%w: no token %q", money.ErrNotFound, token)
		case t.State == state:
			return *t, nil
		case t.State != from && from != "":
			return money.WalletToken{}, fmt.Errorf("%w: the token is %s", money.ErrRefused, t.State)
		case t.State == money.TokenDeleted:
			return money.WalletToken{}, fmt.Errorf("%w: the token is deleted", money.ErrRefused)
		}
		i.token(t, state, kind)
		return *t, nil
	})
}

// SuspendToken suspends an active token: a charge through it is declined until it is resumed.
func (i *Issuer) SuspendToken(_ context.Context, key, token string) (money.WalletToken, error) {
	return i.setToken(key, token, money.TokenSuspended, money.TokenEventSuspended, money.TokenActive)
}

// ResumeToken makes a suspended token active again.
func (i *Issuer) ResumeToken(_ context.Context, key, token string) (money.WalletToken, error) {
	return i.setToken(key, token, money.TokenActive, money.TokenEventActivated, money.TokenSuspended)
}

// DeleteToken deletes a token for good.
func (i *Issuer) DeleteToken(_ context.Context, key, token string) (money.WalletToken, error) {
	return i.setToken(key, token, money.TokenDeleted, money.TokenEventDeleted, "")
}

// TokenEvents is every token event the issuer has sent Lens, oldest first.
func (i *Issuer) TokenEvents() []money.TokenEvent {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.events)
}
