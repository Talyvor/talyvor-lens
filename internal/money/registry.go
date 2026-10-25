package money

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/talyvor/lens/internal/partners"
)

// SandboxName is the sandbox adapter's name.
const SandboxName = "sandbox"

// Adapter is one partner's answers to the capabilities it offers; a nil field is a capability it does not offer.
type Adapter struct {
	Name      string
	Accounts  Accounts
	Wallets   Wallets
	Transfers Transfers
	Cards     Cards
	Payouts   Payouts
	KYC       KYC
	Webhooks  Webhooks // verifies the partner's notifications; nil takes none
}

func (a *Adapter) offers(c Capability) bool {
	switch c {
	case CapAccounts:
		return a.Accounts != nil
	case CapWallets:
		return a.Wallets != nil
	case CapTransfers:
		return a.Transfers != nil
	case CapCards:
		return a.Cards != nil
	case CapPayouts:
		return a.Payouts != nil
	case CapKYC:
		return a.KYC != nil
	}
	return false
}

// Switch is one capability in one country with one partner. On, that partner serves it there with real money.
type Switch struct {
	Capability Capability
	Country    string
	Partner    string
}

// Switches says whether a switch is on.
type Switches interface {
	On(ctx context.Context, s Switch) (bool, error)
}

// SwitchSet is the switches that are on. Its zero value has every switch off, as every switch is today.
type SwitchSet map[Switch]bool

// On is whether s is in the set.
func (s SwitchSet) On(_ context.Context, sw Switch) (bool, error) { return s[sw], nil }

// Route is who a call is for and where: the workspace, the agent if it is one's, the country the money moves in, and
// the wallet capability (economy.Capability*) it moves for. A live adapter serves it only once that capability is
// cleared for real money as well as switched on.
type Route struct {
	Workspace  string `json:"workspace_id"`
	Agent      string `json:"agent_id,omitempty"`
	Country    string `json:"country"`
	Capability string `json:"capability"`
}

// Op is one call, as the owner's limits judge it and the audit trail records it.
type Op struct {
	Route
	Partner    string
	Capability Capability
	Method     string
	Key        string // the caller's idempotency key; "" for a read
	Ref        string // the partner's reference the call is about, when it is about one
	Amount     Amount // what leaves, for a call that moves money out
	Payee      string // who it goes to, for money out
}

// Limits is the owner's limits and one-tap approval, asked before any call that moves money out: nil lets it go,
// ErrLimit refuses it, ErrApprovalNeeded holds it for the owner. The adapter never decides them.
type Limits interface {
	Allow(ctx context.Context, op Op) error
}

// Audit appends to the hash-chained audit trail (economy.DualTokenStore.AppendAudit).
type Audit interface {
	AppendAudit(ctx context.Context, workspaceID, subject, kind string, body map[string]any) error
}

// Config is what a Registry is built from. Sandbox, Limits and Audit are required; nil Switches is every switch off,
// and nil Clearances is nothing cleared.
type Config struct {
	Sandbox    *Adapter
	Switches   Switches
	Clearances partners.Clearances
	Limits     Limits
	Audit      Audit
	Ledger     Ledger  // where a verified webhook's money is posted
	Replays    Replays // the webhook ids already taken
}

// Registry routes every call to the adapter switched on for it, or the sandbox.
type Registry struct {
	cfg Config

	mu       sync.RWMutex
	adapters []*Adapter // live adapters, in the order they were registered
}

// NewRegistry is a registry with only the sandbox until adapters are registered.
func NewRegistry(cfg Config) (*Registry, error) {
	switch {
	case cfg.Sandbox == nil:
		return nil, errors.New("money: a registry needs the sandbox adapter")
	case cfg.Limits == nil:
		return nil, errors.New("money: a registry needs the owner's limits, asked before money moves")
	case cfg.Audit == nil:
		return nil, errors.New("money: a registry needs the audit trail")
	}
	if cfg.Switches == nil {
		cfg.Switches = SwitchSet{}
	}
	return &Registry{cfg: cfg}, nil
}

// Register adds a partner's adapter. It serves nothing until a switch for it is on.
func (r *Registry) Register(a *Adapter) error {
	if a == nil || a.Name == "" || a.Name == SandboxName {
		return errors.New("money: an adapter has a name of its own")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.adapters {
		if b.Name == a.Name {
			return fmt.Errorf("money: an adapter called %q is already registered", a.Name)
		}
	}
	r.adapters = append(r.adapters, a)
	return nil
}

// adapter is the adapter named name, the sandbox included.
func (r *Registry) adapter(name string) *Adapter {
	if name == SandboxName {
		return r.cfg.Sandbox
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.adapters {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// pick is the first registered adapter offering c whose switch is on for rt's country and whose wallet capability is
// cleared, and the sandbox when there is none. It is read before every call.
func (r *Registry) pick(ctx context.Context, c Capability, rt Route) (*Adapter, error) {
	if rt.Workspace == "" {
		return nil, fmt.Errorf("%w: every call names its workspace", ErrInvalid)
	}
	r.mu.RLock()
	live := append([]*Adapter(nil), r.adapters...)
	r.mu.RUnlock()
	for _, a := range live {
		if !a.offers(c) {
			continue
		}
		on, err := r.cfg.Switches.On(ctx, Switch{Capability: c, Country: rt.Country, Partner: a.Name})
		if err != nil {
			return nil, fmt.Errorf("money: is %s switched on for %s in %q: %w", a.Name, c, rt.Country, err)
		}
		if !on || r.cfg.Clearances == nil {
			continue
		}
		cleared, err := r.cfg.Clearances.CapabilityCleared(ctx, rt.Capability)
		if err != nil {
			return nil, fmt.Errorf("money: is %s cleared: %w", rt.Capability, err)
		}
		if cleared {
			return a, nil
		}
	}
	if !r.cfg.Sandbox.offers(c) {
		return nil, fmt.Errorf("%w: no adapter offers %s", ErrInvalid, c)
	}
	return r.cfg.Sandbox, nil
}

// Partner is the name of the adapter that serves c on rt now.
func (r *Registry) Partner(ctx context.Context, c Capability, rt Route) (string, error) {
	a, err := r.pick(ctx, c, rt)
	if err != nil {
		return "", err
	}
	return a.Name, nil
}

// Accounts is the accounts capability for rt.
func (r *Registry) Accounts(ctx context.Context, rt Route) (Accounts, error) {
	a, err := r.pick(ctx, CapAccounts, rt)
	if err != nil {
		return nil, err
	}
	return gatedAccounts{r.gate(a, rt, CapAccounts), a.Accounts}, nil
}

// Wallets is the wallets capability for rt.
func (r *Registry) Wallets(ctx context.Context, rt Route) (Wallets, error) {
	a, err := r.pick(ctx, CapWallets, rt)
	if err != nil {
		return nil, err
	}
	return gatedWallets{r.gate(a, rt, CapWallets), a.Wallets}, nil
}

// Transfers is the transfers capability for rt.
func (r *Registry) Transfers(ctx context.Context, rt Route) (Transfers, error) {
	a, err := r.pick(ctx, CapTransfers, rt)
	if err != nil {
		return nil, err
	}
	return gatedTransfers{r.gate(a, rt, CapTransfers), a.Transfers}, nil
}

// Cards is the cards capability for rt.
func (r *Registry) Cards(ctx context.Context, rt Route) (Cards, error) {
	a, err := r.pick(ctx, CapCards, rt)
	if err != nil {
		return nil, err
	}
	return gatedCards{r.gate(a, rt, CapCards), a.Cards}, nil
}

// Payouts is the payouts capability for rt.
func (r *Registry) Payouts(ctx context.Context, rt Route) (Payouts, error) {
	a, err := r.pick(ctx, CapPayouts, rt)
	if err != nil {
		return nil, err
	}
	return gatedPayouts{r.gate(a, rt, CapPayouts), a.Payouts}, nil
}

// KYC is the verification capability for rt.
func (r *Registry) KYC(ctx context.Context, rt Route) (KYC, error) {
	a, err := r.pick(ctx, CapKYC, rt)
	if err != nil {
		return nil, err
	}
	return gatedKYC{r.gate(a, rt, CapKYC), a.KYC}, nil
}
