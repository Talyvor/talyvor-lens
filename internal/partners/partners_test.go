package partners

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// B30.3 — every Test partner answers success, failure, return and pending, by the amount's last two minor digits
// (13 fails, 14 comes back after it was sent, 15 stays pending) or, where there is no amount, by the name.

var outcomes = []struct {
	name       string
	minor      int64
	now, later Status
}{
	{"success", 10_000, StatusCompleted, StatusCompleted},
	{"failure", 10_013, StatusFailed, StatusFailed},
	{"return", 10_014, StatusCompleted, StatusReturned},
	{"pending", 10_015, StatusPending, StatusPending},
}

var nameOutcomes = []struct {
	name       string
	now, later Status
}{
	{"Ada Lovelace", StatusCompleted, StatusCompleted},
	{"Ada TESTFAIL", StatusFailed, StatusFailed},
	{"Ada TESTRETURN", StatusCompleted, StatusReturned},
	{"Ada TESTPENDING", StatusPending, StatusPending},
	{"Ada TESTSANCTION", StatusFailed, StatusFailed},
}

// must is v, and fails the test on err.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// answers checks a call's answer, the read after it, and that the same call again is answered the same.
func answers(t *testing.T, what string, now, later Status, call func() (Result, error), read func(ref string) (Result, error)) {
	t.Helper()
	first := must(call())
	if first.Status != now {
		t.Fatalf("%s: answered %s (%s), want %s", what, first.Status, first.Detail, now)
	}
	if again := must(call()); again != first {
		t.Fatalf("%s: asked again it answered %+v, not %+v", what, again, first)
	}
	if r := must(read(first.Ref)); r.Status != later {
		t.Fatalf("%s: read after, it is %s, want %s", what, r.Status, later)
	}
}

func TestAccountPartner_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestAccountPartner{}
	for _, o := range nameOutcomes {
		answers(t, "open "+o.name, o.now, o.later, func() (Result, error) {
			a, err := p.OpenAccount(ctx, AccountRequest{ID: "acct-" + o.name, Holder: o.name, Currency: "GBP"})
			return a.Result, err
		}, func(string) (Result, error) { return p.book.status("acct", testRef("acct", "acct-"+o.name)) })
	}
	gbp := must(p.OpenAccount(ctx, AccountRequest{ID: "acme-gbp", Holder: "Acme Ltd", Currency: "GBP"}))
	if d := must(p.AccountDetails(ctx, gbp.Ref)); d.SortCode != "00-00-00" || len(d.AccountNumber) != 8 || d.Holder != "Acme Ltd" {
		t.Fatalf("GBP details = %+v", d)
	}
	eur := must(p.OpenAccount(ctx, AccountRequest{ID: "acme-eur", Holder: "Acme Ltd", Currency: "EUR"}))
	iban := must(p.AccountDetails(ctx, eur.Ref)).IBAN
	if !validIBAN(iban) || !strings.HasPrefix(iban[4:], "TEST") {
		t.Fatalf("EUR IBAN %q does not check, or is not a TEST bank's", iban)
	}
	if _, err := p.AccountDetails(ctx, testRef("acct", "acct-Ada TESTFAIL")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("details of a failed account: %v", err)
	}

	start := time.Now().Add(-time.Second)
	for _, o := range outcomes {
		amount := Money{o.minor, "GBP"}
		answers(t, "pay "+o.name, o.now, o.later, func() (Result, error) {
			return p.SendPayment(ctx, PaymentRequest{ID: "pay-" + o.name, AccountRef: gbp.Ref, Amount: amount, Payee: Payee{Name: "Bob"}})
		}, func(ref string) (Result, error) { return p.PaymentStatus(ctx, ref) })
		answers(t, "pay by bank "+o.name, o.now, o.later, func() (Result, error) {
			r, err := p.PayByBank(ctx, PayByBankRequest{ID: "pbb-" + o.name, AccountRef: gbp.Ref, Amount: amount, Payer: "Acme Ltd"})
			if err == nil && !strings.HasPrefix(r.AuthoriseURL, "https://pay-by-bank.test.invalid/") {
				t.Fatalf("pay by bank sends the payer to %q", r.AuthoriseURL)
			}
			return r.Result, err
		}, func(ref string) (Result, error) { return p.PaymentStatus(ctx, ref) })
	}
	if _, err := p.SendPayment(ctx, PaymentRequest{ID: "pay-success", AccountRef: gbp.Ref, Amount: Money{1, "GBP"}}); !errors.Is(err, ErrIDReused) {
		t.Fatalf("an id reused for another payment: %v", err)
	}
	if _, err := p.SendPayment(ctx, PaymentRequest{ID: "pay-eur", AccountRef: gbp.Ref, Amount: Money{100, "EUR"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("euros from a pounds account: %v", err)
	}

	// The statement: what moved, once however often it was asked; a return as out and back; nothing pending or failed.
	var got []int64
	for _, l := range must(p.StatementLines(ctx, gbp.Ref, start)) {
		got = append(got, l.Amount.Minor)
	}
	want := []int64{-10_000, 10_000, -10_014, 10_014, 10_014, -10_014}
	if len(got) != len(want) {
		t.Fatalf("statement = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statement = %v, want %v", got, want)
		}
	}
}

// B30.22 — the Test partner quotes an international payment at its test rates and cost, pays at the quote within its
// life, and refuses a payment that is not the quote's or whose quote has expired.
func TestAccountPartner_InternationalQuoteThenPay(t *testing.T) {
	ctx := context.Background()
	p := &TestAccountPartner{}
	gbp := must(p.OpenAccount(ctx, AccountRequest{ID: "acme-gbp", Holder: "Acme Ltd", Currency: "GBP"}))
	q := must(p.QuoteInternational(ctx, InternationalQuoteRequest{ID: "q1", AccountRef: gbp.Ref, Amount: Money{10_000, "GBP"}, Deliver: "EUR",
		Country: "DE", Payee: Payee{Name: "Bob", IBAN: "DE89370400440532013000"}}))
	// £100.00 at 1.25 / 1.10 dollars a unit is €113.63, rounded down.
	if q.Cost != (Money{testIntlCost, "GBP"}) || q.Delivers != (Money{11_363, "EUR"}) || q.Rate != "1.13636364" || q.ExpiresAt.IsZero() {
		t.Fatalf("quote = %+v", q)
	}
	if _, err := p.SendPayment(ctx, PaymentRequest{ID: "ip-other", AccountRef: gbp.Ref, Amount: Money{10_001, "GBP"}, QuoteRef: q.Ref}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a payment of another amount at the quote: %v", err)
	}
	r := must(p.SendPayment(ctx, PaymentRequest{ID: "ip1", AccountRef: gbp.Ref, Amount: Money{10_000, "GBP"}, QuoteRef: q.Ref, Payee: Payee{Name: "Bob"}}))
	if r.Status != StatusCompleted {
		t.Fatalf("paid at the quote: %+v", r)
	}
	if lines := must(p.StatementLines(ctx, gbp.Ref, time.Time{})); len(lines) != 1 || lines[0].Amount != (Money{-10_100, "GBP"}) {
		t.Fatalf("statement = %+v, want the amount and the cost out", lines)
	}
	p.book.now = func() time.Time { return time.Now().Add(2 * testQuoteLife) }
	if _, err := p.SendPayment(ctx, PaymentRequest{ID: "ip2", AccountRef: gbp.Ref, Amount: Money{10_000, "GBP"}, QuoteRef: q.Ref}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a payment at an expired quote: %v", err)
	}
}

func validIBAN(iban string) bool {
	digits := ""
	for _, r := range iban[4:] + iban[:4] {
		if r >= 'A' && r <= 'Z' {
			digits += big.NewInt(int64(r-'A') + 10).String()
		} else {
			digits += string(r)
		}
	}
	n, ok := new(big.Int).SetString(digits, 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

func TestFXPartner_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestFXPartner{}
	q := must(p.Quote(ctx, FXQuoteRequest{ID: "q-usdc", Sell: Money{100_00, "GBP"}, Buy: "USDC"}))
	if q.Buy != (Money{125_000_000, "USDC"}) || q.Rate != "1.25000000" {
		t.Fatalf("£100 to USDC = %+v at %s", q.Buy, q.Rate)
	}
	for _, o := range outcomes {
		q := must(p.Quote(ctx, FXQuoteRequest{ID: "q-" + o.name, Sell: Money{o.minor, "EUR"}, Buy: "GBP"}))
		answers(t, "convert "+o.name, o.now, o.later, func() (Result, error) {
			return p.Execute(ctx, FXExecution{ID: "fx-" + o.name, QuoteRef: q.Ref})
		}, func(ref string) (Result, error) { return p.Status(ctx, ref) })
	}
	p.book.now = func() time.Time { return time.Now().Add(2 * testQuoteLife) }
	if _, err := p.Execute(ctx, FXExecution{ID: "fx-late", QuoteRef: q.Ref}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an expired quote executed: %v", err)
	}
}

func TestBrokerPartner_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestBrokerPartner{}
	acct := must(p.OpenAccount(ctx, AccountRequest{ID: "deal", Holder: "Acme Ltd", Currency: "USD"}))
	price := must(p.Quote(ctx, "acme", "USD")).Price
	if price != must(p.Quote(ctx, "ACME", "USD")).Price || price.Minor < 10_00 || price.Minor > 1_000_00 {
		t.Fatalf("test price %v", price)
	}
	refs := map[string]string{}
	for _, o := range outcomes {
		answers(t, "buy "+o.name, o.now, o.later, func() (Result, error) {
			r, err := p.Place(ctx, Order{ID: "buy-" + o.name, AccountRef: acct.Ref, Symbol: "ACME", Side: SideBuy, Amount: Money{o.minor, "USD"}})
			refs[o.name] = r.Ref
			return r, err
		}, func(ref string) (Result, error) { return p.OrderStatus(ctx, ref) })
	}
	// Only the success holds: the returned fill is reversed, and the failed and pending ones never filled.
	pos := must(p.Positions(ctx, acct.Ref))
	if len(pos) != 1 || pos[0].QuantityMicros != 10_000*1_000_000/price.Minor || pos[0].Cost != (Money{10_000, "USD"}) {
		t.Fatalf("positions = %+v", pos)
	}
	if fills := must(p.Fills(ctx, acct.Ref, time.Time{})); len(fills) != 3 {
		t.Fatalf("fills = %+v, want the success, and the return and its reversal", fills)
	}
	if r := must(p.Cancel(ctx, refs["pending"])); r.Status != StatusCancelled {
		t.Fatalf("cancel = %+v", r)
	}
	if _, err := p.Cancel(ctx, refs["success"]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cancelled a filled order: %v", err)
	}
	if r := must(p.Place(ctx, Order{ID: "sell-too-much", AccountRef: acct.Ref, Symbol: "ACME", Side: SideSell,
		Amount: Money{20_000, "USD"}})); r.Status != StatusFailed {
		t.Fatalf("sold more than held: %+v", r)
	}
}

func TestStablecoinPartner_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestStablecoinPartner{}
	a := must(p.Address(ctx, AddressRequest{ID: "agent-1", Network: "base"}))
	if b := must(p.Address(ctx, AddressRequest{ID: "agent-1", Network: "base"})); a != b || len(a.Address) != 42 {
		t.Fatalf("addresses %+v and %+v", a, b)
	}
	for _, o := range outcomes {
		answers(t, "send "+o.name, o.now, o.later, func() (Result, error) {
			return p.Send(ctx, StablecoinTransfer{ID: "usdc-" + o.name, Network: "base", To: a.Address, Amount: Money{100_000_000 + o.minor, "USDC"}})
		}, func(ref string) (Result, error) { return p.Status(ctx, ref) })
	}
	if _, err := p.Send(ctx, StablecoinTransfer{ID: "gbp", Network: "base", To: a.Address, Amount: Money{100, "GBP"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("sent pounds as a stablecoin: %v", err)
	}
}

func TestKYCProvider_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestKYCProvider{}
	for _, o := range nameOutcomes {
		answers(t, "check "+o.name, o.now, o.later, func() (Result, error) {
			return p.StartCheck(ctx, KYCRequest{ID: "kyc-" + o.name, Subject: KYCPerson, Name: o.name, Country: "GB"})
		}, func(ref string) (Result, error) { return p.CheckResult(ctx, ref) })
	}
	if _, err := p.StartCheck(ctx, KYCRequest{ID: "co", Subject: KYCCompany, Name: "Acme Ltd"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a company check without its number: %v", err)
	}
}

// B30.4 — a contact check confirms an email and a phone and answers by the email; a company check names its
// directors and answers by the company's and its people's names together, so a director on a sanctions list fails it.
func TestKYCProvider_ContactAndCompanyChecks(t *testing.T) {
	ctx := context.Background()
	p := &TestKYCProvider{}
	if _, err := p.StartCheck(ctx, KYCRequest{ID: "c0", Subject: KYCContact, Email: "ada@example.com"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a contact check without a phone: %v", err)
	}
	if r := must(p.StartCheck(ctx, KYCRequest{ID: "c1", Subject: KYCContact, Email: "ada@example.com", Phone: "+447700900123"})); r.Status != StatusCompleted {
		t.Fatalf("a contact check: %+v", r)
	}
	if r := must(p.StartCheck(ctx, KYCRequest{ID: "c2", Subject: KYCContact, Email: "testfail@example.com", Phone: "+447700900123"})); r.Status != StatusFailed {
		t.Fatalf("a contact check on a TESTFAIL email: %+v", r)
	}
	if _, err := p.StartCheck(ctx, KYCRequest{ID: "co1", Subject: KYCCompany, Name: "Acme Ltd", CompanyNumber: "01234567"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a company check naming no directors: %v", err)
	}
	co := KYCRequest{ID: "co2", Subject: KYCCompany, Name: "Acme Ltd", Country: "GB", CompanyNumber: "01234567",
		Directors: []string{"Ada Lovelace"}, SignificantControl: []string{"Charles Babbage"}}
	if r := must(p.StartCheck(ctx, co)); r.Status != StatusCompleted {
		t.Fatalf("a company check: %+v", r)
	}
	co.ID, co.SignificantControl = "co3", []string{"Grace TESTSANCTION"}
	if r := must(p.StartCheck(ctx, co)); r.Status != StatusFailed {
		t.Fatalf("a company controlled by a sanctioned person: %+v", r)
	}
}

// A screening is an answer now, with nothing to come back: clear, a hit, held for review (pending), or unavailable.
func TestScreeningProvider_ClearHitReviewAndUnavailable(t *testing.T) {
	ctx := context.Background()
	p := TestScreeningProvider{}
	for name, want := range map[string]string{"Ada Lovelace": ScreenClear, "Ada TestSanction": ScreenHit, "Ada TESTPENDING": ScreenReview} {
		if s := must(p.ScreenName(ctx, NameScreen{Name: name})); s.Outcome != want {
			t.Fatalf("%s screens %s, want %s", name, s.Outcome, want)
		}
	}
	if _, err := p.ScreenName(ctx, NameScreen{Name: "TESTFAIL"}); !errors.Is(err, ErrScreeningUnavailable) {
		t.Fatalf("an unavailable screening: %v", err)
	}
	s := must(p.ScreenPayment(ctx, PaymentScreen{ID: "p1", Payer: "Acme TESTPENDING", Payee: "TESTSANCTION Corp", Amount: Money{100, "GBP"}}))
	if s.Outcome != ScreenHit || len(s.Matches) != 2 {
		t.Fatalf("a payment to a sanctioned payee screens %+v", s)
	}
}

// fakeList is a sanctions list of fixed matches by name (internal/screening is the real one).
type fakeList map[string][]ScreeningMatch

func (l fakeList) Match(_ context.Context, name string) ([]ScreeningMatch, error) {
	if name == "unloaded" {
		return nil, ErrScreeningUnavailable
	}
	return l[name], nil
}

// B30.6 — with a list, an exact match on a name or a good alias is a hit, a close one or an exact one on a weak alias
// is held for review, and a list that cannot answer makes the provider unavailable; the test words still work.
func TestScreeningProvider_ScreensAgainstTheList(t *testing.T) {
	ctx := context.Background()
	p := TestScreeningProvider{List: fakeList{
		"Banco Nacional de Cuba":  {{List: "OFAC", Entry: "306", ScoreBPS: 10_000}},
		"Banco Nacional de Cubaa": {{List: "OFAC", Entry: "306", ScoreBPS: 9_800}},
		"Mudir":                   {{List: "UK", Entry: "AFG0009", ScoreBPS: 10_000, Weak: true}},
	}}
	for name, want := range map[string]string{"Banco Nacional de Cuba": ScreenHit, "Banco Nacional de Cubaa": ScreenReview, "Mudir": ScreenReview,
		"Ada Lovelace": ScreenClear, "Ada TESTSANCTION": ScreenHit} {
		if s := must(p.ScreenName(ctx, NameScreen{Name: name})); s.Outcome != want {
			t.Fatalf("%s screens %s, want %s", name, s.Outcome, want)
		}
	}
	if _, err := p.ScreenName(ctx, NameScreen{Name: "unloaded"}); !errors.Is(err, ErrScreeningUnavailable) {
		t.Fatalf("a list that cannot answer: %v", err)
	}
	s := must(p.ScreenPayment(ctx, PaymentScreen{ID: "p1", Payee: "Banco Nacional de Cuba", Amount: Money{100, "GBP"}}))
	if s.Outcome != ScreenHit || len(s.Matches) != 1 {
		t.Fatalf("a payment to a listed payee, the payer our own: %+v", s)
	}
	if _, err := p.ScreenPayment(ctx, PaymentScreen{ID: "p2", Amount: Money{100, "GBP"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a payment screening naming nobody: %v", err)
	}
}

func TestCapitalPartner_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestCapitalPartner{}
	if _, err := p.Offer(ctx, CapitalRequest{ID: "person", Company: "Ada", Amount: Money{100, "GBP"}, TermDays: 30}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("credit offered without a company number: %v", err)
	}
	for _, o := range outcomes {
		offer := must(p.Offer(ctx, CapitalRequest{ID: "offer-" + o.name, Company: "Acme Ltd", CompanyNumber: "01234567",
			Amount: Money{o.minor, "GBP"}, TermDays: 30}))
		if offer.Fee.Minor != 0 {
			t.Fatalf("the Test partner charged %v", offer.Fee)
		}
		if o.now != StatusCompleted { // declined, or under review: nothing to fund
			if offer.Status != o.now {
				t.Fatalf("offer %s = %s, want %s", o.name, offer.Status, o.now)
			}
			if _, err := p.Fund(ctx, Funding{ID: "fund-" + o.name, OfferRef: offer.Ref, AccountRef: "acct"}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("funded a %s offer: %v", o.name, err)
			}
			continue
		}
		answers(t, "fund "+o.name, o.now, o.later, func() (Result, error) {
			return p.Fund(ctx, Funding{ID: "fund-" + o.name, OfferRef: offer.Ref, AccountRef: "acct"})
		}, func(ref string) (Result, error) { return p.Status(ctx, ref) })
	}
	funded := must(p.Fund(ctx, Funding{ID: "fund-success", OfferRef: testRef("offer", "offer-success"), AccountRef: "acct"}))
	for _, o := range outcomes {
		answers(t, "repay "+o.name, o.now, o.later, func() (Result, error) {
			return p.Repay(ctx, Repayment{ID: "repay-" + o.name, FundingRef: funded.Ref, Amount: Money{o.minor, "GBP"}})
		}, func(ref string) (Result, error) { return p.Status(ctx, ref) })
	}
}

func TestInsurerPartner_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestInsurerPartner{}
	q := must(p.Quote(ctx, CoverRequest{ID: "q", Insured: "Acme Ltd", Cover: Money{50_000, "GBP"}, TermDays: 365, Risk: "agent mistakes"}))
	if q.Premium.Minor != 0 {
		t.Fatalf("the Test insurer charged %v", q.Premium)
	}
	policy := must(p.Bind(ctx, BindRequest{ID: "policy", QuoteRef: q.Ref}))
	if policy.Status != StatusCompleted || policy.To.Sub(policy.From) < 364*24*time.Hour {
		t.Fatalf("policy = %+v", policy)
	}
	for _, o := range outcomes {
		answers(t, "claim "+o.name, o.now, o.later, func() (Result, error) {
			return p.Claim(ctx, ClaimRequest{ID: "claim-" + o.name, PolicyRef: policy.Ref, Amount: Money{o.minor, "GBP"}, What: "a wrong order"})
		}, func(ref string) (Result, error) { return p.ClaimStatus(ctx, ref) })
	}
	if r := must(p.Claim(ctx, ClaimRequest{ID: "too-much", PolicyRef: policy.Ref, Amount: Money{50_001, "GBP"}})); r.Status != StatusFailed {
		t.Fatalf("a claim over the cover: %+v", r)
	}
}

func TestAgentTokenProvider_SuccessFailureReturnAndPending(t *testing.T) {
	ctx := context.Background()
	p := &TestAgentTokenProvider{}
	exp := time.Now().Add(time.Hour)
	for _, o := range nameOutcomes {
		answers(t, "provision "+o.name, o.now, o.later, func() (Result, error) {
			tok, err := p.Provision(ctx, AgentTokenRequest{ID: "tok-" + o.name, AgentID: "agent-1", AgentName: o.name, Currency: "GBP", ExpiresAt: exp})
			return tok.Result, err
		}, func(ref string) (Result, error) { return p.Status(ctx, ref) })
	}
	tok := must(p.Provision(ctx, AgentTokenRequest{ID: "tok-Ada Lovelace", AgentID: "agent-1", AgentName: "Ada Lovelace", Currency: "GBP", ExpiresAt: exp}))
	if tok.Network != "test" || len(tok.Last4) != 4 {
		t.Fatalf("token = %+v", tok)
	}
	for _, o := range outcomes {
		answers(t, "authorise "+o.name, o.now, o.later, func() (Result, error) {
			return p.Authorise(ctx, Authorisation{ID: "auth-" + o.name, TokenRef: tok.Ref, Amount: Money{o.minor, "GBP"}, Merchant: "shop"})
		}, func(ref string) (Result, error) { return p.Status(ctx, ref) })
	}
	if err := p.Revoke(ctx, tok.Ref); err != nil {
		t.Fatal(err)
	}
	if r := must(p.Authorise(ctx, Authorisation{ID: "after-revoke", TokenRef: tok.Ref, Amount: Money{100, "GBP"}})); r.Status != StatusFailed {
		t.Fatalf("a revoked token authorised: %+v", r)
	}
}
