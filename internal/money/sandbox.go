package money

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/talyvor/lens/internal/partners"
)

// SandboxNetwork is the chain the sandbox's USDC addresses are on.
const SandboxNetwork = "base"

// NewSandbox is the sandbox adapter: today's test-money partners in p (internal/partners' Test account, USDC and
// verification partners, the same ones every economy path asks), with the balances of the accounts and wallets it
// opened kept in memory. It moves no real money. Amounts answer as the Test partners' do: one whose minor units end
// in 13 is refused, 14 is paid and later returned, 15 stays pending. webhookSecret signs its webhooks; empty, it takes
// none.
//
// shortcut: balances live in memory and are gone when Lens restarts, as the Test partners' records are; the ledger
// (economy) is what keeps money. Upgrade when a sandbox balance must outlive a restart.
func NewSandbox(p *partners.Registry, webhookSecret string) *Adapter {
	s := &sandbox{account: p.TestAccount(), usdc: p.TestStablecoin(), kyc: p.TestKYC(),
		holdings: map[string]*holding{}, cards: map[string]*Card{}, done: map[string]done{}}
	return &Adapter{Name: SandboxName, Accounts: sandboxAccounts{s}, Wallets: sandboxWallets{s}, Transfers: sandboxTransfers{s},
		Cards: sandboxCards{s}, Payouts: sandboxPayouts{s}, KYC: sandboxKYC{s}, Webhooks: HMACWebhooks{Secret: webhookSecret}}
}

type sandbox struct {
	account partners.AccountPartner
	usdc    partners.StablecoinPartner
	kyc     partners.KYCProvider

	mu       sync.Mutex
	holdings map[string]*holding // accounts and wallets, by ref
	cards    map[string]*Card
	done     map[string]done // every write, by kind and key
}

// holding is an account or a wallet the sandbox opened.
type holding struct {
	asset   string
	balance int64
	closed  bool
	details Details
}

// done is a write that succeeded, so a retry with the same key answers the same.
type done struct {
	req any
	res any
}

// once runs fn for kind and key unless it succeeded before: the same request gets the first answer, a different one
// ErrKeyReused. The caller holds s.mu.
func once[T any](s *sandbox, kind, key string, req any, fn func() (T, error)) (T, error) {
	var zero T
	if err := checkKey(key); err != nil {
		return zero, err
	}
	if d, ok := s.done[kind+"\x00"+key]; ok {
		if !reflect.DeepEqual(d.req, req) {
			return zero, fmt.Errorf("%w: %s %q", ErrKeyReused, kind, key)
		}
		res, _ := d.res.(T)
		return res, nil
	}
	res, err := fn()
	if err == nil { // a refusal is asked again: the Test partners answer a retried id as they did the first time
		s.done[kind+"\x00"+key] = done{req, res}
	}
	return res, err
}

// fromPartner is a Test partner's error as the money layer's.
func fromPartner(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, partners.ErrInvalid):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case errors.Is(err, partners.ErrNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, partners.ErrIDReused):
		return fmt.Errorf("%w: %v", ErrKeyReused, err)
	}
	return fmt.Errorf("%w: %v", ErrPartnerDown, err)
}

func fromResult(r partners.Result) Result {
	return Result{Ref: r.Ref, Status: Status(r.Status), Detail: r.Detail}
}

func sandboxRef(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return "sbx_" + kind + "_" + hex.EncodeToString(sum[:8])
}

// open opens a holding in asset for holder: a Test partner account for pounds, euros and dollars, a Test address for
// USDC. The caller holds s.mu.
func (s *sandbox) open(ctx context.Context, key, holder, asset string) (Account, error) {
	if _, ok := assets[asset]; !ok {
		return Account{}, fmt.Errorf("%w: an asset is GBP, EUR, USD or USDC, not %q", ErrInvalid, asset)
	}
	if strings.TrimSpace(holder) == "" {
		return Account{}, fmt.Errorf("%w: an account names its holder", ErrInvalid)
	}
	if asset == USDC {
		addr, err := s.usdc.Address(ctx, partners.AddressRequest{ID: key, Network: SandboxNetwork})
		if err != nil {
			return Account{}, fromPartner(err)
		}
		ref := sandboxRef("usdc", key)
		s.holdings[ref] = &holding{asset: asset, details: Details{Asset: asset, Holder: holder, Network: addr.Network, Address: addr.Address}}
		return Account{Result: Result{Ref: ref, Status: StatusCompleted}, Asset: asset}, nil
	}
	a, err := s.account.OpenAccount(ctx, partners.AccountRequest{ID: key, Holder: holder, Currency: asset})
	if err != nil {
		return Account{}, fromPartner(err)
	}
	if a.Status == partners.StatusFailed {
		return Account{}, fmt.Errorf("%w: %s", ErrRefused, a.Detail)
	}
	d, err := s.account.AccountDetails(ctx, a.Ref)
	if err != nil && a.Status == partners.StatusCompleted {
		return Account{}, fromPartner(err)
	}
	s.holdings[a.Ref] = &holding{asset: asset, details: Details{Asset: asset, Holder: d.Holder, IBAN: d.IBAN, BIC: d.BIC,
		SortCode: d.SortCode, AccountNumber: d.AccountNumber, RoutingNumber: d.RoutingNumber}}
	return Account{Result: fromResult(a.Result), Asset: asset}, nil
}

// held is the open holding ref names. The caller holds s.mu.
func (s *sandbox) held(ref string) (*holding, error) {
	h, ok := s.holdings[ref]
	if !ok || h.closed {
		return nil, fmt.Errorf("%w: no open account or wallet %q", ErrNotFound, ref)
	}
	return h, nil
}

// debit checks that from holds amount in its asset. The caller holds s.mu.
func (s *sandbox) debitable(from string, amount Amount) (*holding, error) {
	h, err := s.held(from)
	if err != nil {
		return nil, err
	}
	if h.asset != amount.Asset {
		return nil, fmt.Errorf("%w: %s holds %s, not %s", ErrInvalid, from, h.asset, amount.Asset)
	}
	if h.balance < amount.Minor {
		return nil, fmt.Errorf("%w: %s holds %d, not %d", ErrInsufficientFunds, from, h.balance, amount.Minor)
	}
	return h, nil
}

// send pays amount out of from to a payee through the Test partners, and debits it unless the partner refused it.
// From an account in pounds, euros or dollars the sandbox did not open — one the economy opened at the Test account
// partner itself (B30.13), whose money the ledger keeps (B46.8) — the Test partner alone answers, and refuses an
// account it does not hold. The caller holds s.mu.
func (s *sandbox) send(ctx context.Context, key, from string, to Payee, amount Amount, reference string) (Result, error) {
	var h *holding
	var err error
	if _, ours := s.holdings[from]; ours || amount.Asset == USDC {
		if h, err = s.debitable(from, amount); err != nil {
			return Result{}, err
		}
	}
	var r partners.Result
	if amount.Asset == USDC {
		r, err = s.usdc.Send(ctx, partners.StablecoinTransfer{ID: key, Network: SandboxNetwork, To: to.Address,
			Amount: partners.Money{Minor: amount.Minor, Currency: amount.Asset}})
	} else {
		r, err = s.account.SendPayment(ctx, partners.PaymentRequest{ID: key, AccountRef: from, Reference: reference,
			Amount: partners.Money{Minor: amount.Minor, Currency: amount.Asset},
			Payee:  partners.Payee{Name: to.Name, IBAN: to.IBAN, BIC: to.BIC, SortCode: to.SortCode, AccountNumber: to.AccountNumber, RoutingNumber: to.RoutingNumber}})
	}
	if err != nil {
		return Result{}, fromPartner(err)
	}
	if r.Status == partners.StatusFailed {
		return Result{}, fmt.Errorf("%w: %s", ErrRefused, r.Detail)
	}
	if h != nil {
		h.balance -= amount.Minor
	}
	return fromResult(r), nil
}

type sandboxAccounts struct{ s *sandbox }

func (a sandboxAccounts) Open(ctx context.Context, req OpenAccount) (Account, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	return once(a.s, "account", req.Key, req, func() (Account, error) { return a.s.open(ctx, "account:"+req.Key, req.Holder, req.Asset) })
}

func (a sandboxAccounts) Close(_ context.Context, key, ref string) (Result, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	return once(a.s, "close", key, ref, func() (Result, error) {
		h, err := a.s.held(ref)
		if err != nil {
			return Result{}, err
		}
		if h.balance != 0 {
			return Result{}, fmt.Errorf("%w: an account still holding money is not closed", ErrRefused)
		}
		h.closed = true
		return Result{Ref: ref, Status: StatusCompleted}, nil
	})
}

func (a sandboxAccounts) Details(_ context.Context, ref string) (Details, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	h, err := a.s.held(ref)
	if err != nil {
		return Details{}, err
	}
	return h.details, nil
}

type sandboxWallets struct{ s *sandbox }

func (w sandboxWallets) Create(ctx context.Context, req CreateWallet) (Wallet, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return once(w.s, "wallet", req.Key, req, func() (Wallet, error) {
		a, err := w.s.open(ctx, "wallet:"+req.Key, req.AgentID, req.Asset)
		return Wallet{Ref: a.Ref, Asset: a.Asset}, err
	})
}

func (w sandboxWallets) Balance(_ context.Context, ref string) (Amount, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	h, err := w.s.held(ref)
	if err != nil {
		return Amount{}, err
	}
	return Amount{Minor: h.balance, Asset: h.asset}, nil
}

func (w sandboxWallets) Receive(ctx context.Context, ref string) (Details, error) {
	return sandboxAccounts(w).Details(ctx, ref)
}

type sandboxTransfers struct{ s *sandbox }

func (t sandboxTransfers) Internal(_ context.Context, req Internal) (Result, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return once(t.s, "internal", req.Key, req, func() (Result, error) {
		from, err := t.s.debitable(req.From, req.Amount)
		if err != nil {
			return Result{}, err
		}
		to, err := t.s.held(req.To)
		if err != nil {
			return Result{}, err
		}
		if to.asset != req.Amount.Asset || req.From == req.To {
			return Result{}, fmt.Errorf("%w: a transfer is between two holdings of its asset", ErrInvalid)
		}
		from.balance -= req.Amount.Minor
		to.balance += req.Amount.Minor
		return Result{Ref: sandboxRef("internal", req.Key), Status: StatusCompleted}, nil
	})
}

func (t sandboxTransfers) Outbound(ctx context.Context, req Outbound) (Result, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return once(t.s, "outbound", req.Key, req, func() (Result, error) {
		return t.s.send(ctx, "outbound:"+req.Key, req.From, req.Payee, req.Amount, req.Reference)
	})
}

func (t sandboxTransfers) Credit(_ context.Context, req Credit) (Result, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return once(t.s, "credit", req.Key, req, func() (Result, error) {
		h, err := t.s.held(req.To)
		if err != nil {
			return Result{}, err
		}
		if h.asset != req.Amount.Asset {
			return Result{}, fmt.Errorf("%w: %s holds %s, not %s", ErrInvalid, req.To, h.asset, req.Amount.Asset)
		}
		h.balance += req.Amount.Minor
		return Result{Ref: req.PartnerRef, Status: StatusCompleted}, nil
	})
}

type sandboxCards struct{ s *sandbox }

func (c sandboxCards) Issue(ctx context.Context, req IssueCard) (Card, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return once(c.s, "card", req.Key, req, func() (Card, error) {
		// A card on an account in pounds, euros or dollars the sandbox did not open — one the economy opened at the Test
		// account partner, whose money the ledger keeps and holds for the card (B46.10) — is issued once the Test partner
		// says it holds that account, as send pays out of one.
		if _, ours := c.s.holdings[req.Wallet]; ours || req.Cap.Asset == USDC {
			h, err := c.s.held(req.Wallet)
			if err != nil {
				return Card{}, err
			}
			if h.asset != req.Cap.Asset {
				return Card{}, fmt.Errorf("%w: a card's cap is in its wallet's asset, %s", ErrInvalid, h.asset)
			}
		} else if d, err := c.s.account.AccountDetails(ctx, req.Wallet); err != nil {
			return Card{}, fromPartner(err)
		} else if d.Currency != req.Cap.Asset {
			return Card{}, fmt.Errorf("%w: a card's cap is in its account's currency, %s", ErrInvalid, d.Currency)
		}
		ref := sandboxRef("card", req.Key)
		card := &Card{Ref: ref, Wallet: req.Wallet, Last4: ref[len(ref)-4:], Merchant: req.Merchant, Cap: req.Cap, State: CardActive}
		c.s.cards[ref] = card
		return *card, nil
	})
}

// card is the card ref names, unless it is closed. The caller holds s.mu.
func (c sandboxCards) card(ref string) (*Card, error) {
	card, ok := c.s.cards[ref]
	if !ok {
		return nil, fmt.Errorf("%w: no card %q", ErrNotFound, ref)
	}
	if card.State == CardClosed {
		return nil, fmt.Errorf("%w: the card is closed", ErrRefused)
	}
	return card, nil
}

func (c sandboxCards) Lock(_ context.Context, req LockCard) (Card, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return once(c.s, "lock", req.Key, req, func() (Card, error) {
		card, err := c.card(req.Ref)
		if err != nil {
			return Card{}, err
		}
		if card.Cap.Asset != req.Cap.Asset {
			return Card{}, fmt.Errorf("%w: a card's cap stays in %s", ErrInvalid, card.Cap.Asset)
		}
		card.Merchant, card.Cap = req.Merchant, req.Cap
		return *card, nil
	})
}

func (c sandboxCards) set(key, ref, kind, state string) (Card, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return once(c.s, kind, key, ref, func() (Card, error) {
		card, err := c.card(ref)
		if err != nil {
			return Card{}, err
		}
		card.State = state
		return *card, nil
	})
}

func (c sandboxCards) Freeze(_ context.Context, key, ref string) (Card, error) {
	return c.set(key, ref, "freeze", CardFrozen)
}

func (c sandboxCards) Close(_ context.Context, key, ref string) (Card, error) {
	return c.set(key, ref, "close_card", CardClosed)
}

type sandboxPayouts struct{ s *sandbox }

func (p sandboxPayouts) Pay(ctx context.Context, req Payout) (Result, error) {
	if req.Recipient != PayoutSeller && req.Recipient != PayoutRoomMember {
		return Result{}, fmt.Errorf("%w: a payout is to a seller or a Room member, not %q", ErrInvalid, req.Recipient)
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	return once(p.s, "payout", req.Key, req, func() (Result, error) {
		return p.s.send(ctx, "payout:"+req.Key, req.From, req.To, req.Amount, "Talyvor payout")
	})
}

type sandboxKYC struct{ s *sandbox }

func (k sandboxKYC) Start(ctx context.Context, req StartKYC) (Verification, error) {
	pr := partners.KYCRequest{ID: req.Key, Name: req.Name, Country: req.Country}
	switch req.Subject {
	case KYCPerson:
		pr.Subject = partners.KYCPerson
	case KYCCompany:
		// The partner's hosted page collects the number and the directors; the Test provider asks for them up front.
		pr.Subject, pr.CompanyNumber, pr.Directors = partners.KYCCompany, "SANDBOX", []string{req.Name}
	default:
		return Verification{}, fmt.Errorf("%w: a verification is of a person or a company, not %q", ErrInvalid, req.Subject)
	}
	r, err := k.s.kyc.StartCheck(ctx, pr)
	if err != nil {
		return Verification{}, fromPartner(err)
	}
	return Verification{Result: fromResult(r), Link: r.Link}, nil
}

func (k sandboxKYC) Status(ctx context.Context, ref string) (Verification, error) {
	r, err := k.s.kyc.CheckResult(ctx, ref)
	if err != nil {
		return Verification{}, fromPartner(err)
	}
	return Verification{Result: fromResult(r), Link: r.Link}, nil
}
