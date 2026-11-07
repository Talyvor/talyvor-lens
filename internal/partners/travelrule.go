package partners

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// TravelRuleProvider carries the payer's and the payee's identity with a crypto transfer to another regulated provider
// (B48.7: the Travel Rule — FATF Recommendation 16, and in the EU Regulation 2023/1113), and says which addresses such
// a provider holds; any other address is self-hosted.
type TravelRuleProvider interface {
	// Name identifies the implementation.
	Name() string
	// Provider is the regulated provider that holds address on network, or "" when the address is self-hosted.
	Provider(ctx context.Context, network, address string) (string, error)
	// Send sends t to its beneficiary's provider, once under t.ID, and answers its reference.
	Send(ctx context.Context, t TravelRuleTransfer) (string, error)
}

// TravelRuleParty is one side of a transfer as the Travel Rule names it.
type TravelRuleParty struct {
	Name     string `json:"name"`
	Account  string `json:"account,omitempty"`  // the account or wallet it pays from or into
	Address  string `json:"address,omitempty"`  // its address on the network
	Country  string `json:"country,omitempty"`  //
	Provider string `json:"provider,omitempty"` // the regulated provider that holds it
}

// TravelRuleTransfer is the identity data that travels with one transfer.
type TravelRuleTransfer struct {
	ID          string          `json:"id"` // the caller's id for the transfer
	Network     string          `json:"network"`
	Asset       string          `json:"asset"`
	AmountMinor int64           `json:"amount_minor"`
	Originator  TravelRuleParty `json:"originator"`
	Beneficiary TravelRuleParty `json:"beneficiary"`
}

// TestTravelRuleProvider is test mode for the Travel Rule: VASPs is the addresses a regulated provider holds (lower
// case → its name), beside TestVASPAddress, which "Test Exchange" always holds (B48.31), and every other address is
// self-hosted. It keeps what it is sent, once per id.
type TestTravelRuleProvider struct {
	VASPs map[string]string

	mu   sync.Mutex
	sent []TravelRuleTransfer
}

// Name is "test".
func (*TestTravelRuleProvider) Name() string { return "test" }

// TestVASPAddress is the address the Test Travel Rule provider always names as held by TestVASP (B48.31).
const TestVASPAddress, TestVASP = "0x7e57000000000000000000000000000000000002", "Test Exchange"

// Provider is the provider VASPs names for address, and TestVASP for TestVASPAddress.
func (p *TestTravelRuleProvider) Provider(_ context.Context, _, address string) (string, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if vasp, ok := p.VASPs[address]; ok {
		return vasp, nil
	}
	if address == TestVASPAddress {
		return TestVASP, nil
	}
	return "", nil
}

// Send keeps t.
func (p *TestTravelRuleProvider) Send(_ context.Context, t TravelRuleTransfer) (string, error) {
	if err := checkID(t.ID); err != nil {
		return "", err
	}
	if t.Originator.Name == "" || t.Beneficiary.Name == "" || t.Beneficiary.Provider == "" {
		return "", fmt.Errorf("%w: a Travel Rule transfer names its originator, its beneficiary and the beneficiary's provider", ErrInvalid)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sent {
		if s.ID == t.ID {
			return "tr_test_" + t.ID, nil
		}
	}
	p.sent = append(p.sent, t)
	return "tr_test_" + t.ID, nil
}

// Sent is every transfer p was sent, in order.
func (p *TestTravelRuleProvider) Sent() []TravelRuleTransfer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TravelRuleTransfer(nil), p.sent...)
}
