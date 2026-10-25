package partners

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// Service is one kind of outside service.
type Service string

// The services.
const (
	ServiceAccount    Service = "account"
	ServiceFX         Service = "fx"
	ServiceBroker     Service = "broker"
	ServiceStablecoin Service = "stablecoin"
	ServiceKYC        Service = "kyc"
	ServiceScreening  Service = "screening"
	ServiceCapital    Service = "capital"
	ServiceInsurer    Service = "insurer"
	ServiceAgentToken Service = "agent_token"
	ServiceTax        Service = "tax"
)

// Clearances says whether a wallet capability may take live money now: the operator's clearance for it is in
// force, and lists the country the use comes from (economy.DualTokenStore.CapabilityCleared).
type Clearances interface {
	CapabilityCleared(ctx context.Context, capability string) (bool, error)
}

// Registry hands out the partner for each service. Each is asked with the wallet capability the money moves
// for, and answers with the Test implementation unless that capability has a live clearance AND a real adapter
// is configured for the service. There is no real adapter yet.
//
// It keeps one Test implementation of each, so what one call does a later status or statement read sees.
type Registry struct {
	clearances Clearances

	mu       sync.RWMutex
	adapters map[Service]any
	health   map[Service]*railHealth
	calls    CallLog

	account    *TestAccountPartner
	fx         *TestFXPartner
	broker     *TestBrokerPartner
	stablecoin *TestStablecoinPartner
	kyc        *TestKYCProvider
	capital    *TestCapitalPartner
	insurer    *TestInsurerPartner
	agentToken *TestAgentTokenProvider
	tax        *TestTaxPartner
	screening  TestScreeningProvider
	verifiers  map[string]verifier // by subject
}

// verifier is a real verification provider and the countries it checks; none is every country.
type verifier struct {
	KYCProvider
	countries []string
}

// NewRegistry is a registry that asks clearances before it hands out a real adapter. With nil clearances it
// hands out the Test implementations only.
func NewRegistry(clearances Clearances) *Registry {
	health := map[Service]*railHealth{}
	for _, s := range Services {
		health[s] = &railHealth{}
	}
	return &Registry{clearances: clearances, adapters: map[Service]any{}, health: health, account: &TestAccountPartner{}, fx: &TestFXPartner{},
		broker: &TestBrokerPartner{}, stablecoin: &TestStablecoinPartner{}, kyc: &TestKYCProvider{}, capital: &TestCapitalPartner{},
		insurer: &TestInsurerPartner{}, agentToken: &TestAgentTokenProvider{}, tax: &TestTaxPartner{}}
}

// UseTaxData is where the Test tax partner reads the rates and registrations: a TaxStore.
func (r *Registry) UseTaxData(d TaxData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tax = &TestTaxPartner{Data: d}
}

// UseScreeningList is the sanctions lists the Test screening provider screens against (internal/screening, B30.6).
func (r *Registry) UseScreeningList(l ScreeningList) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.screening = TestScreeningProvider{List: l}
}

// UseCallLog is where every call through the registry's partners is audited (B37.5): a CallStore.
func (r *Registry) UseCallLog(l CallLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = l
}

// observer is s's rail, audited to the call log.
func (r *Registry) observer(s Service) observer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return observer{s, r.health[s], r.calls}
}

// Configure sets the real adapter for a service. It is used only once a capability the service serves is
// cleared; until then the Test implementation answers.
func (r *Registry) Configure(s Service, adapter any) error {
	ok := false
	switch s {
	case ServiceAccount:
		_, ok = adapter.(AccountPartner)
	case ServiceFX:
		_, ok = adapter.(FXPartner)
	case ServiceBroker:
		_, ok = adapter.(BrokerPartner)
	case ServiceStablecoin:
		_, ok = adapter.(StablecoinPartner)
	case ServiceKYC:
		_, ok = adapter.(KYCProvider)
	case ServiceScreening:
		_, ok = adapter.(ScreeningProvider)
	case ServiceCapital:
		_, ok = adapter.(CapitalPartner)
	case ServiceInsurer:
		_, ok = adapter.(InsurerPartner)
	case ServiceAgentToken:
		_, ok = adapter.(AgentTokenProvider)
	case ServiceTax:
		_, ok = adapter.(TaxPartner)
	default:
		return fmt.Errorf("partners: no service is called %q", s)
	}
	if !ok {
		return fmt.Errorf("partners: %T is not a %s partner", adapter, s)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[s] = adapter
	return nil
}

// pick is the configured adapter for s when capability is cleared, and test otherwise.
func pick[T any](ctx context.Context, r *Registry, s Service, capability string, test T) (T, error) {
	r.mu.RLock()
	adapter, configured := r.adapters[s]
	r.mu.RUnlock()
	if !configured || r.clearances == nil {
		return test, nil
	}
	cleared, err := r.clearances.CapabilityCleared(ctx, capability)
	if err != nil {
		var none T
		return none, fmt.Errorf("partners: is %s cleared: %w", capability, err)
	}
	if !cleared {
		return test, nil
	}
	return adapter.(T), nil
}

// Account is the account partner for capability.
func (r *Registry) Account(ctx context.Context, capability string) (AccountPartner, error) {
	p, err := pick[AccountPartner](ctx, r, ServiceAccount, capability, r.account)
	if err != nil {
		return nil, err
	}
	return observedAccount{p, r.observer(ServiceAccount)}, nil
}

// FX is the conversion partner for capability.
func (r *Registry) FX(ctx context.Context, capability string) (FXPartner, error) {
	p, err := pick[FXPartner](ctx, r, ServiceFX, capability, r.fx)
	if err != nil {
		return nil, err
	}
	return observedFX{p, r.observer(ServiceFX)}, nil
}

// Broker is the broker for capability.
func (r *Registry) Broker(ctx context.Context, capability string) (BrokerPartner, error) {
	p, err := pick[BrokerPartner](ctx, r, ServiceBroker, capability, r.broker)
	if err != nil {
		return nil, err
	}
	return observedBroker{p, r.observer(ServiceBroker)}, nil
}

// Stablecoin is the stablecoin partner for capability.
func (r *Registry) Stablecoin(ctx context.Context, capability string) (StablecoinPartner, error) {
	p, err := pick[StablecoinPartner](ctx, r, ServiceStablecoin, capability, r.stablecoin)
	if err != nil {
		return nil, err
	}
	return observedStablecoin{p, r.observer(ServiceStablecoin)}, nil
}

// KYC is the verification provider for capability.
func (r *Registry) KYC(ctx context.Context, capability string) (KYCProvider, error) {
	p, err := pick[KYCProvider](ctx, r, ServiceKYC, capability, r.kyc)
	if err != nil {
		return nil, err
	}
	return observedKYC{p, r.observer(ServiceKYC)}, nil
}

// UseVerifier makes p the provider of checks on subject (KYCPerson, KYCCompany) in countries, or in every country
// when none is named (B30.112): Persona for a person's identity, Companies House for a UK company.
func (r *Registry) UseVerifier(subject string, p KYCProvider, countries ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verifiers == nil {
		r.verifiers = map[string]verifier{}
	}
	r.verifiers[subject] = verifier{p, countries}
}

// Verifiers is the provider a check on subject in country goes to; synthetic is a tester's workspace.
type Verifiers func(subject, country string, synthetic bool) KYCProvider

// Verification is the provider a check on subject in country goes to (B30.4): the real one set for the subject and
// country by UseVerifier, else the configured KYC adapter, else the Test one — and the Test one, always, for a
// synthetic workspace. A check moves no money, so no clearance is asked; a pass by the Test provider counts for test
// money only (economy.WorkspaceVerification).
func (r *Registry) Verification(subject, country string, synthetic bool) KYCProvider {
	r.mu.RLock()
	p := KYCProvider(r.kyc)
	if adapter, ok := r.adapters[ServiceKYC]; ok && !synthetic {
		p = adapter.(KYCProvider)
	}
	if v, ok := r.verifiers[subject]; ok && !synthetic && (len(v.countries) == 0 || slices.Contains(v.countries, country)) {
		p = v.KYCProvider
	}
	r.mu.RUnlock()
	return observedKYC{p, r.observer(ServiceKYC)}
}

// OneVerifier is p for every check.
func OneVerifier(p KYCProvider) Verifiers {
	return func(string, string, bool) KYCProvider { return p }
}

// Screening is the screening provider for capability.
func (r *Registry) Screening(ctx context.Context, capability string) (ScreeningProvider, error) {
	r.mu.RLock()
	test := r.screening
	r.mu.RUnlock()
	p, err := pick[ScreeningProvider](ctx, r, ServiceScreening, capability, test)
	if err != nil {
		return nil, err
	}
	return observedScreening{p, r.observer(ServiceScreening)}, nil
}

// Capital is the capital partner for capability.
func (r *Registry) Capital(ctx context.Context, capability string) (CapitalPartner, error) {
	p, err := pick[CapitalPartner](ctx, r, ServiceCapital, capability, r.capital)
	if err != nil {
		return nil, err
	}
	return observedCapital{p, r.observer(ServiceCapital)}, nil
}

// Insurer is the insurer for capability.
func (r *Registry) Insurer(ctx context.Context, capability string) (InsurerPartner, error) {
	p, err := pick[InsurerPartner](ctx, r, ServiceInsurer, capability, r.insurer)
	if err != nil {
		return nil, err
	}
	return observedInsurer{p, r.observer(ServiceInsurer)}, nil
}

// AgentToken is the agent-token provider for capability.
func (r *Registry) AgentToken(ctx context.Context, capability string) (AgentTokenProvider, error) {
	p, err := pick[AgentTokenProvider](ctx, r, ServiceAgentToken, capability, r.agentToken)
	if err != nil {
		return nil, err
	}
	return observedAgentToken{p, r.observer(ServiceAgentToken)}, nil
}

// Tax is the tax partner: the real one once it is configured (B32.45), and the Test one until then. Tax moves no
// money, so no clearance is asked.
func (r *Registry) Tax() TaxPartner {
	r.mu.RLock()
	p := TaxPartner(r.tax)
	if adapter, ok := r.adapters[ServiceTax]; ok {
		p = adapter.(TaxPartner)
	}
	r.mu.RUnlock()
	return observedTax{p, r.observer(ServiceTax)}
}

// TestAccount, TestStablecoin and TestKYC are the registry's Test partners whatever is cleared or configured, observed
// as every other call is: the money layer's sandbox adapter (B46.3) answers with them.
func (r *Registry) TestAccount() AccountPartner {
	return observedAccount{r.account, r.observer(ServiceAccount)}
}

func (r *Registry) TestStablecoin() StablecoinPartner {
	return observedStablecoin{r.stablecoin, r.observer(ServiceStablecoin)}
}

func (r *Registry) TestKYC() KYCProvider { return observedKYC{r.kyc, r.observer(ServiceKYC)} }
