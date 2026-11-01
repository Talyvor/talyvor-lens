package moneytest

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/money"
)

// Decide is Lens answering a card partner's authorisation request (issuerfake.Decide).
type Decide = func(ctx context.Context, partner string, a money.Authorization) (approved bool, reason string, err error)

// CardPartner is a card partner's fake as the contract drives it (B53.10): a person adding a card to their phone's
// wallet, a merchant charging the card or one of its tokens, and the token events the partner sent Lens.
type CardPartner interface {
	AddToWallet(ctx context.Context, ref, wallet string) (money.WalletToken, error)
	Charge(ctx context.Context, ref, merchant string, amount money.Amount) (bool, string, error)
	ChargeToken(ctx context.Context, token, merchant string, amount money.Amount) (bool, string, error)
	TokenEvents() []money.TokenEvent
}

// cardEnv is one card case's issuer and partner, and every authorisation Lens was asked.
type cardEnv struct {
	cards   money.CardIssuer
	partner CardPartner
	asset   string

	mu    sync.Mutex
	asked []money.Authorization
	seen  []any // everything the partner answered, for the card-number check
}

func (e *cardEnv) keep(v any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, v)
}

func (e *cardEnv) card(t *testing.T, key, formFactor string) money.Card {
	t.Helper()
	c := must(e.cards.Issue(ctx, money.IssueCard{Key: key, Wallet: "wallet-" + key, Cap: money.Amount{Minor: 100_00, Asset: e.asset}, FormFactor: formFactor}))(t)
	e.keep(c)
	return c
}

func (e *cardEnv) state(t *testing.T, want string, fn func() (money.Card, error)) money.Card {
	t.Helper()
	c := must(fn())(t)
	e.keep(c)
	if c.State != want {
		t.Fatalf("card = %+v, want %s", c, want)
	}
	return c
}

func (e *cardEnv) token(t *testing.T, want string, fn func() (money.WalletToken, error)) money.WalletToken {
	t.Helper()
	tok := must(fn())(t)
	e.keep(tok)
	if tok.State != want {
		t.Fatalf("token = %+v, want %s", tok, want)
	}
	return tok
}

// charge is a merchant charging 5.00 through the card, or through token when it is not "", and whether Lens was asked.
func (e *cardEnv) charge(t *testing.T, ref, token string) (approved, asked bool) {
	t.Helper()
	e.mu.Lock()
	before := len(e.asked)
	e.mu.Unlock()
	amount := money.Amount{Minor: 5_00, Asset: e.asset}
	var err error
	if token == "" {
		approved, _, err = e.partner.Charge(ctx, ref, "Tesco", amount)
	} else {
		approved, _, err = e.partner.ChargeToken(ctx, token, "Tesco", amount)
	}
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return approved, len(e.asked) > before
}

func (e *cardEnv) events(tok money.WalletToken) []string {
	var kinds []string
	for _, ev := range e.partner.TokenEvents() {
		if ev.Token.Ref == tok.Ref {
			kinds = append(kinds, ev.Kind)
		}
	}
	return kinds
}

func wantKinds(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("token events %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("token events %v, want %v", got, want)
		}
	}
}

var cardContract = []struct {
	name string
	run  func(t *testing.T, e *cardEnv)
}{
	{"a reusable card pays again, freezes, unfreezes and closes", func(t *testing.T, e *cardEnv) {
		c := e.card(t, "ru-1", "")
		if c.Ref == "" || c.State != money.CardActive || c.Last4 == "" || c.Merchant != "" || c.FormFactor != money.CardVirtual {
			t.Fatalf("card = %+v", c)
		}
		if again := e.card(t, "ru-1", ""); again.Ref != c.Ref {
			t.Fatalf("a retry issued %q then %q", c.Ref, again.Ref)
		}
		for range 2 {
			if approved, asked := e.charge(t, c.Ref, ""); !approved || !asked {
				t.Fatalf("a charge on a reusable card: approved %v, Lens asked %v", approved, asked)
			}
		}
		e.state(t, money.CardFrozen, func() (money.Card, error) { return e.cards.Freeze(ctx, "ru-1-freeze", c.Ref) })
		_, err := e.partner.AddToWallet(ctx, c.Ref, money.WalletApplePay)
		wantErr(t, err, money.ErrRefused)
		e.state(t, money.CardActive, func() (money.Card, error) { return e.cards.Unfreeze(ctx, "ru-1-unfreeze", c.Ref) })
		e.state(t, money.CardClosed, func() (money.Card, error) { return e.cards.Close(ctx, "ru-1-close", c.Ref) })
		_, err = e.cards.Unfreeze(ctx, "ru-1-unfreeze-2", c.Ref)
		wantErr(t, err, money.ErrRefused)
	}},
	{"a physical card waits to be activated", func(t *testing.T, e *cardEnv) {
		c := e.card(t, "ph-1", money.CardPhysical)
		if c.State != money.CardInactive || c.FormFactor != money.CardPhysical {
			t.Fatalf("physical card = %+v", c)
		}
		_, err := e.partner.AddToWallet(ctx, c.Ref, money.WalletGooglePay)
		wantErr(t, err, money.ErrRefused)
		e.state(t, money.CardActive, func() (money.Card, error) { return e.cards.Activate(ctx, "ph-1-activate", c.Ref) })
	}},
	{"a phone-wallet token is requested, activated, suspended, resumed and deleted", func(t *testing.T, e *cardEnv) {
		c := e.card(t, "tok-1", "")
		tok := e.token(t, money.TokenActive, func() (money.WalletToken, error) { return e.partner.AddToWallet(ctx, c.Ref, money.WalletApplePay) })
		if tok.Ref == "" || tok.Card != c.Ref || tok.Wallet != money.WalletApplePay {
			t.Fatalf("token = %+v", tok)
		}
		wantKinds(t, e.events(tok), money.TokenEventRequested, money.TokenEventActivated)
		if approved, asked := e.charge(t, c.Ref, tok.Ref); !approved || !asked {
			t.Fatalf("a charge through an active token: approved %v, Lens asked %v", approved, asked)
		}
		e.mu.Lock()
		a := e.asked[len(e.asked)-1]
		e.mu.Unlock()
		if a.Card != c.Ref || a.Token != tok.Ref || a.Wallet != money.WalletApplePay {
			t.Fatalf("Lens was asked %+v, want the card, its token and apple_pay", a)
		}

		e.token(t, money.TokenSuspended, func() (money.WalletToken, error) { return e.cards.SuspendToken(ctx, "tok-1-suspend", tok.Ref) })
		if approved, asked := e.charge(t, c.Ref, tok.Ref); approved || asked {
			t.Fatalf("a charge through a suspended token: approved %v, Lens asked %v", approved, asked)
		}
		if approved, _ := e.charge(t, c.Ref, ""); !approved {
			t.Fatal("suspending a token stopped the card itself")
		}
		e.token(t, money.TokenActive, func() (money.WalletToken, error) { return e.cards.ResumeToken(ctx, "tok-1-resume", tok.Ref) })

		e.token(t, money.TokenDeleted, func() (money.WalletToken, error) { return e.cards.DeleteToken(ctx, "tok-1-delete", tok.Ref) })
		if approved, asked := e.charge(t, c.Ref, tok.Ref); approved || asked {
			t.Fatalf("a charge through a deleted token: approved %v, Lens asked %v", approved, asked)
		}
		_, err := e.cards.ResumeToken(ctx, "tok-1-resume-2", tok.Ref)
		wantErr(t, err, money.ErrRefused)
		wantKinds(t, e.events(tok), money.TokenEventRequested, money.TokenEventActivated, money.TokenEventSuspended,
			money.TokenEventActivated, money.TokenEventDeleted)
		toks := must(e.cards.Tokens(ctx, c.Ref))(t)
		e.keep(toks)
		if len(toks) != 1 || toks[0].State != money.TokenDeleted {
			t.Fatalf("tokens = %+v", toks)
		}
	}},
	{"closing a card deletes its tokens", func(t *testing.T, e *cardEnv) {
		c := e.card(t, "tok-2", "")
		g := e.token(t, money.TokenActive, func() (money.WalletToken, error) { return e.partner.AddToWallet(ctx, c.Ref, money.WalletGooglePay) })
		s := e.token(t, money.TokenActive, func() (money.WalletToken, error) { return e.partner.AddToWallet(ctx, c.Ref, money.WalletSamsungPay) })
		e.state(t, money.CardClosed, func() (money.Card, error) { return e.cards.Close(ctx, "tok-2-close", c.Ref) })
		for _, tok := range []money.WalletToken{g, s} {
			wantKinds(t, e.events(tok), money.TokenEventRequested, money.TokenEventActivated, money.TokenEventDeleted)
			if approved, asked := e.charge(t, c.Ref, tok.Ref); approved || asked {
				t.Fatalf("a charge through a closed card's token: approved %v, Lens asked %v", approved, asked)
			}
		}
	}},
	{"an unknown token or card is not found", func(t *testing.T, e *cardEnv) {
		_, err := e.cards.SuspendToken(ctx, "nt-1", "no-such-token")
		wantErr(t, err, money.ErrNotFound)
		_, err = e.cards.Unfreeze(ctx, "nt-2", "no-such-card")
		wantErr(t, err, money.ErrNotFound)
		_, err = e.partner.AddToWallet(ctx, "no-such-card", money.WalletApplePay)
		wantErr(t, err, money.ErrNotFound)
	}},
	{"a token is for a phone wallet", func(t *testing.T, e *cardEnv) {
		c := e.card(t, "tok-3", "")
		_, err := e.partner.AddToWallet(ctx, c.Ref, money.WalletNone)
		wantErr(t, err, money.ErrInvalid)
	}},
}

// cardNumber is a run of digits as long as the shortest card number.
var cardNumber = regexp.MustCompile(`\d{12,}`)

// CardContract runs every card-partner case (B53.10) on a fresh issuer and its fake from newPartner, which asks decide
// about every charge Lens is asked about; here Lens approves every charge, so a decline is the partner's own. After each
// case, nothing the partner answered — cards, tokens, token events, authorisation requests — carries a card number.
func CardContract(t *testing.T, newPartner func(decide Decide) (money.CardIssuer, CardPartner), asset string) {
	for _, cc := range cardContract {
		t.Run("card_issuer/"+asset+"/"+cc.name, func(t *testing.T) {
			e := &cardEnv{asset: asset}
			e.cards, e.partner = newPartner(func(_ context.Context, _ string, a money.Authorization) (bool, string, error) {
				e.mu.Lock()
				defer e.mu.Unlock()
				e.asked = append(e.asked, a)
				return true, "", nil
			})
			cc.run(t, e)
			b, err := json.Marshal([]any{e.seen, e.asked, e.partner.TokenEvents()})
			if err != nil {
				t.Fatal(err)
			}
			if n := cardNumber.Find(b); n != nil {
				t.Fatalf("the partner answered something that reads as a card number (%d digits)", len(n))
			}
		})
	}
}
