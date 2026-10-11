package partners

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// B37.5 — every call through a partner rail leaves exactly one audit row holding nothing forbidden, and every call
// that moves or commits money refuses a missing id and a reused one. Both are checked by walking each service's
// interface by reflection, so a method added later is held to them without anyone listing it here.

// callRecorder is a CallLog that keeps every Call in memory.
type callRecorder struct {
	mu    sync.Mutex
	calls []Call
}

func (c *callRecorder) LogCall(_ context.Context, call Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *callRecorder) since(n int) []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Call(nil), c.calls[n:]...)
}

// rail is one service as the walk sees it: its interface, what the registry hands out, and the Test partner.
type rail struct {
	iface    reflect.Type
	observed any
	test     any
}

func ifaceOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

func railsOf(r *Registry) map[Service]rail {
	ctx := context.Background()
	return map[Service]rail{
		ServiceAccount:    {ifaceOf[AccountPartner](), must(r.Account(ctx, "test")), r.account},
		ServiceFX:         {ifaceOf[FXPartner](), must(r.FX(ctx, "test")), r.fx},
		ServiceBroker:     {ifaceOf[BrokerPartner](), must(r.Broker(ctx, "test")), r.broker},
		ServiceStablecoin: {ifaceOf[StablecoinPartner](), must(r.Stablecoin(ctx, "test")), r.stablecoin},
		ServiceKYC:        {ifaceOf[KYCProvider](), must(r.KYC(ctx, "test")), r.kyc},
		ServiceScreening:  {ifaceOf[ScreeningProvider](), must(r.Screening(ctx, "test")), r.screening},
		ServiceCapital:    {ifaceOf[CapitalPartner](), must(r.Capital(ctx, "test")), r.capital},
		ServiceInsurer:    {ifaceOf[InsurerPartner](), must(r.Insurer(ctx, "test")), r.insurer},
		ServiceAgentToken: {ifaceOf[AgentTokenProvider](), must(r.AgentToken(ctx, "test")), r.agentToken},
		ServiceTax:        {ifaceOf[TaxPartner](), r.Tax(), r.tax},
	}
}

var ctxType = ifaceOf[context.Context]()

// calls is every method of iface that calls the partner: those that take a context. Name only identifies one.
func calls(iface reflect.Type) []reflect.Method {
	var out []reflect.Method
	for i := 0; i < iface.NumMethod(); i++ {
		if m := iface.Method(i); m.Type.NumIn() > 0 && m.Type.In(0) == ctxType {
			out = append(out, m)
		}
	}
	return out
}

// forbidden is planted in every string a call is given but the caller's id: a reference, a name, an account number.
// No audit row may hold it.
const forbidden = "FORBIDDEN"

// planted is a value of t with forbidden in every string, the caller's id where a top-level ID is, and one element
// in every slice.
func planted(t reflect.Type, id string, top bool) reflect.Value {
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.String:
		v.SetString(forbidden + "-" + t.Name())
	case reflect.Slice:
		v.Set(reflect.Append(v, planted(t.Elem(), id, false)))
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			switch {
			case !f.IsExported() || f.Type == reflect.TypeOf(time.Time{}):
			case top && f.Name == "ID" && f.Type.Kind() == reflect.String:
				v.Field(i).SetString(id)
			default:
				v.Field(i).Set(planted(f.Type, id, false))
			}
		}
	}
	return v
}

// takesID is whether a call's request carries the caller's id.
func takesID(m reflect.Method) bool {
	if m.Type.NumIn() < 2 || m.Type.In(1).Kind() != reflect.Struct {
		return false
	}
	f, ok := m.Type.In(1).FieldByName("ID")
	return ok && f.Type.Kind() == reflect.String
}

// auditProblems calls each of s's methods through observed and says how its audit row is wrong.
func auditProblems(s Service, rl rail, log *callRecorder) []string {
	var problems []string
	for _, m := range calls(rl.iface) {
		id, wantID := "id-"+m.Name, ""
		if takesID(m) {
			wantID = id
		}
		args := []reflect.Value{reflect.ValueOf(WithWorkspace(context.Background(), "ws_audit"))}
		for i := 1; i < m.Type.NumIn(); i++ {
			args = append(args, planted(m.Type.In(i), id, true))
		}
		before := len(log.since(0))
		reflect.ValueOf(rl.observed).MethodByName(m.Name).Call(args)
		rows := log.since(before)
		if len(rows) != 1 {
			problems = append(problems, fmt.Sprintf("%s.%s left %d audit rows, not 1", s, m.Name, len(rows)))
			continue
		}
		c := rows[0]
		switch {
		case c.Service != s || c.Method != m.Name:
			problems = append(problems, fmt.Sprintf("%s.%s is audited as %s.%s", s, m.Name, c.Service, c.Method))
		case c.Workspace != "ws_audit":
			problems = append(problems, fmt.Sprintf("%s.%s is audited for workspace %q, not the caller's", s, m.Name, c.Workspace))
		case c.ID != wantID:
			problems = append(problems, fmt.Sprintf("%s.%s is audited with id %q, want %q", s, m.Name, c.ID, wantID))
		case c.Outcome != OutcomeOK && c.Outcome != OutcomeRefused && c.Outcome != OutcomeFailed:
			problems = append(problems, fmt.Sprintf("%s.%s is audited with outcome %q", s, m.Name, c.Outcome))
		case c.At.IsZero() || c.Duration < 0:
			problems = append(problems, fmt.Sprintf("%s.%s is audited at %v taking %v", s, m.Name, c.At, c.Duration))
		case strings.Contains(fmt.Sprintf("%+v", c), forbidden):
			problems = append(problems, fmt.Sprintf("%s.%s's audit row holds what it was given: %+v", s, m.Name, c))
		}
	}
	return problems
}

// notMoney is every call that neither moves nor commits money. Every other call is held to the id rules, so a
// method added later is held to them unless it is put here.
var notMoney = map[string]bool{
	"account.OpenAccount": true, "account.AccountDetails": true, "account.PaymentStatus": true, "account.StatementLines": true, "account.CheckPayee": true,
	"account.QuoteInternational": true,
	"fx.Quote":                   true, "fx.Status": true,
	"broker.OpenAccount": true, "broker.Quote": true, "broker.OrderStatus": true, "broker.Cancel": true,
	"broker.Positions": true, "broker.Fills": true,
	"stablecoin.Address": true, "stablecoin.Status": true,
	"kyc.StartCheck": true, "kyc.CheckResult": true,
	"screening.ScreenName": true, "screening.ScreenPayment": true,
	"capital.Offer": true, "capital.Status": true,
	"insurer.Quote": true, "insurer.ClaimStatus": true,
	"agent_token.Status": true, "agent_token.Revoke": true,
	"tax.Calculate": true, "tax.ValidateTaxID": true,
}

// tokenExpiry is one expiry for every token moneyCalls asks for, so asking again is the same request.
var tokenExpiry = time.Now().Add(time.Hour)

// moneyCalls is a valid request for each call that moves or commits money, made against p, the service's Test
// partner, under id. Variant 1 is a different request from variant 0.
var moneyCalls = map[string]func(p any, id string, variant int64) any{
	"account.SendPayment": func(p any, id string, v int64) any {
		acct := must(p.(AccountPartner).OpenAccount(context.Background(), AccountRequest{ID: "acct-" + id, Holder: "Ada", Currency: "GBP"}))
		return PaymentRequest{ID: id, AccountRef: acct.Ref, Amount: Money{10_00 + v, "GBP"}}
	},
	"account.PayByBank": func(p any, id string, v int64) any {
		acct := must(p.(AccountPartner).OpenAccount(context.Background(), AccountRequest{ID: "acct-" + id, Holder: "Ada", Currency: "GBP"}))
		return PayByBankRequest{ID: id, AccountRef: acct.Ref, Amount: Money{10_00 + v, "GBP"}}
	},
	"fx.Execute": func(p any, id string, v int64) any {
		q := must(p.(FXPartner).Quote(context.Background(), FXQuoteRequest{ID: fmt.Sprint("q-", id, v), Sell: Money{10_00, "GBP"}, Buy: "EUR"}))
		return FXExecution{ID: id, QuoteRef: q.Ref}
	},
	"broker.Place": func(p any, id string, v int64) any {
		acct := must(p.(BrokerPartner).OpenAccount(context.Background(), AccountRequest{ID: "bacct-" + id, Holder: "Ada", Currency: "GBP"}))
		return Order{ID: id, AccountRef: acct.Ref, Symbol: "TLVR", Side: SideBuy, Amount: Money{1000_00 + v, "GBP"}}
	},
	"stablecoin.Send": func(_ any, id string, v int64) any {
		return StablecoinTransfer{ID: id, Network: "base", To: "0xabc", Amount: Money{10_000_000 + v, "USDC"}}
	},
	"capital.Fund": func(p any, id string, v int64) any {
		offer := must(p.(CapitalPartner).Offer(context.Background(), CapitalRequest{ID: "offer-" + id, Company: "Acme Ltd",
			CompanyNumber: "01234567", Amount: Money{100_00, "GBP"}, TermDays: 30}))
		return Funding{ID: id, OfferRef: offer.Ref, AccountRef: fmt.Sprint("acct-", v)}
	},
	"capital.Repay": func(p any, id string, v int64) any {
		c := p.(CapitalPartner)
		offer := must(c.Offer(context.Background(), CapitalRequest{ID: "offer-" + id, Company: "Acme Ltd", CompanyNumber: "01234567",
			Amount: Money{100_00, "GBP"}, TermDays: 30}))
		funding := must(c.Fund(context.Background(), Funding{ID: "fund-" + id, OfferRef: offer.Ref, AccountRef: "acct"}))
		return Repayment{ID: id, FundingRef: funding.Ref, Amount: Money{10_00 + v, "GBP"}}
	},
	"insurer.Bind": func(p any, id string, v int64) any {
		q := must(p.(InsurerPartner).Quote(context.Background(), CoverRequest{ID: fmt.Sprint("cq-", id, v), Insured: "Acme Ltd",
			Cover: Money{100_00, "GBP"}, TermDays: 30, Risk: "an agent's mistakes"}))
		return BindRequest{ID: id, QuoteRef: q.Ref}
	},
	"insurer.Claim": func(p any, id string, v int64) any {
		i := p.(InsurerPartner)
		q := must(i.Quote(context.Background(), CoverRequest{ID: "cq-" + id, Insured: "Acme Ltd", Cover: Money{100_00, "GBP"},
			TermDays: 30, Risk: "an agent's mistakes"}))
		policy := must(i.Bind(context.Background(), BindRequest{ID: "pol-" + id, QuoteRef: q.Ref}))
		return ClaimRequest{ID: id, PolicyRef: policy.Ref, Amount: Money{10_00 + v, "GBP"}, What: "a bad trade"}
	},
	"agent_token.Provision": func(_ any, id string, v int64) any {
		return AgentTokenRequest{ID: id, AgentID: "agent-1", AgentName: fmt.Sprint("Agent ", v), Currency: "GBP",
			ExpiresAt: tokenExpiry}
	},
	"agent_token.Authorise": func(p any, id string, v int64) any {
		token := must(p.(AgentTokenProvider).Provision(context.Background(), AgentTokenRequest{ID: "tok-" + id, AgentID: "agent-1",
			AgentName: "Agent", Currency: "GBP", ExpiresAt: tokenExpiry}))
		return Authorisation{ID: id, TokenRef: token.Ref, Amount: Money{10_00 + v, "GBP"}, Merchant: "Acme"}
	},
}

// idProblems makes each of s's money calls on its Test partner without an id, and twice under one id with different
// requests, and says where it was not refused as it must be.
func idProblems(s Service, rl rail) []string {
	var problems []string
	for _, m := range calls(rl.iface) {
		key := string(s) + "." + m.Name
		if notMoney[key] {
			continue
		}
		request, ok := moneyCalls[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s moves or commits money, but there is no valid request for it in "+
				"moneyCalls to check it refuses a missing or reused id: add one, or add it to notMoney", key))
			continue
		}
		call := func(req any) error {
			out := reflect.ValueOf(rl.test).MethodByName(m.Name).Call([]reflect.Value{reflect.ValueOf(context.Background()), reflect.ValueOf(req)})
			err, _ := out[len(out)-1].Interface().(error)
			return err
		}
		id := "id-" + key
		if err := call(request(rl.test, "", 0)); !errors.Is(err, ErrInvalid) {
			problems = append(problems, fmt.Sprintf("%s without the caller's id answered %v, not ErrInvalid", key, err))
		}
		if err := call(request(rl.test, id, 0)); err != nil {
			problems = append(problems, fmt.Sprintf("%s refused moneyCalls' request: %v", key, err))
			continue
		}
		if err := call(request(rl.test, id, 1)); !errors.Is(err, ErrIDReused) {
			problems = append(problems, fmt.Sprintf("%s under a used id with a different request answered %v, not ErrIDReused", key, err))
		}
	}
	return problems
}

func TestEveryPartnerCallIsAuditedAndEveryMoneyCallNeedsItsOwnID(t *testing.T) {
	r := NewRegistry(nil)
	r.UseTaxData(noTaxRows{})
	log := &callRecorder{}
	r.UseCallLog(log)
	rails := railsOf(r)
	for _, s := range Services {
		rl, ok := rails[s]
		if !ok {
			t.Errorf("%s is not walked: add it to railsOf", s)
			continue
		}
		for _, p := range auditProblems(s, rl, log) {
			t.Error(p)
		}
		for _, p := range idProblems(s, rl) {
			t.Error(p)
		}
	}
	// The row holds only what B37.5 names: a field added to it is a field to decide about.
	var fields []string
	for i := 0; i < reflect.TypeOf(Call{}).NumField(); i++ {
		fields = append(fields, reflect.TypeOf(Call{}).Field(i).Name)
	}
	if got := strings.Join(fields, " "); got != "At Service Method Workspace ID Outcome Duration" {
		t.Errorf("an audit row holds %s", got)
	}
}

// skipsAudit is the FX rail with Execute written as a later method could be: forwarded without its audit call.
type skipsAudit struct{ observedFX }

func (o skipsAudit) Execute(ctx context.Context, req FXExecution) (Result, error) {
	return o.FXPartner.Execute(ctx, req)
}

// skipsID is a Test FX partner whose Execute ignores the caller's id.
type skipsID struct{ *TestFXPartner }

func (skipsID) Execute(context.Context, FXExecution) (Result, error) {
	return Result{Status: StatusCompleted}, nil
}

// The walk is not blind: a planted method that skips the audit, or the id, is named.
func TestTheWalkCatchesAPlantedMethodThatSkipsTheAuditOrTheID(t *testing.T) {
	r := NewRegistry(nil)
	log := &callRecorder{}
	r.UseCallLog(log)
	fx := must(r.FX(context.Background(), "test")).(observedFX)

	got := auditProblems(ServiceFX, rail{iface: ifaceOf[FXPartner](), observed: skipsAudit{fx}}, log)
	if len(got) != 1 || !strings.HasPrefix(got[0], "fx.Execute left 0 audit rows") {
		t.Errorf("a planted Execute that skips the audit: %q", got)
	}

	got = idProblems(ServiceFX, rail{iface: ifaceOf[FXPartner](), test: skipsID{&TestFXPartner{}}})
	if len(got) != 2 || !strings.Contains(got[0], "without the caller's id") || !strings.Contains(got[1], "not ErrIDReused") {
		t.Errorf("a planted Execute that ignores the id: %q", got)
	}

	// And a money method with no request to check it by is named rather than passed.
	if got := idProblems(ServiceFX, rail{iface: reflect.TypeOf((*interface {
		FXPartner
		Hedge(context.Context, FXExecution) (Result, error)
	})(nil)).Elem(), test: struct{ *TestFXPartner }{&TestFXPartner{}}}); len(got) != 1 || !strings.Contains(got[0], "fx.Hedge moves or commits money") {
		t.Errorf("a new money method with no request in moneyCalls: %q", got)
	}
}
