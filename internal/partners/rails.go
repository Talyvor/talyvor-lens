package partners

import (
	"context"
	"sync"
	"time"
)

// Services is every service, in the order the status page lists them.
var Services = []Service{ServiceAccount, ServiceFX, ServiceBroker, ServiceStablecoin, ServiceKYC, ServiceScreening,
	ServiceCapital, ServiceInsurer, ServiceAgentToken, ServiceTax}

// Rail is one service as the status page shows it (B30.12): which partner answers it, and when one last answered and
// last failed. It carries no reference, key or error text.
type Rail struct {
	Service Service `json:"service"`
	// Mode is "live" once a real adapter is configured for the service, and "test" until then. A live adapter takes
	// real money only for a capability that is cleared.
	Mode        string     `json:"mode"`
	LastSuccess *time.Time `json:"last_success"`
	LastFailure *time.Time `json:"last_failure"`
}

// railHealth is when a service's partner last answered and last failed: this process's calls and probes, and what
// WatchRails last took from partner_rails.
type railHealth struct {
	mu         sync.Mutex
	ok, failed time.Time
}

// done records a call's outcome. A request the partner turned down as invalid, unknown or a reused id is an answer,
// so it counts as a success: the rail is up and the caller asked wrongly.
func (h *railHealth) done(err *error) {
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	if outcome(*err) == OutcomeFailed {
		h.failed = now
	} else {
		h.ok = now
	}
}

// Rails is every service's rail, in Services order. Another process's calls show once WatchRails has shared them,
// within its interval.
func (r *Registry) Rails() []Rail {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Rail, 0, len(Services))
	for _, s := range Services {
		rail := Rail{Service: s, Mode: "test"}
		if _, ok := r.adapters[s]; ok {
			rail.Mode = "live"
		}
		h := r.health[s]
		h.mu.Lock()
		if !h.ok.IsZero() {
			t := h.ok
			rail.LastSuccess = &t
		}
		if !h.failed.IsZero() {
			t := h.failed
			rail.LastFailure = &t
		}
		h.mu.Unlock()
		out = append(out, rail)
	}
	return out
}

// observer watches one service's rail: each call's outcome goes on the rail and, once the registry has a CallLog,
// into the audit as one Call (B37.5).
type observer struct {
	service Service
	h       *railHealth
	log     CallLog
}

// call starts a call to method under the caller's id ("" for a read: its argument is a partner's reference, which
// the audit never holds). Defer the func it returns with the call's error.
func (o observer) call(ctx context.Context, method, id string) func(*error) {
	start := time.Now()
	return func(err *error) {
		o.h.done(err)
		if o.log != nil {
			ws, _ := ctx.Value(workspaceKey{}).(string)
			o.log.LogCall(ctx, Call{At: start.UTC(), Service: o.service, Method: method, Workspace: ws, ID: id,
				Outcome: outcome(*err), Duration: time.Since(start)})
		}
	}
}

// Each observed partner is the partner it wraps, recording every call on its service's rail and in the audit.

type observedAccount struct {
	AccountPartner
	observer
}

func (o observedAccount) OpenAccount(ctx context.Context, req AccountRequest) (_ Account, err error) {
	defer o.call(ctx, "OpenAccount", req.ID)(&err)
	return o.AccountPartner.OpenAccount(ctx, req)
}
func (o observedAccount) AccountDetails(ctx context.Context, ref string) (_ AccountDetails, err error) {
	defer o.call(ctx, "AccountDetails", "")(&err)
	return o.AccountPartner.AccountDetails(ctx, ref)
}
func (o observedAccount) SendPayment(ctx context.Context, req PaymentRequest) (_ Result, err error) {
	defer o.call(ctx, "SendPayment", req.ID)(&err)
	return o.AccountPartner.SendPayment(ctx, req)
}
func (o observedAccount) PaymentStatus(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "PaymentStatus", "")(&err)
	return o.AccountPartner.PaymentStatus(ctx, ref)
}
func (o observedAccount) StatementLines(ctx context.Context, ref string, since time.Time) (_ []StatementLine, err error) {
	defer o.call(ctx, "StatementLines", "")(&err)
	return o.AccountPartner.StatementLines(ctx, ref, since)
}
func (o observedAccount) PayByBank(ctx context.Context, req PayByBankRequest) (_ PayByBank, err error) {
	defer o.call(ctx, "PayByBank", req.ID)(&err)
	return o.AccountPartner.PayByBank(ctx, req)
}
func (o observedAccount) CheckPayee(ctx context.Context, p Payee) (_ PayeeCheck, err error) {
	defer o.call(ctx, "CheckPayee", "")(&err)
	return o.AccountPartner.CheckPayee(ctx, p)
}

type observedFX struct {
	FXPartner
	observer
}

func (o observedFX) Quote(ctx context.Context, req FXQuoteRequest) (_ FXQuote, err error) {
	defer o.call(ctx, "Quote", req.ID)(&err)
	return o.FXPartner.Quote(ctx, req)
}
func (o observedFX) Execute(ctx context.Context, req FXExecution) (_ Result, err error) {
	defer o.call(ctx, "Execute", req.ID)(&err)
	return o.FXPartner.Execute(ctx, req)
}
func (o observedFX) Status(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "Status", "")(&err)
	return o.FXPartner.Status(ctx, ref)
}

type observedBroker struct {
	BrokerPartner
	observer
}

func (o observedBroker) OpenAccount(ctx context.Context, req AccountRequest) (_ Account, err error) {
	defer o.call(ctx, "OpenAccount", req.ID)(&err)
	return o.BrokerPartner.OpenAccount(ctx, req)
}
func (o observedBroker) Quote(ctx context.Context, symbol, currency string) (_ BrokerQuote, err error) {
	defer o.call(ctx, "Quote", "")(&err)
	return o.BrokerPartner.Quote(ctx, symbol, currency)
}
func (o observedBroker) Place(ctx context.Context, req Order) (_ Result, err error) {
	defer o.call(ctx, "Place", req.ID)(&err)
	return o.BrokerPartner.Place(ctx, req)
}
func (o observedBroker) OrderStatus(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "OrderStatus", "")(&err)
	return o.BrokerPartner.OrderStatus(ctx, ref)
}
func (o observedBroker) Cancel(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "Cancel", "")(&err)
	return o.BrokerPartner.Cancel(ctx, ref)
}
func (o observedBroker) Positions(ctx context.Context, ref string) (_ []Position, err error) {
	defer o.call(ctx, "Positions", "")(&err)
	return o.BrokerPartner.Positions(ctx, ref)
}
func (o observedBroker) Fills(ctx context.Context, ref string, since time.Time) (_ []Fill, err error) {
	defer o.call(ctx, "Fills", "")(&err)
	return o.BrokerPartner.Fills(ctx, ref, since)
}

type observedStablecoin struct {
	StablecoinPartner
	observer
}

func (o observedStablecoin) Address(ctx context.Context, req AddressRequest) (_ Address, err error) {
	defer o.call(ctx, "Address", req.ID)(&err)
	return o.StablecoinPartner.Address(ctx, req)
}
func (o observedStablecoin) Send(ctx context.Context, req StablecoinTransfer) (_ Result, err error) {
	defer o.call(ctx, "Send", req.ID)(&err)
	return o.StablecoinPartner.Send(ctx, req)
}
func (o observedStablecoin) Status(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "Status", "")(&err)
	return o.StablecoinPartner.Status(ctx, ref)
}

type observedKYC struct {
	KYCProvider
	observer
}

func (o observedKYC) StartCheck(ctx context.Context, req KYCRequest) (_ Result, err error) {
	defer o.call(ctx, "StartCheck", req.ID)(&err)
	return o.KYCProvider.StartCheck(ctx, req)
}
func (o observedKYC) CheckResult(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "CheckResult", "")(&err)
	return o.KYCProvider.CheckResult(ctx, ref)
}

type observedScreening struct {
	ScreeningProvider
	observer
}

func (o observedScreening) ScreenName(ctx context.Context, req NameScreen) (_ Screening, err error) {
	defer o.call(ctx, "ScreenName", "")(&err)
	return o.ScreeningProvider.ScreenName(ctx, req)
}
func (o observedScreening) ScreenPayment(ctx context.Context, req PaymentScreen) (_ Screening, err error) {
	defer o.call(ctx, "ScreenPayment", req.ID)(&err)
	return o.ScreeningProvider.ScreenPayment(ctx, req)
}

type observedCapital struct {
	CapitalPartner
	observer
}

func (o observedCapital) Offer(ctx context.Context, req CapitalRequest) (_ CapitalOffer, err error) {
	defer o.call(ctx, "Offer", req.ID)(&err)
	return o.CapitalPartner.Offer(ctx, req)
}
func (o observedCapital) Fund(ctx context.Context, req Funding) (_ Result, err error) {
	defer o.call(ctx, "Fund", req.ID)(&err)
	return o.CapitalPartner.Fund(ctx, req)
}
func (o observedCapital) Repay(ctx context.Context, req Repayment) (_ Result, err error) {
	defer o.call(ctx, "Repay", req.ID)(&err)
	return o.CapitalPartner.Repay(ctx, req)
}
func (o observedCapital) Status(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "Status", "")(&err)
	return o.CapitalPartner.Status(ctx, ref)
}

type observedInsurer struct {
	InsurerPartner
	observer
}

func (o observedInsurer) Quote(ctx context.Context, req CoverRequest) (_ CoverQuote, err error) {
	defer o.call(ctx, "Quote", req.ID)(&err)
	return o.InsurerPartner.Quote(ctx, req)
}
func (o observedInsurer) Bind(ctx context.Context, req BindRequest) (_ Policy, err error) {
	defer o.call(ctx, "Bind", req.ID)(&err)
	return o.InsurerPartner.Bind(ctx, req)
}
func (o observedInsurer) Claim(ctx context.Context, req ClaimRequest) (_ Result, err error) {
	defer o.call(ctx, "Claim", req.ID)(&err)
	return o.InsurerPartner.Claim(ctx, req)
}
func (o observedInsurer) ClaimStatus(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "ClaimStatus", "")(&err)
	return o.InsurerPartner.ClaimStatus(ctx, ref)
}

type observedAgentToken struct {
	AgentTokenProvider
	observer
}

func (o observedAgentToken) Provision(ctx context.Context, req AgentTokenRequest) (_ AgentToken, err error) {
	defer o.call(ctx, "Provision", req.ID)(&err)
	return o.AgentTokenProvider.Provision(ctx, req)
}
func (o observedAgentToken) Authorise(ctx context.Context, req Authorisation) (_ Result, err error) {
	defer o.call(ctx, "Authorise", req.ID)(&err)
	return o.AgentTokenProvider.Authorise(ctx, req)
}
func (o observedAgentToken) Status(ctx context.Context, ref string) (_ Result, err error) {
	defer o.call(ctx, "Status", "")(&err)
	return o.AgentTokenProvider.Status(ctx, ref)
}
func (o observedAgentToken) Revoke(ctx context.Context, ref string) (err error) {
	defer o.call(ctx, "Revoke", "")(&err)
	return o.AgentTokenProvider.Revoke(ctx, ref)
}

type observedTax struct {
	TaxPartner
	observer
}

func (o observedTax) Calculate(ctx context.Context, req TaxRequest) (_ TaxResult, err error) {
	defer o.call(ctx, "Calculate", "")(&err)
	return o.TaxPartner.Calculate(ctx, req)
}
func (o observedTax) ValidateTaxID(ctx context.Context, country, id string) (_ TaxIDResult, err error) {
	defer o.call(ctx, "ValidateTaxID", "")(&err)
	return o.TaxPartner.ValidateTaxID(ctx, country, id)
}
