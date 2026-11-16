package economy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// agent_rules_currency.go — B61.12: AN AGENT'S RULES ARE HELD AND JUDGED IN THE ACCOUNT'S OWN CURRENCY.
//
// An agent's rules carry the currency the owner typed them in, and their limits per payment, hour, day, week and month
// and the amount above which a payment asks the owner, in its minor units (migration 0393). Every movement the rules
// judge is valued in that currency by judgedAmount, the one function they all go through — payments out, Instant
// transfers, card spends, mandates, invoices, conversions, payments to other agents, "Would it pass?" — so a euro
// limit holds in euros whatever the rate does: a payment in the rules' currency is judged on its own amount with no
// conversion, and any other at what it is worth in it at the conversion provider's rate, rounded up, so rounding never
// lets a payment through. LXC is valued as dollars at the peg.
//
// The µLXC limits stay readable for the parked gateway: a save in dollars writes them at the peg, exact, and a save
// in pounds or euros writes them at the rate, rounded down. A save that names no currency — every client before this
// — names the µLXC limits, and is moved to dollars at the peg.

// currency is the currency r is held and judged in: dollars for rules saved before B61.12 or never saved.
func (r AgentRules) currency() string {
	if r.Currency == "" {
		return CurrencyUSD
	}
	return r.Currency
}

// rulesAmount is minor units of the rules' currency for a person: "€500.00".
func (r AgentRules) rulesAmount(minor int64) string { return Money(minor, r.currency()) }

// ulxcPerCent is how many µLXC one US cent buys at the peg (100,000 at $0.10).
var ulxcPerCent = int64(math.Round(1e4 / LXCUSDValue))

// centsAtPeg is a µLXC limit in US cents at the peg: exact in whole cents; a part of a cent keeps its whole cents but
// never fewer than one, so no limit becomes none (migration 0393 moves the stored rules the same way).
func centsAtPeg(ulxc int64) int64 {
	if ulxc <= 0 {
		return 0
	}
	return max(ulxc/ulxcPerCent, 1)
}

func centsAtPegOf(ulxc *int64) *int64 {
	if ulxc == nil {
		return nil
	}
	v := centsAtPeg(*ulxc)
	return &v
}

// dollarLike: USD and USDC are one dollar each, converted by their decimals with no rate.
func dollarLike(currency string) bool { return currency == CurrencyUSD || currency == CurrencyUSDC }

// rulesRate is what one whole unit of from buys in to at the conversion provider's rate, as it states it.
func (s *DualTokenStore) rulesRate(ctx context.Context, workspaceID, from, to string) (*big.Rat, error) {
	if s.accountPartners == nil {
		return nil, fmt.Errorf("economy: no conversion provider to value %s in %s", from, to)
	}
	conv, err := s.accountPartners.FX(ctx, CapabilityFX)
	if err != nil {
		return nil, fmt.Errorf("economy: value in the rules' currency: %w", err)
	}
	if from == CurrencyUSDC {
		from = CurrencyUSD
	}
	if to == CurrencyUSDC {
		to = CurrencyUSD
	}
	q, err := conv.Quote(partners.WithWorkspace(ctx, workspaceID), partners.FXQuoteRequest{ID: "rules:" + uuid.NewString(),
		Sell: partners.Money{Minor: pow10(MoneyCurrencies[from]).Int64(), Currency: from}, Buy: to})
	if err != nil {
		return nil, fmt.Errorf("economy: value in the rules' currency: %w", err)
	}
	rate, ok := new(big.Rat).SetString(q.Rate)
	if !ok || rate.Sign() <= 0 {
		return nil, fmt.Errorf("economy: value in the rules' currency: the rate %q is not a positive number", q.Rate)
	}
	return rate, nil
}

// convertMinor is minor units of from in to's minor units at the conversion provider's rate (none between dollars),
// rounded up when up and down otherwise.
func (s *DualTokenStore) convertMinor(ctx context.Context, workspaceID string, minor int64, from, to string, up bool) (int64, error) {
	if minor <= 0 || from == to {
		return max(minor, 0), nil
	}
	rate := big.NewRat(1, 1)
	if !dollarLike(from) || !dollarLike(to) {
		var err error
		if rate, err = s.rulesRate(ctx, workspaceID, from, to); err != nil {
			return 0, err
		}
	}
	// to minor = from minor × rate × 10^(to's decimals − from's decimals)
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(minor), rate)
	v.Mul(v, new(big.Rat).SetFrac(pow10(MoneyCurrencies[to]), pow10(MoneyCurrencies[from])))
	return ratInt64(v, up)
}

// ratInt64 is v as an int64, rounded up when up and down otherwise.
func ratInt64(v *big.Rat, up bool) (int64, error) {
	q, m := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
	if up && m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("%w: the amount is more than the rules can count", ErrAgentRule)
	}
	return q.Int64(), nil
}

// ulxcIn is ulxc µLXC in currency's minor units: dollars at the peg, then at the rate, rounded up when up.
func (s *DualTokenStore) ulxcIn(ctx context.Context, workspaceID string, ulxc int64, currency string, up bool) (int64, error) {
	if ulxc <= 0 {
		return 0, nil
	}
	usdMicro, err := ratInt64(big.NewRat(ulxc, ULXCPerUSDMicro), up) // millionths of a dollar are USDC's minor units
	if err != nil {
		return 0, err
	}
	return s.convertMinor(ctx, workspaceID, usdMicro, CurrencyUSDC, currency, up)
}

// ownMoney is the movement req judges in its own currency: req.Own, or else the currency account it is paid from
// (req.PaidFrom); ok false for LXC — the parked gateway's questions, marketplace uses, LXC payments and the agent card,
// which draws on LXC — which the rules judge in µLXC as before, since such a movement can cost less than a cent.
func ownMoney(req AgentRequest) (partners.Money, bool) {
	own := req.Own
	if from := req.PaidFrom; own.Minor <= 0 && from.Kind == "account" {
		own = partners.Money{Minor: from.AmountMinor, Currency: from.Currency}
	}
	_, ok := MoneyCurrencies[own.Currency]
	return own, ok && own.Minor > 0
}

// judgedLimits are the amount rules as one movement is judged by them: the movement and the limits in one unit, and
// what has been spent since a time in it. Rules in pounds or euros record what a movement from a currency account was
// worth in their currency as they let it through (B61.41): recordedIn is that currency ("" for none) and worth the
// amount recorded (0 when it was not valued).
type judgedLimits struct {
	amount, perPayment, approvalAbove int64
	periods                           []agentPeriodLimit
	words                             func(int64) string
	spentIn                           func(agentSpend) (int64, error)
	recordedIn                        string
	worth                             int64
}

// judgedAmount is how r judges a movement of amount µLXC (B61.12), the one valuation every amount rule uses: money
// from a currency account in the rules' currency on its own amount, exactly; money in another currency at what it is
// worth in the rules' currency at the conversion provider's rate, rounded up, so rounding never lets it through; and
// LXC in µLXC against the µLXC limits, as ever. What was spent is counted in the same unit, rounded up.
func (s *DualTokenStore) judgedAmount(ctx context.Context, workspaceID string, r AgentRules, req AgentRequest, amount int64,
	now time.Time, loc *time.Location) (judgedLimits, error) {
	own, ok := ownMoney(req)
	if !ok {
		return judgedLimits{amount: amount, perPayment: r.MaxPerRequestULXC, approvalAbove: r.ApprovalAboveULXC,
			periods: r.periodLimitsULXC(now, loc), words: LXCMoney, spentIn: func(so agentSpend) (int64, error) { return so.ulxc, nil }}, nil
	}
	j := judgedLimits{perPayment: r.MaxPerPaymentMinor, approvalAbove: r.ApprovalAboveMinor, periods: r.periodLimits(now, loc),
		words: r.rulesAmount, spentIn: func(so agentSpend) (int64, error) {
			rest, err := s.ulxcIn(ctx, workspaceID, so.restULXC, r.currency(), true)
			return so.recordedMinor + rest, err
		}}
	// B61.41: dollar rules count the µLXC spent at the peg, which no rate moves; pounds and euros count what each movement
	// was worth in them when they let it through.
	if !dollarLike(r.currency()) {
		j.recordedIn = r.currency()
		if own.Currency == r.currency() {
			j.worth = own.Minor
		}
	}
	if j.perPayment == 0 && j.approvalAbove == 0 && !slices.ContainsFunc(j.periods, func(l agentPeriodLimit) bool { return l.amount > 0 }) {
		return j, nil // no amount rule to judge it by, so nothing to value
	}
	var err error
	j.amount, err = s.convertMinor(ctx, workspaceID, own.Minor, own.Currency, r.currency(), true)
	if j.recordedIn != "" {
		j.worth = j.amount
	}
	return j, err
}

// ErrRulesCurrency: the rules are saved in a currency other than the one the save names its limits in.
var ErrRulesCurrency = errors.New("economy: the agent's rules are in another currency")

// rulesInCurrency makes r, as a save names it, rules held in a currency, each amount limit in both its forms. A save
// that names no currency names its limits in µLXC, as every client before B61.12 does: a limit it changes is moved to
// dollars at the peg. One that names a currency names each limit in it, and the µLXC form is written for the parked
// gateway — at the peg for dollars, at the rate rounded down otherwise. A limit it leaves as saved but whose µLXC form
// it changes to another amount than 0 — a client that reads the rules and writes them back with a µLXC limit changed —
// is moved at the peg on dollar rules and refused on others, which a µLXC limit cannot say; a µLXC form of 0 there is
// a client that does not name it, and changes nothing. A change of currency names every limit in the new one.
func (s *DualTokenStore) rulesInCurrency(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, r *AgentRules) error {
	saved := struct {
		currency                                          string
		perPayment, hourly, daily, weekly, monthly, asks  int64
		perRequestULXC, hourlyULXC, dailyULXC, weeklyULXC int64
		monthlyULXC, asksULXC                             int64
	}{currency: CurrencyUSD}
	err := tx.QueryRow(ctx, `SELECT currency, COALESCE(max_per_payment_minor, 0), COALESCE(hourly_limit_minor, 0), COALESCE(daily_limit_minor, 0),
		COALESCE(weekly_limit_minor, 0), COALESCE(monthly_limit_minor, 0), COALESCE(approval_above_minor, 0),
		COALESCE(max_per_request_ulxc, 0), COALESCE(hourly_limit_ulxc, 0), COALESCE(daily_limit_ulxc, 0), COALESCE(weekly_limit_ulxc, 0),
		COALESCE(monthly_limit_ulxc, 0), COALESCE(approval_above_ulxc, 0) FROM agent_rules WHERE agent_id = $1 AND workspace_id = $2`,
		agentID, workspaceID).Scan(&saved.currency, &saved.perPayment, &saved.hourly, &saved.daily, &saved.weekly, &saved.monthly,
		&saved.asks, &saved.perRequestULXC, &saved.hourlyULXC, &saved.dailyULXC, &saved.weeklyULXC, &saved.monthlyULXC, &saved.asksULXC)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("economy: set agent rules: %w", err)
	}
	legacy := r.Currency == ""
	if legacy {
		r.Currency = saved.currency
	}
	if _, ok := MoneyCurrencies[r.Currency]; !ok {
		return fmt.Errorf("%w: rules are held in GBP, EUR, USD or USDC, not %q", ErrAgentRule, r.Currency)
	}
	sameCurrency := r.Currency == saved.currency
	// one is a limit as the save leaves it, from what it names (nil: an hourly or weekly limit it leaves out) and what
	// is saved; both nil keeps it as saved.
	one := func(name string, minor, ulxc *int64, savedMinor, savedULXC int64) (*int64, *int64, error) {
		if legacy {
			minor = nil
		}
		switch {
		case minor != nil && (*minor != savedMinor || !sameCurrency): // as the owner typed it, in the currency
			v, err := s.minorInLXC(ctx, workspaceID, *minor, r.Currency)
			return minor, &v, err
		case ulxc != nil && *ulxc != savedULXC && (legacy || *ulxc > 0): // named in µLXC
			if r.Currency != CurrencyUSD {
				return nil, nil, fmt.Errorf("%w: they are in %s; name the %s in it", ErrRulesCurrency, r.Currency, name)
			}
			v := centsAtPeg(*ulxc)
			return &v, ulxc, nil
		case !sameCurrency && savedMinor > 0:
			return nil, nil, fmt.Errorf("%w: they are in %s; a change to %s names the %s in it too", ErrRulesCurrency, saved.currency, r.Currency, name)
		case minor == nil && ulxc == nil:
			return nil, nil, nil
		}
		return &savedMinor, &savedULXC, nil
	}
	for _, l := range []struct {
		name                  string
		minor, ulxc           *int64
		savedMinor, savedULXC int64
	}{
		{"limit per payment", &r.MaxPerPaymentMinor, &r.MaxPerRequestULXC, saved.perPayment, saved.perRequestULXC},
		{"daily limit", &r.DailyLimitMinor, &r.DailyLimitULXC, saved.daily, saved.dailyULXC},
		{"monthly limit", &r.MonthlyLimitMinor, &r.MonthlyLimitULXC, saved.monthly, saved.monthlyULXC},
		{"amount to ask above", &r.ApprovalAboveMinor, &r.ApprovalAboveULXC, saved.asks, saved.asksULXC},
	} {
		minor, ulxc, err := one(l.name, l.minor, l.ulxc, l.savedMinor, l.savedULXC)
		if err != nil {
			return err
		}
		*l.minor, *l.ulxc = *minor, *ulxc
	}
	if r.HourlyLimitMinor, r.HourlyLimitULXC, err = one("hourly limit", r.HourlyLimitMinor, r.HourlyLimitULXC, saved.hourly, saved.hourlyULXC); err != nil {
		return err
	}
	r.WeeklyLimitMinor, r.WeeklyLimitULXC, err = one("weekly limit", r.WeeklyLimitMinor, r.WeeklyLimitULXC, saved.weekly, saved.weeklyULXC)
	return err
}

// minorInLXC is minor units of currency in µLXC, for the parked gateway: dollars at the peg, exact; pounds and euros
// at the rate, rounded down — never below 1 µLXC for a limit there is.
func (s *DualTokenStore) minorInLXC(ctx context.Context, workspaceID string, minor int64, currency string) (int64, error) {
	if minor <= 0 {
		return 0, nil
	}
	usdMicro, err := s.convertMinor(ctx, workspaceID, minor, currency, CurrencyUSDC, false)
	if err != nil {
		return 0, err
	}
	v, err := ratInt64(new(big.Rat).SetInt(new(big.Int).Mul(big.NewInt(usdMicro), big.NewInt(ULXCPerUSDMicro))), false)
	return max(v, 1), err
}
