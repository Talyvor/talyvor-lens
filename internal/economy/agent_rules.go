package economy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // active hours are read in any IANA zone; the runtime image carries no zoneinfo

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// agent_rules.go — B19.2: SPENDING RULES PER AGENT, ENFORCED BEFORE THE PROVIDER IS CALLED.
//
// The rules are judged inside agentMovement — the hold or the debit of an agent's key, under the
// agent's row lock — so they bind every seam that spends an agent's balance (the reservation and the
// immediate debit, buffered and streamed), and concurrent requests cannot together pass a daily or
// monthly limit. What the rules need to know about the request beyond its amount — model, provider,
// fingerprint — rides the context (WithAgentRequest). A request above the approval amount files an
// approval (migration 0141) and is refused until the workspace's owner approves it; the approved
// request is then let through once.

// ErrAgentRule wraps every refusal by an agent's rules except the approval amount's.
var ErrAgentRule = errors.New("economy: the agent's spending rules refuse this request")

// ErrAgentRequestRate is the requests-per-minute rule's refusal (B28.302): an ErrAgentRule, which the proxy answers
// 429 rather than 403, because the same request goes through once the minute has room again.
var ErrAgentRequestRate = fmt.Errorf("%w", ErrAgentRule)

// ErrApprovalRequired is the approval amount's refusal; *ApprovalNeededError carries the approval.
var ErrApprovalRequired = errors.New("economy: this request needs a person's approval")

// ErrApprovalDeclined is the refusal of a request the workspace's owner declined within the approval life (B55.18):
// asking again the same way neither files an approval nor pushes.
var ErrApprovalDeclined = errors.New("economy: the workspace's owner declined this request")

// approvalDeclined is ErrApprovalDeclined saying when, why and until when. It is ErrAgentRule too, so every caller
// refuses it as it refuses a rule (403), while its words name the owner's decline rather than the spending rules.
type approvalDeclined struct {
	at, until time.Time
	reason    string
}

func (e approvalDeclined) Error() string {
	const day, clock = "2 Jan 2006", "15:04 UTC"
	reason := ""
	if e.reason != "" {
		reason = ": " + e.reason
	}
	at, until := e.at.UTC(), e.until.UTC()
	return fmt.Sprintf("%s on %s at %s%s; the same request is refused until %s at %s",
		ErrApprovalDeclined, at.Format(day), at.Format(clock), reason, until.Format(day), until.Format(clock))
}

func (e approvalDeclined) Is(target error) bool {
	return target == ErrApprovalDeclined || target == ErrAgentRule
}

// ErrApprovalNotFound: no such open approval in this workspace.
var ErrApprovalNotFound = errors.New("economy: no such pending approval in this workspace")

// AgentRules are one agent's spending rules. A zero amount, an empty list or an empty window is no rule.
type AgentRules struct {
	MaxPerRequestULXC int64 `json:"max_per_request_ulxc"`
	// HourlyLimitULXC and WeeklyLimitULXC (B28.300) count the clock hour and the week from Monday. Read, they
	// are never nil; nil (absent from the JSON) saves the rules without changing them, like AllowedListings.
	HourlyLimitULXC   *int64   `json:"hourly_limit_ulxc"`
	DailyLimitULXC    int64    `json:"daily_limit_ulxc"`
	WeeklyLimitULXC   *int64   `json:"weekly_limit_ulxc"`
	MonthlyLimitULXC  int64    `json:"monthly_limit_ulxc"`
	ApprovalAboveULXC int64    `json:"approval_above_ulxc"`
	AllowedModels     []string `json:"allowed_models"`
	AllowedProviders  []string `json:"allowed_providers"`
	AllowedListings   []string `json:"allowed_listings"` // marketplace listings it may use (B19.14); empty allows any
	ActiveFrom        string   `json:"active_from"`      // "HH:MM" in Timezone; with ActiveUntil, the hours it may spend
	ActiveUntil       string   `json:"active_until"`     // exclusive; earlier than ActiveFrom means the window crosses midnight
	Timezone          string   `json:"timezone"`         // IANA name; UTC when empty. Days and months are counted in it too
	// PauseOnUnusualSpend pauses the agent when its spend raises an unusual-spend alert (B19.6).
	PauseOnUnusualSpend bool `json:"pause_on_unusual_spend"`
	// ModelDailyLimitsULXC (B28.301) caps what the agent spends on one model, named as it asks for it, in a day.
	// Read, it is never nil; nil (absent from the JSON) saves the rules without changing it. Saved, it replaces
	// the stored caps whole: a model left out, or capped at zero, has no cap.
	ModelDailyLimitsULXC map[string]int64 `json:"model_daily_limits_ulxc"`
	// RequestsPerMinute (B28.302) caps the questions the agent asks in any sixty seconds, and (B50.4) its MCP tool
	// calls in any sixty seconds, counted apart. Read, it is never nil;
	// nil (absent from the JSON) saves the rules without changing it.
	RequestsPerMinute *int64 `json:"requests_per_minute"`
	// AllowedPayees and BlockedPayees (B28.303) name, by id, who the agent may pay and who it may not: an agent
	// (agt_…), a listing (lst_…), a company (its workspace id: its agents and listings too) or a card merchant. Once
	// AllowedPayees names any, a payee it does not name is refused. Read, they are never nil; nil (absent from the
	// JSON) saves the rules without changing them.
	AllowedPayees []string `json:"allowed_payees"`
	BlockedPayees []string `json:"blocked_payees"`
	// PayeeDailyLimitsULXC (B28.304) caps what the agent pays one payee, named by id as the payee lists name it, in
	// a day. Read, it is never nil; nil (absent from the JSON) saves the rules without changing it. Saved, it
	// replaces the stored caps whole: a payee left out, or capped at zero, has no cap.
	PayeeDailyLimitsULXC map[string]int64 `json:"payee_daily_limits_ulxc"`
	// PayeeMonthlyLimitsULXC (B41.104) is PayeeDailyLimitsULXC over the month, in Timezone.
	PayeeMonthlyLimitsULXC map[string]int64 `json:"payee_monthly_limits_ulxc"`
	// PayeeAllowances (B41.104) are the owner's "always allow this": by payee id, as the payee lists name it, what
	// the agent may pay that payee in a day or a month without a person's approval. Past it, a payment above the
	// approval amount waits for approval again. Read, it is never nil; nil (absent from the JSON) saves the rules
	// without changing it. Saved, it replaces the stored allowances whole.
	PayeeAllowances map[string]PayeeAllowance `json:"payee_allowances"`
	// MaxCommitmentULXC, AllowedLicences and MaySubscribe (B32.22) bound the marketplace licences the agent may take:
	// the most one may commit it to (a buy's or a rent's price, one subscription period's), the licences it may hold —
	// commercial and enterprise; personal is never an agent's, and empty allows both — and whether it may subscribe at
	// all — or take a rent that renews itself — which it may not until its owner says so. Read, they are never nil; nil
	// (absent from the JSON) saves the rules without changing them.
	MaxCommitmentULXC *int64   `json:"max_commitment_ulxc"`
	AllowedLicences   []string `json:"allowed_licences"`
	MaySubscribe      *bool    `json:"may_subscribe"`
	// AllowedCurrencies and FXDailyLimitULXC (B30.29) bound its currency conversions: the currencies it may convert
	// between — the one sold and the one bought; empty allows any — and the most it may convert in a day, valued as its
	// payments are. Read, they are never nil; nil (absent from the JSON) saves the rules without changing them.
	AllowedCurrencies []string `json:"allowed_currencies"`
	FXDailyLimitULXC  *int64   `json:"fx_daily_limit_ulxc"`
	// MayInstantTransfer (B59.15) is "Can use Instant transfer": whether the agent may send an Instant transfer at all,
	// which it may not until its owner says so. Read, it is never nil; nil (absent from the JSON) saves the rules
	// without changing it.
	MayInstantTransfer *bool `json:"may_instant_transfer"`
	// AllowedMerchantCategories and BlockedMerchantCategories (B61.43) are its "Merchants and categories": the merchant
	// categories it may pay in by card — once it names any, a card payment in another is refused — and those it may
	// never pay in, each a MerchantCategories id or a four-digit merchant category code. Read, they are never nil; nil
	// (absent from the JSON) saves the rules without changing them.
	AllowedMerchantCategories []string `json:"allowed_merchant_categories"`
	BlockedMerchantCategories []string `json:"blocked_merchant_categories"`
	// Currency (B61.12) is the currency the rules are held and judged in: the account's, as the owner typed them. The
	// limits below are in its minor units, and every movement is judged by them (judgedAmount): one in this currency on
	// its own amount, any other at what it is worth in it at the conversion provider's rate, rounded up. The µLXC limits
	// above are what these are worth in LXC, for the parked gateway to read. A save without Currency names the µLXC
	// limits, and is moved to dollars at the peg.
	Currency           string `json:"currency"`
	MaxPerPaymentMinor int64  `json:"max_per_payment_minor"`
	// HourlyLimitMinor and WeeklyLimitMinor are, like their µLXC forms, never nil read; nil saves the rules without
	// changing them.
	HourlyLimitMinor   *int64 `json:"hourly_limit_minor"`
	DailyLimitMinor    int64  `json:"daily_limit_minor"`
	WeeklyLimitMinor   *int64 `json:"weekly_limit_minor"`
	MonthlyLimitMinor  int64  `json:"monthly_limit_minor"`
	ApprovalAboveMinor int64  `json:"approval_above_minor"`
	// PayeeDailyLimitsMinor, PayeeMonthlyLimitsMinor and FXDailyLimitMinor (B61.40) are each payee's caps and the daily
	// conversion maximum in the minor units of Currency, judged as the limits above are; PayeeDailyLimitsULXC,
	// PayeeMonthlyLimitsULXC and FXDailyLimitULXC are what they are worth in LXC. Read, they are never nil; nil saves
	// the rules without changing them, and a map saved replaces the stored caps whole.
	PayeeDailyLimitsMinor   map[string]int64 `json:"payee_daily_limits_minor"`
	PayeeMonthlyLimitsMinor map[string]int64 `json:"payee_monthly_limits_minor"`
	FXDailyLimitMinor       *int64           `json:"fx_daily_limit_minor"`
}

// PayeeAllowance is one payee's "always allow" (B41.104): up to Minor of the rules' currency (B61.40) a day or a month
// (Per) without approval. ULXC is what that is worth in LXC; one named in ULXC alone is moved to dollars at the peg.
type PayeeAllowance struct {
	ULXC  int64  `json:"amount_ulxc"`
	Minor int64  `json:"amount_minor"`
	Per   string `json:"per"` // day | month
}

// AgentLicences are the marketplace licences an agent may hold (B32.22): a personal licence is one person's.
var AgentLicences = []string{"commercial", "enterprise"}

// Commitment is what a marketplace licence commits an agent to (B32.22): its kind — buy, rent or subscribe — its
// licence, its price — a buy's or a rent's, or one subscription period's — and whether it renews itself, as a
// subscription and a rent set to renew do. A use commits to nothing beyond itself, and is the zero Commitment.
type Commitment struct {
	Kind    string `json:"kind"`
	Licence string `json:"licence"`
	Renews  bool   `json:"renews"`
	ULXC    int64  `json:"-"`
}

// AgentRequest is what the rules judge about a request besides its amount.
type AgentRequest struct {
	Model       string
	Provider    string
	Fingerprint string    // the key, model and prompt, hashed: what an approval is for
	At          time.Time // zero means now
	Payment     bool      // a payment to another agent (B19.3) or a card purchase (B19.12): no model or provider to judge
	Listing     string    // the marketplace listing a use is of (B19.14)
	Payee       Payee     // who a payment goes to, and Memo why: the approval it files names both (B23.5)
	Memo        string
	Commitment  Commitment // the marketplace licence a payment takes, if it takes one (B32.22)
	PaidFrom    PaidFrom   // what pays it, for the approval it files (B57.11); zero is the agent's LXC, or the bill for a listing
	// ByCard: a card network's authorisation of a payment at a merchant (B61.43), whose issuer reported the merchant's
	// category code as MerchantCategory ("" or "unknown" when it did not); the rules' merchant categories judge it.
	ByCard           bool
	MerchantCategory string
	// Approved: the owner signed for this payment in advance — a pull within the mandate they approved (B55.24) — so
	// it needs no approval of its own. Every other rule still judges it.
	Approved bool
	// Own is the movement in its own currency when it moves money from a currency account or a card, as the rules
	// judge it (B61.12, judgedAmount); zero for LXC, and PaidFrom's account or card amount stands in for it.
	Own partners.Money
}

// PaidFrom is what pays an approval's payment (B57.11): the agent's LXC balance, one of its currency accounts, the
// company's monthly marketplace bill or the agent's card, and the amount in that one's currency. Name and BalanceMinor
// are read with the approval: a bill has no balance, nor has a card, which draws on the agent's LXC.
type PaidFrom struct {
	Kind         string `json:"kind"` // lxc | account | marketplace_bill | card
	AccountID    string `json:"account_id"`
	Name         string `json:"name"`
	Currency     string `json:"currency"`
	AmountMinor  int64  `json:"amount_minor"`
	BalanceMinor *int64 `json:"balance_minor"`
	funding      string // an account's test or live money, whose balance is shown
}

// paidFromName names what pays in words, as the approval sheet shows it.
func paidFromName(kind, agent, currency string) string {
	switch kind {
	case "account":
		return agent + "’s " + cmp.Or(currencyWords[currency], currency) + " account"
	case "marketplace_bill":
		return "Your monthly marketplace bill"
	case "card":
		return agent + "’s card"
	}
	return agent + "’s balance"
}

// currencyWords are the currencies an agent's account holds, by their ISO 4217 names.
var currencyWords = map[string]string{"USD": "US dollar", "EUR": "euro", "GBP": "pound sterling", "USDC": "USDC"}

// Payee is who a payment goes to. Name is the one it had when the approval was filed; empty until then
// (namePayee reads it) unless the caller already knows it.
type Payee struct {
	Kind string `json:"kind"` // agent | listing | company | merchant
	ID   string `json:"id"`
	Name string `json:"name"`
}

// namePayee reads p's name when the caller did not give one; a payee that no longer exists keeps none.
func namePayee(ctx context.Context, q pgxDB, p Payee) (Payee, error) {
	query := map[string]string{
		"agent":   `SELECT name FROM agent_accounts WHERE id = $1`,
		"listing": `SELECT title FROM market_listings WHERE id = $1`,
		"company": `SELECT name FROM workspaces WHERE id = $1`,
	}[p.Kind]
	if p.Name != "" || p.ID == "" || query == "" {
		return p, nil
	}
	if err := q.QueryRow(ctx, query, p.ID).Scan(&p.Name); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("economy: payee name: %w", err)
	}
	return p, nil
}

type agentRequestKey struct{}

// WithAgentRequest carries req to the agent's hold or debit made with ctx.
func WithAgentRequest(ctx context.Context, req AgentRequest) context.Context {
	return context.WithValue(ctx, agentRequestKey{}, req)
}

type denyReasonKey struct{}

// WithDenyReason carries what the owner wrote when declining (B55.18) to DecideAgentApproval and DecideAgentApprovalSigned.
func WithDenyReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, denyReasonKey{}, reason)
}

func denyReasonFrom(ctx context.Context) string {
	r, _ := ctx.Value(denyReasonKey{}).(string)
	return r
}

func agentRequestFrom(ctx context.Context) AgentRequest {
	req, _ := ctx.Value(agentRequestKey{}).(AgentRequest)
	if req.At.IsZero() {
		req.At = time.Now()
	}
	return req
}

// ApprovalNeededError is the refusal of a request above the agent's approval amount. ApprovalID is the
// approval filed for it, which the workspace's owner approves or denies.
type ApprovalNeededError struct {
	ApprovalID  string
	AmountULXC  int64
	Expired     bool // the request's approval was past its life (B50.18): this is a fresh one
	workspaceID string
	agentID     string
	req         AgentRequest
	asksAbove   int64  // the approval amount it is above (B57.11)
	monthLeft   *int64 // the monthly limit less the month's spend and this amount; nil with no monthly limit
	inRules     rulesSheet
	rule        string // the rule that asks the owner (B62.1): ProofRuleApprovalAbove or ProofRulePrepared
}

// rulesSheet is what an approval's sheet says of the rules, in their currency (B61.42): the amount they ask above and
// the month they leave after it, in its minor units; monthLeft nil with no monthly limit.
type rulesSheet struct {
	currency  string
	asksAbove int64
	monthLeft *int64
}

func (e *ApprovalNeededError) Error() string {
	if e.Expired {
		return fmt.Sprintf("the approval expired, ask again: this request would cost up to %s, and approval %s, a fresh one, must be approved by the workspace's owner before it is retried",
			LXCMoney(e.AmountULXC), e.ApprovalID)
	}
	return fmt.Sprintf("this request would cost up to %s, above the agent's approval amount. Approval %s must be approved by the workspace's owner before it is retried",
		LXCMoney(e.AmountULXC), e.ApprovalID)
}

func (e *ApprovalNeededError) Unwrap() error { return ErrApprovalRequired }

func lxcString(ulxc int64) string { return strconv.FormatFloat(float64(ulxc)/1e6, 'f', -1, 64) }

func ruleRefusal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAgentRule, fmt.Sprintf(format, args...))
}

func clockMinutes(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM)", hhmm)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (r AgentRules) location() (*time.Location, error) {
	if r.Timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(r.Timezone)
}

// limitOf is a limit that may be absent: absent is no limit.
func limitOf(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func (r AgentRules) validate() error {
	for _, v := range []int64{r.MaxPerRequestULXC, limitOf(r.HourlyLimitULXC), r.DailyLimitULXC, limitOf(r.WeeklyLimitULXC),
		r.MonthlyLimitULXC, r.ApprovalAboveULXC, limitOf(r.RequestsPerMinute), limitOf(r.MaxCommitmentULXC), limitOf(r.FXDailyLimitULXC),
		r.MaxPerPaymentMinor, limitOf(r.HourlyLimitMinor), r.DailyLimitMinor, limitOf(r.WeeklyLimitMinor), r.MonthlyLimitMinor, r.ApprovalAboveMinor,
		limitOf(r.FXDailyLimitMinor)} {
		if v < 0 {
			return errors.New("a limit cannot be negative (0 means no limit)")
		}
	}
	named := map[string]string{}
	for model, v := range r.ModelDailyLimitsULXC {
		key := modelCapKey(model)
		if key == "" {
			return errors.New("a model's daily limit needs the model's name")
		}
		if v < 0 {
			return fmt.Errorf("the daily limit for %q cannot be negative (0 means no limit)", model)
		}
		if other, ok := named[key]; ok {
			return fmt.Errorf("%q and %q are the same model; give it one daily limit", other, model)
		}
		named[key] = model
	}
	if limitOf(r.RequestsPerMinute) > math.MaxInt32 {
		return errors.New("requests_per_minute is too large")
	}
	for _, id := range slices.Concat(r.AllowedPayees, r.BlockedPayees) {
		if strings.TrimSpace(id) != id || id == "" {
			return fmt.Errorf("%q is not a payee's id", id)
		}
	}
	for _, id := range r.AllowedPayees {
		if slices.Contains(r.BlockedPayees, id) {
			return fmt.Errorf("%q is both allowed and blocked; give it one", id)
		}
	}
	for _, c := range []struct {
		window string
		caps   map[string]int64
	}{{"daily", r.PayeeDailyLimitsULXC}, {"monthly", r.PayeeMonthlyLimitsULXC}, {"daily", r.PayeeDailyLimitsMinor}, {"monthly", r.PayeeMonthlyLimitsMinor}} {
		window := c.window
		for id, v := range c.caps {
			if strings.TrimSpace(id) != id || id == "" {
				return fmt.Errorf("%q is not a payee's id", id)
			}
			if v < 0 {
				return fmt.Errorf("the %s limit for %q cannot be negative (0 means no limit)", window, id)
			}
		}
	}
	for id, a := range r.PayeeAllowances {
		if strings.TrimSpace(id) != id || id == "" {
			return fmt.Errorf("%q is not a payee's id", id)
		}
		if a.ULXC < 0 || a.Minor < 0 || (a.Per != "day" && a.Per != "month") {
			return fmt.Errorf("always allowing %q needs an amount of 0 or more and per \"day\" or \"month\"", id)
		}
	}
	for _, names := range [][]string{r.AllowedMerchantCategories, r.BlockedMerchantCategories} {
		if err := validMerchantCategories(names); err != nil {
			return err
		}
	}
	for _, name := range r.AllowedMerchantCategories {
		if slices.Contains(r.BlockedMerchantCategories, name) {
			return fmt.Errorf("the merchant category %q is both allowed and blocked; give it one", name)
		}
	}
	for _, licence := range r.AllowedLicences {
		if !slices.Contains(AgentLicences, licence) {
			return fmt.Errorf("an agent may hold commercial and enterprise licences, not %q: a personal licence is one person's", licence)
		}
	}
	for _, c := range r.AllowedCurrencies {
		if MoneyCurrencies[c] == 0 {
			return fmt.Errorf("an agent converts between GBP, EUR, USD and USDC, not %q", c)
		}
	}
	if (r.ActiveFrom == "") != (r.ActiveUntil == "") {
		return errors.New("active_from and active_until go together")
	}
	if r.ActiveFrom != "" {
		from, err := clockMinutes(r.ActiveFrom)
		if err != nil {
			return err
		}
		until, err := clockMinutes(r.ActiveUntil)
		if err != nil {
			return err
		}
		if from == until {
			return errors.New("active_from and active_until are the same time; leave both empty for all day")
		}
	}
	if _, err := r.location(); err != nil {
		return fmt.Errorf("timezone %q is not an IANA time zone", r.Timezone)
	}
	return nil
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

const agentRulesColumns = `COALESCE(max_per_request_ulxc, 0), COALESCE(daily_limit_ulxc, 0), COALESCE(monthly_limit_ulxc, 0),
	COALESCE(approval_above_ulxc, 0), allowed_models, allowed_providers, COALESCE(active_from, ''), COALESCE(active_until, ''), timezone,
	pause_on_unusual_spend, allowed_listings, COALESCE(hourly_limit_ulxc, 0), COALESCE(weekly_limit_ulxc, 0), model_daily_limits_ulxc,
	COALESCE(requests_per_minute, 0), allowed_payees, blocked_payees, payee_daily_limits_ulxc,
	COALESCE(max_commitment_ulxc, 0), COALESCE(allowed_licences, '{}'), COALESCE(may_subscribe, false),
	COALESCE(payee_monthly_limits_ulxc, '{}'), COALESCE(payee_allowances, '{}'), allowed_currencies, COALESCE(fx_daily_limit_ulxc, 0),
	COALESCE(may_instant_transfer, false), COALESCE(currency, 'USD'), COALESCE(max_per_payment_minor, 0), COALESCE(hourly_limit_minor, 0),
	COALESCE(daily_limit_minor, 0), COALESCE(weekly_limit_minor, 0), COALESCE(monthly_limit_minor, 0), COALESCE(approval_above_minor, 0),
	COALESCE(payee_daily_limits_minor, '{}'), COALESCE(payee_monthly_limits_minor, '{}'), COALESCE(fx_daily_limit_minor, 0), allowed_merchant_categories, blocked_merchant_categories`

func scanAgentRules(row pgx.Row) (AgentRules, error) {
	var r AgentRules
	var hourly, weekly, rpm, commitment, fxDaily, hourlyMinor, weeklyMinor, fxDailyMinor int64
	var maySubscribe, mayInstant bool
	err := row.Scan(&r.MaxPerRequestULXC, &r.DailyLimitULXC, &r.MonthlyLimitULXC, &r.ApprovalAboveULXC,
		&r.AllowedModels, &r.AllowedProviders, &r.ActiveFrom, &r.ActiveUntil, &r.Timezone, &r.PauseOnUnusualSpend, &r.AllowedListings,
		&hourly, &weekly, &r.ModelDailyLimitsULXC, &rpm, &r.AllowedPayees, &r.BlockedPayees, &r.PayeeDailyLimitsULXC,
		&commitment, &r.AllowedLicences, &maySubscribe, &r.PayeeMonthlyLimitsULXC, &r.PayeeAllowances, &r.AllowedCurrencies, &fxDaily,
		&mayInstant, &r.Currency, &r.MaxPerPaymentMinor, &hourlyMinor, &r.DailyLimitMinor, &weeklyMinor, &r.MonthlyLimitMinor,
		&r.ApprovalAboveMinor, &r.PayeeDailyLimitsMinor, &r.PayeeMonthlyLimitsMinor, &fxDailyMinor, &r.AllowedMerchantCategories, &r.BlockedMerchantCategories)
	r.HourlyLimitULXC, r.WeeklyLimitULXC, r.RequestsPerMinute = &hourly, &weekly, &rpm
	r.HourlyLimitMinor, r.WeeklyLimitMinor, r.FXDailyLimitMinor = &hourlyMinor, &weeklyMinor, &fxDailyMinor
	r.MaxCommitmentULXC, r.MaySubscribe, r.FXDailyLimitULXC, r.MayInstantTransfer = &commitment, &maySubscribe, &fxDaily, &mayInstant
	return r, err
}

// modelCapSuffix is what a dated snapshot or a -latest alias adds to its model's name.
var modelCapSuffix = regexp.MustCompile(`-([0-9]{8}|[0-9]{4}-[0-9]{2}-[0-9]{2}|latest)$`)

// modelCapKey is the name a per-model daily cap knows a model by (B28.301): lower-case, without a dated
// snapshot's or a -latest alias's suffix, so an agent cannot step around its cap on claude-opus-4-1 by asking
// for Claude-Opus-4-1 or claude-opus-4-1-20250805. Caps are saved, judged and counted under it.
func modelCapKey(model string) string {
	return modelCapSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(model)), "")
}

// modelLimitsJSON is the JSON a model's daily caps are saved as, by modelCapKey, with the zeros (no cap) left
// out; nil when absent.
func modelLimitsJSON(limits map[string]int64) (*string, error) {
	return capsJSON(limits, modelCapKey)
}

// capsJSON is the JSON daily caps are saved as, each under key(its name), with the zeros (no cap) left out; nil
// when absent. A payee's caps (B28.304) are saved under its id as given.
func capsJSON(limits map[string]int64, key func(string) string) (*string, error) {
	if limits == nil {
		return nil, nil
	}
	kept := map[string]int64{}
	for name, v := range limits {
		if v > 0 {
			kept[key(name)] = v
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	out := string(b)
	return &out, nil
}

// allowancesJSON is the JSON payee allowances (B41.104) are saved as, the zeros (no allowance) left out; nil when absent.
func allowancesJSON(allowances map[string]PayeeAllowance) (*string, error) {
	if allowances == nil {
		return nil, nil
	}
	kept := map[string]PayeeAllowance{}
	for id, a := range allowances {
		if a.ULXC > 0 || a.Minor > 0 {
			kept[id] = a
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	out := string(b)
	return &out, nil
}

// SetAgentRules replaces an agent's rules — all but AllowedListings, HourlyLimitULXC, WeeklyLimitULXC,
// ModelDailyLimitsULXC, RequestsPerMinute, AllowedPayees, BlockedPayees, PayeeDailyLimitsULXC, MaxCommitmentULXC,
// AllowedLicences, MaySubscribe, PayeeMonthlyLimitsULXC, PayeeAllowances, AllowedCurrencies, FXDailyLimitULXC,
// MayInstantTransfer and the merchant categories when they are nil (absent from the JSON): a client that predates them (B19.14, B28.300, B28.301,
// B28.302, B28.303, B28.304, B32.22, B41.104, B30.29, B59.15, B61.43) must not clear an agent's listings, caps, payees, licence or conversion rules by saving its
// other rules. An empty list or a zero clears them. A save
// that changes them is their next version (B28.307), recorded with who ctx names (WithRulesChange).
func (s *DualTokenStore) SetAgentRules(ctx context.Context, workspaceID, agentID string, r AgentRules) (AgentRules, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if r, err = s.setAgentRulesTx(ctx, tx, workspaceID, agentID, r); err != nil {
		return r, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	return r, nil
}

// setAgentRulesTx is SetAgentRules in tx.
func (s *DualTokenStore) setAgentRulesTx(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, r AgentRules) (AgentRules, error) {
	if err := r.validate(); err != nil {
		return r, fmt.Errorf("%w: %s", ErrAgentRule, err)
	}
	if err := s.rulesInCurrency(ctx, tx, workspaceID, agentID, &r); err != nil {
		return r, err
	}
	if r.AllowedModels == nil {
		r.AllowedModels = []string{}
	}
	if r.AllowedProviders == nil {
		r.AllowedProviders = []string{}
	}
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	var from, until any
	if r.ActiveFrom != "" {
		from, until = r.ActiveFrom, r.ActiveUntil
	}
	models, err := modelLimitsJSON(r.ModelDailyLimitsULXC)
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	payeeCaps, err := capsJSON(r.PayeeDailyLimitsULXC, func(id string) string { return id })
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	payeeMonthlyCaps, err := capsJSON(r.PayeeMonthlyLimitsULXC, func(id string) string { return id })
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	allowances, err := allowancesJSON(r.PayeeAllowances)
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	payeeCapsMinor, err := capsJSON(r.PayeeDailyLimitsMinor, func(id string) string { return id })
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	payeeMonthlyCapsMinor, err := capsJSON(r.PayeeMonthlyLimitsMinor, func(id string) string { return id })
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	// B32.32: a room wallet's monthly limit is the room's budget, at most what the room owner's plan allows.
	if err := s.checkRoomBudget(ctx, tx, workspaceID, agentID, r.MonthlyLimitULXC); err != nil {
		return r, err
	}
	// $14 to $28, $31, $33 and $36 to $40 are NULL when absent, which keeps the stored caps, payees, licence and conversion rules; a zero or an
	// empty list clears one.
	var hourly, weekly, rpm, commitment, fxDaily, hourlyMinor, weeklyMinor, fxDailyMinor int64
	var maySubscribe, mayInstant bool
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_rules (agent_id, workspace_id, max_per_request_ulxc, daily_limit_ulxc, monthly_limit_ulxc,
		       approval_above_ulxc, allowed_models, allowed_providers, active_from, active_until, timezone, pause_on_unusual_spend,
		       allowed_listings, hourly_limit_ulxc, weekly_limit_ulxc, model_daily_limits_ulxc, requests_per_minute,
		       allowed_payees, blocked_payees, payee_daily_limits_ulxc, max_commitment_ulxc, allowed_licences, may_subscribe,
		       payee_monthly_limits_ulxc, payee_allowances, allowed_currencies, fx_daily_limit_ulxc, may_instant_transfer,
		       currency, max_per_payment_minor, hourly_limit_minor, daily_limit_minor, weekly_limit_minor, monthly_limit_minor,
		       approval_above_minor, payee_daily_limits_minor, payee_monthly_limits_minor, fx_daily_limit_minor,
		       allowed_merchant_categories, blocked_merchant_categories)
		SELECT id, workspace_id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, COALESCE($13::text[], '{}'),
		       NULLIF($14::bigint, 0), NULLIF($15::bigint, 0), COALESCE($16::text::jsonb, '{}'), NULLIF($17::integer, 0),
		       COALESCE($18::text[], '{}'), COALESCE($19::text[], '{}'), COALESCE($20::text::jsonb, '{}'),
		       NULLIF($21::bigint, 0), COALESCE($22::text[], '{}'), COALESCE($23::boolean, false),
		       COALESCE($24::text::jsonb, '{}'), COALESCE($25::text::jsonb, '{}'),
		       COALESCE($26::text[], '{}'), NULLIF($27::bigint, 0), COALESCE($28::boolean, false),
		       $29, $30, NULLIF($31::bigint, 0), $32, NULLIF($33::bigint, 0), $34, $35,
		       COALESCE($36::text::jsonb, '{}'), COALESCE($37::text::jsonb, '{}'), NULLIF($38::bigint, 0),
		       COALESCE($39::text[], '{}'), COALESCE($40::text[], '{}')
		  FROM agent_accounts WHERE id = $1 AND workspace_id = $2
		ON CONFLICT (agent_id) DO UPDATE SET max_per_request_ulxc = EXCLUDED.max_per_request_ulxc,
		       daily_limit_ulxc = EXCLUDED.daily_limit_ulxc, monthly_limit_ulxc = EXCLUDED.monthly_limit_ulxc,
		       approval_above_ulxc = EXCLUDED.approval_above_ulxc, allowed_models = EXCLUDED.allowed_models,
		       allowed_providers = EXCLUDED.allowed_providers, active_from = EXCLUDED.active_from,
		       active_until = EXCLUDED.active_until, timezone = EXCLUDED.timezone,
		       pause_on_unusual_spend = EXCLUDED.pause_on_unusual_spend, allowed_listings = COALESCE($13::text[], agent_rules.allowed_listings),
		       hourly_limit_ulxc = NULLIF(COALESCE($14::bigint, agent_rules.hourly_limit_ulxc), 0),
		       weekly_limit_ulxc = NULLIF(COALESCE($15::bigint, agent_rules.weekly_limit_ulxc), 0),
		       model_daily_limits_ulxc = COALESCE($16::text::jsonb, agent_rules.model_daily_limits_ulxc),
		       requests_per_minute = NULLIF(COALESCE($17::integer, agent_rules.requests_per_minute), 0),
		       allowed_payees = COALESCE($18::text[], agent_rules.allowed_payees),
		       blocked_payees = COALESCE($19::text[], agent_rules.blocked_payees),
		       payee_daily_limits_ulxc = COALESCE($20::text::jsonb, agent_rules.payee_daily_limits_ulxc),
		       max_commitment_ulxc = NULLIF(COALESCE($21::bigint, agent_rules.max_commitment_ulxc), 0),
		       allowed_licences = COALESCE($22::text[], agent_rules.allowed_licences),
		       may_subscribe = COALESCE($23::boolean, agent_rules.may_subscribe),
		       payee_monthly_limits_ulxc = COALESCE($24::text::jsonb, agent_rules.payee_monthly_limits_ulxc),
		       payee_allowances = COALESCE($25::text::jsonb, agent_rules.payee_allowances),
		       allowed_currencies = COALESCE($26::text[], agent_rules.allowed_currencies),
		       fx_daily_limit_ulxc = NULLIF(COALESCE($27::bigint, agent_rules.fx_daily_limit_ulxc), 0),
		       may_instant_transfer = COALESCE($28::boolean, agent_rules.may_instant_transfer),
		       currency = EXCLUDED.currency, max_per_payment_minor = EXCLUDED.max_per_payment_minor,
		       hourly_limit_minor = NULLIF(COALESCE($31::bigint, agent_rules.hourly_limit_minor), 0),
		       daily_limit_minor = EXCLUDED.daily_limit_minor,
		       weekly_limit_minor = NULLIF(COALESCE($33::bigint, agent_rules.weekly_limit_minor), 0),
		       monthly_limit_minor = EXCLUDED.monthly_limit_minor, approval_above_minor = EXCLUDED.approval_above_minor,
		       payee_daily_limits_minor = COALESCE($36::text::jsonb, agent_rules.payee_daily_limits_minor),
		       payee_monthly_limits_minor = COALESCE($37::text::jsonb, agent_rules.payee_monthly_limits_minor),
		       fx_daily_limit_minor = NULLIF(COALESCE($38::bigint, agent_rules.fx_daily_limit_minor), 0),
		       allowed_merchant_categories = COALESCE($39::text[], agent_rules.allowed_merchant_categories),
		       blocked_merchant_categories = COALESCE($40::text[], agent_rules.blocked_merchant_categories), updated_at = now()
		RETURNING COALESCE(hourly_limit_ulxc, 0), COALESCE(weekly_limit_ulxc, 0), model_daily_limits_ulxc, COALESCE(requests_per_minute, 0),
		       allowed_payees, blocked_payees, payee_daily_limits_ulxc, COALESCE(max_commitment_ulxc, 0), allowed_licences, may_subscribe,
		       payee_monthly_limits_ulxc, payee_allowances, allowed_currencies, COALESCE(fx_daily_limit_ulxc, 0), may_instant_transfer,
		       COALESCE(hourly_limit_minor, 0), COALESCE(weekly_limit_minor, 0), payee_daily_limits_minor, payee_monthly_limits_minor,
		       COALESCE(fx_daily_limit_minor, 0), allowed_merchant_categories, blocked_merchant_categories`,
		agentID, workspaceID, nullIfZero(r.MaxPerRequestULXC), nullIfZero(r.DailyLimitULXC), nullIfZero(r.MonthlyLimitULXC),
		nullIfZero(r.ApprovalAboveULXC), r.AllowedModels, r.AllowedProviders, from, until, r.Timezone, r.PauseOnUnusualSpend,
		r.AllowedListings, r.HourlyLimitULXC, r.WeeklyLimitULXC, models, r.RequestsPerMinute, r.AllowedPayees, r.BlockedPayees, payeeCaps,
		r.MaxCommitmentULXC, r.AllowedLicences, r.MaySubscribe, payeeMonthlyCaps, allowances, r.AllowedCurrencies, r.FXDailyLimitULXC, r.MayInstantTransfer,
		r.Currency, nullIfZero(r.MaxPerPaymentMinor), r.HourlyLimitMinor, nullIfZero(r.DailyLimitMinor), r.WeeklyLimitMinor,
		nullIfZero(r.MonthlyLimitMinor), nullIfZero(r.ApprovalAboveMinor), payeeCapsMinor, payeeMonthlyCapsMinor, r.FXDailyLimitMinor,
		r.AllowedMerchantCategories, r.BlockedMerchantCategories).
		Scan(&hourly, &weekly, &r.ModelDailyLimitsULXC, &rpm, &r.AllowedPayees, &r.BlockedPayees, &r.PayeeDailyLimitsULXC,
			&commitment, &r.AllowedLicences, &maySubscribe, &r.PayeeMonthlyLimitsULXC, &r.PayeeAllowances, &r.AllowedCurrencies, &fxDaily, &mayInstant,
			&hourlyMinor, &weeklyMinor, &r.PayeeDailyLimitsMinor, &r.PayeeMonthlyLimitsMinor, &fxDailyMinor,
			&r.AllowedMerchantCategories, &r.BlockedMerchantCategories)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrAgentNotFound
	}
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	if err := recordRulesVersion(ctx, tx, agentID, rulesChangeFrom(ctx)); err != nil {
		return r, err
	}
	r.HourlyLimitULXC, r.WeeklyLimitULXC, r.RequestsPerMinute = &hourly, &weekly, &rpm
	r.MaxCommitmentULXC, r.MaySubscribe, r.FXDailyLimitULXC, r.MayInstantTransfer = &commitment, &maySubscribe, &fxDaily, &mayInstant
	r.HourlyLimitMinor, r.WeeklyLimitMinor, r.FXDailyLimitMinor = &hourlyMinor, &weeklyMinor, &fxDailyMinor
	return r, nil
}

// GetAgentRules reads an agent's rules; an agent with none set has none.
func (s *DualTokenStore) GetAgentRules(ctx context.Context, workspaceID, agentID string) (AgentRules, error) {
	return getAgentRules(ctx, s.pool, workspaceID, agentID)
}

func getAgentRules(ctx context.Context, q pgxDB, workspaceID, agentID string) (AgentRules, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return AgentRules{}, fmt.Errorf("economy: agent rules: %w", err)
	}
	if !exists {
		return AgentRules{}, ErrAgentNotFound
	}
	r, err := scanAgentRules(q.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		var hourly, weekly, rpm, commitment, fxDaily int64
		var maySubscribe, mayInstant bool
		return AgentRules{HourlyLimitULXC: &hourly, WeeklyLimitULXC: &weekly, RequestsPerMinute: &rpm, ModelDailyLimitsULXC: map[string]int64{},
			AllowedModels: []string{}, AllowedProviders: []string{}, AllowedListings: []string{}, AllowedPayees: []string{},
			BlockedPayees: []string{}, PayeeDailyLimitsULXC: map[string]int64{}, Timezone: "UTC",
			MaxCommitmentULXC: &commitment, AllowedLicences: []string{}, MaySubscribe: &maySubscribe,
			PayeeMonthlyLimitsULXC: map[string]int64{}, PayeeAllowances: map[string]PayeeAllowance{},
			AllowedCurrencies: []string{}, FXDailyLimitULXC: &fxDaily, MayInstantTransfer: &mayInstant,
			Currency: CurrencyUSD, HourlyLimitMinor: new(int64), WeeklyLimitMinor: new(int64),
			PayeeDailyLimitsMinor: map[string]int64{}, PayeeMonthlyLimitsMinor: map[string]int64{}, FXDailyLimitMinor: new(int64),
			AllowedMerchantCategories: []string{}, BlockedMerchantCategories: []string{}}, nil
	}
	if err != nil {
		return r, fmt.Errorf("economy: agent rules: %w", err)
	}
	return r, nil
}

// agentPeriodLimit is one of an agent's spending limits over a period: what it may spend since `since`.
type agentPeriodLimit struct {
	amount int64 // in the rules' currency (B61.12); µLXC from periodLimitsULXC
	since  time.Time
	name   string
}

// periodLimits are r's hourly, daily, weekly and monthly limits, their periods begun in loc as of now (in
// loc): the clock hour, the day, the week from Monday and the month. The hour is counted back from now, not
// rebuilt with time.Date: when the clocks go back an hour repeats, and time.Date may answer its other
// occurrence — one in the future, which would count nothing spent.
func (r AgentRules) periodLimits(now time.Time, loc *time.Location) []agentPeriodLimit {
	monday := now.Day() - (int(now.Weekday())+6)%7
	hour := now.Add(-(time.Duration(now.Minute())*time.Minute + time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond())))
	return []agentPeriodLimit{
		{limitOf(r.HourlyLimitMinor), hour, "hourly"},
		{r.DailyLimitMinor, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), "daily"},
		{limitOf(r.WeeklyLimitMinor), time.Date(now.Year(), now.Month(), monday, 0, 0, 0, 0, loc), "weekly"},
		{r.MonthlyLimitMinor, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), "monthly"},
	}
}

// periodLimitsULXC are periodLimits in µLXC, as the parked gateway's allowance counts them.
func (r AgentRules) periodLimitsULXC(now time.Time, loc *time.Location) []agentPeriodLimit {
	l := r
	l.HourlyLimitMinor, l.DailyLimitMinor, l.WeeklyLimitMinor, l.MonthlyLimitMinor = r.HourlyLimitULXC, r.DailyLimitULXC, r.WeeklyLimitULXC, r.MonthlyLimitULXC
	return l.periodLimits(now, loc)
}

// agentSpentSince is what an agent has spent since `since`, as its period limits count it, in µLXC.
func agentSpentSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, since time.Time) (int64, error) {
	spent, err := agentSpentIn(ctx, tx, workspaceID, agentID, since, "")
	return spent.ulxc, err
}

// agentSpend is what an agent has spent over a period: all of it in µLXC, and of it what the rules recorded in their
// currency as they let it through (B61.41) and the µLXC of the rest.
type agentSpend struct{ ulxc, recordedMinor, restULXC int64 }

// agentSpentIn is what an agent has spent since `since`, as its period limits count it, with the movements the
// rules recorded in currency (agent_rule_spends) at what they were worth in it then; "" records none.
func agentSpentIn(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, since time.Time, currency string) (agentSpend, error) {
	var s agentSpend
	// What it paid other agents counts (B19.3), and its card purchases (B19.12); what they paid it does not raise its limit. What it
	// used of paid marketplace listings counts too (B20.2), though it is billed to its company. Its questions' platform fees count (B32.11).
	// So do its payments out of Talyvor (B30.17) at what they were worth in LXC, unless they failed or came back, and the
	// invoices it paid from its currency account (B30.131) and what payees pulled under its mandates (B55.24), recorded
	// under their entry key as the rules let them through, the charges on its reusable cards (B54.6), and the purchases
	// its agent card held on its currency account (B30.30). So do (B50.53) its Instant transfers unless they were
	// reversed, its payments to people and its currency payments to other agents, and its card spends unless the issuer
	// refused the card, as the rules recorded them under their ref. Each of the last four sources is named by the ref the
	// rules judged it by, so what they recorded of it in currency replaces its µLXC.
	err := tx.QueryRow(ctx, `WITH moved (ulxc, ref) AS (
		SELECT (SELECT COALESCE(-sum(amount_ulxc), 0) FROM agent_postings
		 WHERE workspace_id = $1 AND account = $2 AND created_at >= $3
		   AND (kind IN ('spend', 'hold', 'settle', 'release', 'card', 'platform_fee') OR (kind = 'pay' AND amount_ulxc < 0)))
		 + (SELECT COALESCE(sum(price_ulxc), 0) FROM market_uses
		   WHERE buyer_workspace_id = $1 AND agent_id = $4 AND charge = 'billed' AND used_at >= $3), NULL::text
		UNION ALL SELECT value_ulxc, id FROM money_payments
		   WHERE workspace_id = $1 AND agent_id = $4 AND created_at >= $3 AND status NOT IN ('failed', 'returned')
		UNION ALL SELECT p.amount_ulxc, p.ref FROM agent_payee_payments p
		   WHERE p.workspace_id = $1 AND p.agent_id = $4 AND p.created_at >= $3
		     AND (p.ref LIKE 'invoice_agent:%' OR p.ref LIKE 'mandate_pull:%' OR p.ref LIKE 'person_pay:%' OR p.ref LIKE 'agent_money_pay:%'
		       OR (p.ref LIKE 'itr\_%' AND NOT EXISTS (SELECT 1 FROM instant_transfers t WHERE t.id = p.ref AND t.status = 'reversed'))
		       OR (p.ref LIKE 'card_spend:%' AND NOT EXISTS (SELECT 1 FROM money_card_spends c
		         WHERE c.id = substr(p.ref, length('card_spend:') + 1) AND c.status = 'failed')))
		UNION ALL SELECT value_ulxc, 'card_charge:' || partner || ':' || id FROM money_card_authorizations
		   WHERE workspace_id = $1 AND agent_id = $4 AND approved AND created_at >= $3
		UNION ALL SELECT amount_ulxc, authorization_id FROM agent_card_authorizations
		   WHERE workspace_id = $1 AND agent_id = $4 AND approved AND money_account_id IS NOT NULL AND created_at >= $3)
		SELECT COALESCE(sum(m.ulxc), 0)::bigint, COALESCE(sum(r.amount_minor), 0)::bigint,
			COALESCE(sum(m.ulxc) FILTER (WHERE r.ref IS NULL), 0)::bigint
		FROM moved m LEFT JOIN agent_rule_spends r ON r.workspace_id = $1 AND r.agent_id = $4 AND r.ref = m.ref AND r.currency = $5`,
		workspaceID, agentAccount(agentID), since, agentID, currency).Scan(&s.ulxc, &s.recordedMinor, &s.restULXC)
	return s, err
}

// recordRuleSpend records what a movement the rules let through was worth in their currency (B61.41), in its
// transaction, under the ref they judged it by; one already recorded under that ref keeps what it recorded.
func recordRuleSpend(ctx context.Context, tx pgx.Tx, workspaceID, agentID, ref string, j judgedLimits) error {
	if j.recordedIn == "" || j.worth <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_rule_spends (workspace_id, agent_id, ref, currency, amount_minor) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (agent_id, ref) DO NOTHING`, workspaceID, agentID, ref, j.recordedIn, j.worth); err != nil {
		return fmt.Errorf("economy: record the rules' spend: %w", err)
	}
	return nil
}

// modelDailyLimit is r's daily cap on model (B28.301), 0 for none, and when its day began in loc as of now.
func (r AgentRules) modelDailyLimit(model string, now time.Time, loc *time.Location) (int64, time.Time) {
	return r.ModelDailyLimitsULXC[modelCapKey(model)], time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

// agentModelSpentSince is what an agent has spent on model since `since`: its questions to it, held or debited,
// net of what their settles and releases gave back, with their platform fees (B32.11) — the postings that name
// the model (B28.301).
func agentModelSpentSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID, model string, since time.Time) (int64, error) {
	var spent int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(-sum(amount_ulxc), 0)::bigint FROM agent_postings
		WHERE workspace_id = $1 AND account = $2 AND model = $3 AND created_at >= $4 AND kind IN ('spend', 'hold', 'settle', 'release', 'platform_fee')`,
		workspaceID, agentAccount(agentID), modelCapKey(model), since).Scan(&spent)
	return spent, err
}

// agentRequestsSince is how many questions an agent has held or debited since `since` (B28.302): one per
// reservation or claim, counted by its first posting — a debit's later under-charge settle is a 'spend' of the
// same ref, not another question. A settle is looked for no further back than an hour.
func agentRequestsSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, since time.Time) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT ref FROM agent_postings
		WHERE workspace_id = $1 AND account = $2 AND kind IN ('hold', 'spend') AND created_at > $3::timestamptz - interval '1 hour'
		GROUP BY ref HAVING min(created_at) > $3) AS questions`,
		workspaceID, agentAccount(agentID), since).Scan(&n)
	return n, err
}

// payeeIDs are the ids a payee lists may name p by (B28.303): its own, and for an agent or a listing the company
// that has it, so that naming a company names its agents and listings too.
func payeeIDs(ctx context.Context, tx pgx.Tx, p Payee) ([]string, error) {
	query := map[string]string{
		"agent":   `SELECT workspace_id FROM agent_accounts WHERE id = $1`,
		"listing": `SELECT workspace_id FROM market_listings WHERE id = $1`,
	}[p.Kind]
	if query == "" || p.ID == "" {
		return []string{p.ID}, nil
	}
	var company string
	if err := tx.QueryRow(ctx, query, p.ID).Scan(&company); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("economy: payee's company: %w", err)
	}
	return []string{p.ID, company}, nil
}

// agentPaidPayeeSince is what an agent has paid the payee a daily cap names by id since `since` (B28.304): its
// payments the rules let through, to that payee or — for a company — to its agents and listings.
func agentPaidPayeeSince(ctx context.Context, tx pgx.Tx, agentID, id string, since time.Time) (int64, error) {
	var paid int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_payee_payments
		WHERE agent_id = $1 AND $2 = ANY(payee_ids) AND created_at >= $3`, agentID, id, since).Scan(&paid)
	return paid, err
}

// agentPaidPayee is what an agent has paid the payee id names since `since`: in µLXC, as agentPaidPayeeSince counts
// it, or — inCurrency — in the minor units of currency (B61.40): payments in it on their own amounts, those in another
// currency at the conversion provider's rate and those recorded only in µLXC at what they are worth, each rounded up,
// so rounding never lets a payment through.
func (s *DualTokenStore) agentPaidPayee(ctx context.Context, tx pgx.Tx, workspaceID, agentID, id string, since time.Time,
	currency string, inCurrency bool) (int64, error) {
	if !inCurrency {
		return agentPaidPayeeSince(ctx, tx, agentID, id, since)
	}
	rows, err := tx.Query(ctx, `SELECT COALESCE(currency, ''), COALESCE(sum(COALESCE(amount_minor, amount_ulxc)), 0)::bigint
		FROM agent_payee_payments WHERE agent_id = $1 AND $2 = ANY(payee_ids) AND created_at >= $3 GROUP BY 1`, agentID, id, since)
	if err != nil {
		return 0, err
	}
	sums, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Currency string
		Sum      int64
	}])
	if err != nil {
		return 0, err
	}
	var paid int64
	for _, p := range sums {
		var v int64
		if p.Currency == "" { // recorded before B61.40, or a payment in LXC
			v, err = s.ulxcIn(ctx, workspaceID, p.Sum, currency, true)
		} else {
			v, err = s.convertMinor(ctx, workspaceID, p.Sum, p.Currency, currency, true)
		}
		if err != nil {
			return 0, err
		}
		paid += v
	}
	return paid, nil
}

// recordPayeePayment records a payment the rules let through, in its movement's transaction, under the ids a
// payee's daily cap may name it by (B28.304) — every payment, capped or not, so a cap set at noon counts the
// morning — and, for money from a currency account, its own amount (B61.40). A payment of nothing, or to no one, is
// not one.
func recordPayeePayment(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, req AgentRequest, ids []string, amount int64, ref string) error {
	if !req.Payment || req.Payee.ID == "" || amount <= 0 {
		return nil
	}
	if ids == nil {
		var err error
		if ids, err = payeeIDs(ctx, tx, req.Payee); err != nil {
			return err
		}
	}
	var currency, minor any
	if own, ok := ownMoney(req); ok {
		currency, minor = own.Currency, own.Minor
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_payee_payments (workspace_id, agent_id, payee_kind, payee_ids, amount_ulxc, ref, currency, amount_minor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, workspaceID, agentID, req.Payee.Kind, slices.DeleteFunc(ids, func(id string) bool { return id == "" }),
		amount, ref, currency, minor); err != nil {
		return fmt.Errorf("economy: record payee payment: %w", err)
	}
	return nil
}

// CheckAgentRules judges an agent key's request that moves no LXC — one answered on its workspace's own
// provider key under BYOK (B27.26) — against the agent's rules: paused, active hours, models, providers. The
// amount rules pass at zero. A key attached to no agent has none. The request rides ctx (WithAgentRequest).
func (s *DualTokenStore) CheckAgentRules(ctx context.Context, scopedKeyID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var agentID, workspaceID string
	err = tx.QueryRow(ctx, `SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: agent of key: %w", err)
	}
	if err := s.enforceAgentRules(ctx, tx, workspaceID, agentID, 0, "byok"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// enforceAgentRules judges a positive movement of amount µLXC (a hold or a debit, ref its reservation or
// request id) against the agent's rules, inside the movement's transaction with the agent's row locked. Its limits
// judge it in the rules' currency (judgedAmount, B61.12). A paused agent is refused first; a movement the rules let
// through is then watched for unusual spend (B19.6).
func (s *DualTokenStore) enforceAgentRules(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, amount int64, ref string) error {
	if err := refuseIfPaused(ctx, tx, agentID); err != nil {
		return err
	}
	req := agentRequestFrom(ctx)
	prepared := req.Payment && prepareOnly(ctx)           // B52.41: it waits for the owner's approval, whatever the rules allow
	r, err := agentRulesInForce(ctx, tx, agentID, req.At) // with its boosts in force then (B28.308)
	if errors.Is(err, pgx.ErrNoRows) && prepared {
		r, err = AgentRules{}, nil // judged by no rules, as below, then held for the approval
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if err := (AgentRules{}).judgeCommitment(req.Commitment); err != nil {
			return err
		}
		if err := recordPayeePayment(ctx, tx, workspaceID, agentID, req, nil, amount, ref); err != nil {
			return err
		}
		if err := s.proveRuledPayment(ctx, tx, workspaceID, agentID, ref, ProofRuleNoRules, req, amount, nil); err != nil {
			return err
		}
		return watchAgentSpend(ctx, tx, workspaceID, agentID, amount, req.At, false)
	}
	if err != nil {
		return fmt.Errorf("economy: agent rules: %w", err)
	}
	what := "request"
	if req.Payment {
		what = "payment"
	}
	loc, err := r.location()
	if err != nil {
		return ruleRefusal("the agent's timezone %q cannot be read", r.Timezone)
	}
	now := req.At.In(loc)
	if r.ActiveFrom != "" {
		from, _ := clockMinutes(r.ActiveFrom)
		until, _ := clockMinutes(r.ActiveUntil)
		m := now.Hour()*60 + now.Minute()
		inside := from <= m && m < until
		if from > until { // the window crosses midnight
			inside = m >= from || m < until
		}
		if !inside {
			return ruleRefusal("the agent may spend only between %s and %s (%s)", r.ActiveFrom, r.ActiveUntil, r.Timezone)
		}
	}
	if len(r.AllowedModels) > 0 && !req.Payment && !slices.Contains(r.AllowedModels, req.Model) {
		return ruleRefusal("the agent may not use the model %q", req.Model)
	}
	if len(r.AllowedProviders) > 0 && !req.Payment && !slices.Contains(r.AllowedProviders, req.Provider) {
		return ruleRefusal("the agent may not use the provider %q", req.Provider)
	}
	if len(r.AllowedListings) > 0 && req.Listing != "" && !slices.Contains(r.AllowedListings, req.Listing) {
		return ruleRefusal("the agent may not use the marketplace listing %q", req.Listing)
	}
	if err := r.judgeCommitment(req.Commitment); err != nil {
		return err
	}
	var ids []string                              // the ids the payee lists and caps name a payment's payee by
	payee := cmp.Or(req.Payee.Name, req.Payee.ID) // B55.30: the name a refusal gives the payee
	if req.Payment && len(r.AllowedPayees)+len(r.BlockedPayees)+len(r.PayeeDailyLimitsULXC)+len(r.PayeeMonthlyLimitsULXC)+len(r.PayeeAllowances) > 0 {
		if ids, err = payeeIDs(ctx, tx, req.Payee); err != nil {
			return err
		}
		// Only the workspace's own agents and marketplace listings are named; another company's agent keeps its id.
		if req.Payee.Name == "" && (req.Payee.Kind == "listing" || len(ids) > 1 && ids[1] == workspaceID) {
			named, err := namePayee(ctx, tx, req.Payee)
			if err != nil {
				return err
			}
			payee = cmp.Or(named.Name, named.ID)
		}
	}
	if req.Payment && len(r.AllowedPayees)+len(r.BlockedPayees) > 0 {
		if slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(r.BlockedPayees, id) }) {
			return ruleRefusal("the agent may not pay %s", payee)
		}
		if len(r.AllowedPayees) > 0 && !slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(r.AllowedPayees, id) }) {
			return ruleRefusal("the agent may pay only the payees its rules name, and %s is not one", payee)
		}
	}
	if req.Payment && req.ByCard {
		if err := r.judgeMerchantCategory(payee, req.MerchantCategory); err != nil {
			return err
		}
	}
	if rpm := limitOf(r.RequestsPerMinute); rpm > 0 && !req.Payment {
		asked, err := agentRequestsSince(ctx, tx, workspaceID, agentID, req.At.Add(-time.Minute))
		if err != nil {
			return fmt.Errorf("economy: agent requests so far: %w", err)
		}
		if asked >= rpm {
			return fmt.Errorf("%w: the agent may make %d requests a minute and has made %d in the last minute; try again shortly",
				ErrAgentRequestRate, rpm, asked)
		}
	}
	// B61.12: money from a currency account is judged in the rules' currency, LXC in µLXC (judgedAmount).
	j, err := s.judgedAmount(ctx, workspaceID, r, req, amount, now, loc)
	if err != nil {
		return err
	}
	if j.perPayment > 0 && j.amount > j.perPayment {
		limit := "request"
		if _, inCurrency := ownMoney(req); inCurrency {
			limit = "payment"
		}
		return ruleRefusal("this %s would cost up to %s; the agent's limit per %s is %s",
			what, j.words(j.amount), limit, j.words(j.perPayment))
	}
	var monthLeft, monthLeftMinor *int64
	for _, limit := range j.periods {
		if limit.amount == 0 {
			continue
		}
		so, err := agentSpentIn(ctx, tx, workspaceID, agentID, limit.since, j.recordedIn)
		if err != nil {
			return fmt.Errorf("economy: agent spend so far: %w", err)
		}
		spentULXC := so.ulxc
		spent, err := j.spentIn(so)
		if err != nil {
			return err
		}
		if spent+j.amount > limit.amount {
			return ruleRefusal("the agent has spent %s of its %s limit of %s, and this %s would cost up to %s",
				j.words(spent), limit.name, j.words(limit.amount), what, j.words(j.amount))
		}
		if limit.name == "monthly" && r.MonthlyLimitULXC > 0 { // the approval sheet's "left this month", in LXC (B57.11)
			left := r.MonthlyLimitULXC - spentULXC - amount
			monthLeft = &left
		}
		if limit.name == "monthly" && r.MonthlyLimitMinor > 0 { // and in the rules' currency (B61.42)
			left := limit.amount - spent - j.amount
			if !j.inCurrency { // an LXC movement, judged in µLXC: valued in the rules' currency
				sheet, err := s.sheetFor(ctx, tx, workspaceID, agentID, r, limit.since, amount)
				if err != nil {
					return err
				}
				left = *sheet.monthLeft
			}
			monthLeftMinor = &left
		}
	}
	if limit, since := r.modelDailyLimit(req.Model, now, loc); limit > 0 && !req.Payment {
		spent, err := agentModelSpentSince(ctx, tx, workspaceID, agentID, req.Model, since)
		if err != nil {
			return fmt.Errorf("economy: agent spend on the model so far: %w", err)
		}
		if spent+amount > limit {
			return ruleRefusal("the agent has spent %s of its daily limit of %s for the model %q, and this request would cost up to %s",
				LXCMoney(spent), LXCMoney(limit), req.Model, LXCMoney(amount))
		}
	}
	day, month := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	for _, id := range ids {
		for _, c := range []struct {
			minor, ulxc int64
			since       time.Time
			name, when  string
		}{{r.PayeeDailyLimitsMinor[id], r.PayeeDailyLimitsULXC[id], day, "daily", "today"},
			{r.PayeeMonthlyLimitsMinor[id], r.PayeeMonthlyLimitsULXC[id], month, "monthly", "this month"}} {
			limit, cost, words, inCurrency := j.payeeLimit(c.minor, c.ulxc, amount)
			if limit <= 0 {
				continue
			}
			paid, err := s.agentPaidPayee(ctx, tx, workspaceID, agentID, id, c.since, r.currency(), inCurrency)
			if err != nil {
				return fmt.Errorf("economy: agent payments to the payee so far: %w", err)
			}
			if paid+cost > limit {
				return ruleRefusal("the agent has paid %s %s of its %s limit of %s for %s, and this payment would cost %s",
					words(paid), c.when, c.name, words(limit), payee, words(cost))
			}
		}
	}
	rule := ProofRuleWithinRules // B62.1: the rule that passes it, named on its record
	if req.Approved {
		rule = ProofRuleMandate
	}
	var used *usedApproval
	if prepared || (j.approvalAbove > 0 && j.amount > j.approvalAbove && !req.Approved) {
		need := &ApprovalNeededError{AmountULXC: amount, workspaceID: workspaceID, agentID: agentID, req: req,
			asksAbove: r.ApprovalAboveULXC, monthLeft: monthLeft, rule: ProofRuleApprovalAbove,
			inRules: rulesSheet{currency: r.currency(), asksAbove: r.ApprovalAboveMinor, monthLeft: monthLeftMinor}}
		if prepared {
			need.rule = ProofRulePrepared
		}
		// An approved request goes through once: the approval is used up in this transaction, so a
		// refusal later in it (the workspace's balance, say) leaves it approved. One the owner always allows
		// (B41.104) needs none.
		if req.Fingerprint != "" {
			u, ok, err := useApproval(ctx, tx, workspaceID, agentID, req.Fingerprint, ref)
			if err != nil {
				return err
			}
			if ok {
				// B62.1: the payment about to be sent must be the one the owner approved; anything else stops here, and
				// this transaction rolling back releases whatever it held.
				sent, err := sentOf(ctx, tx, workspaceID, agentID, ref, req, amount)
				if err != nil {
					return err
				}
				if err := approvalMatches(workspaceID, u.shown, sent); err != nil {
					return err
				}
				used = &u
			}
		}
		if used == nil {
			allowed := false // a prepared payment is never "always allowed"
			if !prepared {
				if allowed, err = s.withinPayeeAllowance(ctx, tx, workspaceID, agentID, r, j, ids, amount, day, month); err != nil {
					return err
				}
			}
			if allowed {
				rule = ProofRuleAlwaysAllow
			}
			switch {
			case allowed && req.Fingerprint != "":
				// "Always allow" is for requests made after it (B50.18): one already waiting in the inbox waits for its own tap.
				var waiting bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2
					AND status = 'pending' AND expires_at > now())`, agentID, req.Fingerprint).Scan(&waiting); err != nil {
					return fmt.Errorf("economy: waiting approval: %w", err)
				}
				if waiting {
					return need
				}
			case allowed:
			case req.Fingerprint == "":
				return ruleRefusal("this %s needs approval and carries nothing to approve it by", what)
			default:
				return need
			}
		}
	}
	if err := recordRuleSpend(ctx, tx, workspaceID, agentID, ref, j); err != nil {
		return err
	}
	if err := recordPayeePayment(ctx, tx, workspaceID, agentID, req, ids, amount, ref); err != nil {
		return err
	}
	if err := s.proveRuledPayment(ctx, tx, workspaceID, agentID, ref, rule, req, amount, used); err != nil {
		return err
	}
	return watchAgentSpend(ctx, tx, workspaceID, agentID, amount, req.At, r.PauseOnUnusualSpend)
}

// proveRuledPayment writes the record of a payment the rules let through by rule (B62.1); a request to a model is no
// payment and has none.
func (s *DualTokenStore) proveRuledPayment(ctx context.Context, tx pgx.Tx, workspaceID, agentID, ref, rule string, req AgentRequest,
	amount int64, used *usedApproval) error {
	if !req.Payment {
		return nil
	}
	sent, err := sentOf(ctx, tx, workspaceID, agentID, ref, req, amount)
	if err != nil {
		return err
	}
	var version *int64
	if used == nil {
		if version, err = latestRulesVersion(ctx, tx, agentID); err != nil {
			return err
		}
		if rule == ProofRuleNoRules {
			version = nil
		}
	}
	return recordPaymentProof(ctx, tx, workspaceID, agentID, ref, rule, version, sent, used)
}

// withinPayeeAllowance is whether a payment of amount µLXC, judged by r as j (B61.40), to the payee ids name fits one
// of r's "always allow"s (B41.104): what the agent has paid that payee in the allowance's day or month, with this
// payment, is within it.
func (s *DualTokenStore) withinPayeeAllowance(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, r AgentRules, j judgedLimits,
	ids []string, amount int64, day, month time.Time) (bool, error) {
	for _, id := range ids {
		a := r.PayeeAllowances[id]
		limit, cost, _, inCurrency := j.payeeLimit(a.Minor, a.ULXC, amount)
		if limit <= 0 {
			continue
		}
		since := day
		if a.Per == "month" {
			since = month
		}
		paid, err := s.agentPaidPayee(ctx, tx, workspaceID, agentID, id, since, r.currency(), inCurrency)
		if err != nil {
			return false, fmt.Errorf("economy: agent payments to the payee so far: %w", err)
		}
		if paid+cost <= limit {
			return true, nil
		}
	}
	return false, nil
}

// AllowPayee saves the owner's "always allow" (B41.104) of the payee an approved approval names: the agent may pay it
// up to a.Minor of the rules' currency (B61.40) a day or a month without approval. It replaces that payee's allowance and keeps the agent's other
// rules; it is their next version, recorded with who ctx names (WithRulesChange).
func (s *DualTokenStore) AllowPayee(ctx context.Context, workspaceID, approvalID string, a PayeeAllowance) (AgentRules, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentRules{}, fmt.Errorf("economy: always allow: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var agentID, payeeID string
	err = tx.QueryRow(ctx, `SELECT agent_id, payee_id FROM agent_approvals WHERE id = $1 AND workspace_id = $2
		AND status IN ('approved', 'used')`,
		approvalID, workspaceID).Scan(&agentID, &payeeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRules{}, ErrApprovalNotFound
	}
	if err != nil {
		return AgentRules{}, fmt.Errorf("economy: always allow: %w", err)
	}
	if err := s.allowPayeeTx(ctx, tx, workspaceID, agentID, payeeID, a); err != nil {
		return AgentRules{}, err
	}
	if err := recordRulesVersion(ctx, tx, agentID, rulesChangeFrom(ctx)); err != nil {
		return AgentRules{}, err
	}
	r, err := scanAgentRules(tx.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if err != nil {
		return r, fmt.Errorf("economy: always allow: %w", err)
	}
	return r, tx.Commit(ctx)
}

// allowPayeeTx sets the agent's allowance for payeeID to a in tx, keeping its other allowances and rules. a names its
// amount in the rules' currency (B61.40), and its worth in LXC is written beside it; one named in µLXC alone, as
// every client before B61.40 does, is moved to dollars at the peg, and refused on rules in another currency.
func (s *DualTokenStore) allowPayeeTx(ctx context.Context, tx pgx.Tx, workspaceID, agentID, payeeID string, a PayeeAllowance) error {
	if payeeID == "" {
		return fmt.Errorf("%w: only a payment has a payee to always allow", ErrAgentRule)
	}
	if err := (AgentRules{PayeeAllowances: map[string]PayeeAllowance{payeeID: a}}).validate(); err != nil || a.ULXC == 0 && a.Minor == 0 {
		return fmt.Errorf("%w: always allow needs an amount above 0 and per \"day\" or \"month\"", ErrAgentRule)
	}
	currency := CurrencyUSD
	if err := tx.QueryRow(ctx, `SELECT currency FROM agent_rules WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID).
		Scan(&currency); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("economy: always allow: %w", err)
	}
	switch {
	case a.Minor > 0:
		v, err := s.minorInLXC(ctx, workspaceID, a.Minor, currency)
		if err != nil {
			return err
		}
		a.ULXC = v
	case currency != CurrencyUSD:
		return fmt.Errorf("%w: they are in %s; name the amount always allowed in it (amount_minor)", ErrRulesCurrency, currency)
	default:
		a.Minor = centsAtPeg(a.ULXC)
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO agent_rules (agent_id, workspace_id, payee_allowances)
		SELECT id, workspace_id, jsonb_build_object($3::text, $4::jsonb) FROM agent_accounts WHERE id = $1 AND workspace_id = $2
		ON CONFLICT (agent_id) DO UPDATE SET payee_allowances = agent_rules.payee_allowances || EXCLUDED.payee_allowances, updated_at = now()`,
		agentID, workspaceID, payeeID, string(b))
	if err != nil {
		return fmt.Errorf("economy: always allow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// judgeCommitment judges the marketplace licence a payment takes, if it takes one (B32.22), against r — the zero rules
// for an agent with none, which let it take a commercial or an enterprise licence at any price but never subscribe.
// Each refusal names its rule.
func (r AgentRules) judgeCommitment(c Commitment) error {
	switch {
	case c.Kind == "":
		return nil
	case !slices.Contains(AgentLicences, c.Licence):
		return ruleRefusal("an agent may hold only commercial and enterprise licences, and this %s is %s: a personal licence is one person's", c.Kind, c.Licence)
	case len(r.AllowedLicences) > 0 && !slices.Contains(r.AllowedLicences, c.Licence):
		return ruleRefusal("the agent may hold only %s licences, and this %s is %s",
			strings.Join(r.AllowedLicences, " and "), c.Kind, c.Licence)
	case c.Kind == "subscribe" && (r.MaySubscribe == nil || !*r.MaySubscribe):
		return ruleRefusal("the agent's rules do not let it subscribe")
	case c.Renews && (r.MaySubscribe == nil || !*r.MaySubscribe): // a rent that renews itself is a subscription
		return ruleRefusal("the agent's rules do not let it take a %s that renews itself", c.Kind)
	case limitOf(r.MaxCommitmentULXC) > 0 && c.ULXC > *r.MaxCommitmentULXC:
		return ruleRefusal("this %s would commit the agent to %s; the most one licence may commit it to is %s",
			c.Kind, LXCMoney(c.ULXC), LXCMoney(*r.MaxCommitmentULXC))
	}
	return nil
}

// fileApproval records the approval a refused request needs — or finds the one already open for it — once
// the refused transaction has rolled back, and returns the refusal naming it.
func (s *DualTokenStore) fileApproval(ctx context.Context, need *ApprovalNeededError) error {
	payee, err := namePayee(ctx, s.pool, need.req.Payee)
	if err != nil {
		return err
	}
	if need.Expired, err = s.expireApprovals(ctx, need.agentID, need.req.Fingerprint); err != nil {
		return err
	}
	from, err := s.paidFrom(ctx, need)
	if err != nil {
		return err
	}
	version, err := latestRulesVersion(ctx, s.pool, need.agentID)
	if err != nil {
		return err
	}
	client, _ := ctx.Value(connectClientKey{}).(string)
	var filed string
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if client != "" {
			// B54.13: an assistant leaves at most ConnectApprovalsWaiting new approvals waiting; under the agent's lock, so
			// two at once cannot both be the last. A retry of one already open finds it below.
			if err := lockAgent(ctx, tx, need.workspaceID, need.agentID); err != nil {
				return err
			}
			var open bool
			var waiting int
			if err := tx.QueryRow(ctx, `SELECT
				EXISTS (SELECT 1 FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2 AND status IN ('pending', 'approved')),
				(SELECT count(*) FROM agent_approvals WHERE agent_id = $1 AND connect_client_id = $3 AND status = 'pending' AND expires_at > now())`,
				need.agentID, need.req.Fingerprint, client).Scan(&open, &waiting); err != nil {
				return fmt.Errorf("economy: approvals waiting: %w", err)
			}
			if !open && waiting >= ConnectApprovalsWaiting {
				return ErrConnectApprovalsWaiting
			}
		}
		if err := declined(ctx, tx, need.agentID, need.req.Fingerprint); err != nil { // B55.18
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, model, payee_kind, payee_id, payee_name, memo, expires_at, connect_client_id,
				paid_from_kind, paid_from_account_id, paid_from_currency, paid_from_amount_minor, paid_from_funding, asks_above_ulxc, budget_left_after_ulxc,
				rules_currency, asks_above_minor, budget_left_after_minor, rule, rules_version, task)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now() + $11 * interval '1 microsecond', $12, $13, $14, $15, $16, $17, $18, $19,
				$20, $21, $22, $23, $24, $25)
			ON CONFLICT (agent_id, fingerprint) WHERE status IN ('pending', 'approved') DO NOTHING RETURNING id`,
			"apr_"+uuid.NewString(), need.workspaceID, need.agentID, need.req.Fingerprint, need.AmountULXC, need.req.Model,
			payee.Kind, payee.ID, payee.Name, need.req.Memo, s.approvalLife().Microseconds(), client,
			from.Kind, from.AccountID, from.Currency, from.AmountMinor, from.funding, need.asksAbove, need.monthLeft,
			need.inRules.currency, need.inRules.asksAboveOrNil(), need.inRules.monthLeft, need.rule, version, taskIn(ctx, need.workspaceID)).Scan(&filed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("economy: file approval: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if filed != "" {
		s.notifyApproval(need.workspaceID, filed) // B19.16: a retry finds the open one and tells nobody again
	}
	if err := s.pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2
		AND status IN ('pending', 'approved')`, need.agentID, need.req.Fingerprint).Scan(&need.ApprovalID); err != nil {
		return fmt.Errorf("economy: read approval: %w", err)
	}
	return need
}

// paidFrom is what pays the request need is for (B57.11): what its caller named, else the marketplace bill for a
// listing's use and the agent's LXC for anything else. An account named by its currency alone is the agent's open one.
func (s *DualTokenStore) paidFrom(ctx context.Context, need *ApprovalNeededError) (PaidFrom, error) {
	p := need.req.PaidFrom
	switch {
	case p.Kind == "account" && p.AccountID == "":
		err := s.pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2 AND currency = $3
			AND purpose = 'agent' AND status = 'open'`, need.workspaceID, need.agentID, p.Currency).Scan(&p.AccountID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return p, fmt.Errorf("economy: paying account: %w", err)
		}
	case p.Kind != "":
	case need.req.Listing != "":
		p = PaidFrom{Kind: "marketplace_bill", AccountID: need.workspaceID}
		p.AmountMinor, p.Currency = shownAmount(need.req, need.AmountULXC)
	default:
		p = PaidFrom{Kind: "lxc", AccountID: need.agentID}
		p.AmountMinor, p.Currency = shownAmount(need.req, need.AmountULXC)
	}
	return p, nil
}

// declined is approvalDeclined when the owner declined the agent's request of fingerprint within the approval life
// (B55.18): the decline's expires_at is when the same request may ask again. Every place that files an approval asks it
// in the filing's transaction, first locking the request's open approval: a retry racing the owner's decline waits for
// it, and this statement, run after, sees it.
func declined(ctx context.Context, tx pgx.Tx, agentID, fingerprint string) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2 AND status IN ('pending', 'approved')
		FOR UPDATE`, agentID, fingerprint); err != nil {
		return fmt.Errorf("economy: lock the open approval: %w", err)
	}
	var d approvalDeclined
	err := tx.QueryRow(ctx, `SELECT decided_at, expires_at, deny_reason FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2
		AND status = 'denied' AND expires_at > now() ORDER BY expires_at DESC LIMIT 1`, agentID, fingerprint).Scan(&d.at, &d.until, &d.reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: declined approval: %w", err)
	}
	return d
}

// refusedMovement is what a hold or debit returns when agentMovement refused it: a request that needs
// approval files it (after rolling back, so nothing of the refused movement is kept); anything else as is.
func (s *DualTokenStore) refusedMovement(ctx context.Context, tx pgx.Tx, err error) error {
	var changed *ApprovalChangedError
	if errors.As(err, &changed) { // B62.1: rolled back, so what it held is released; the owner is told
		_ = tx.Rollback(ctx)
		s.tellApprovalChanged(changed)
		return err
	}
	var need *ApprovalNeededError
	if !errors.As(err, &need) {
		return err
	}
	_ = tx.Rollback(ctx)
	return s.fileApproval(ctx, need)
}

// AgentApproval is one request that needed a person's approval.
type AgentApproval struct {
	ID         string     `json:"id"`
	AgentID    string     `json:"agent_id"`
	AmountULXC int64      `json:"amount_ulxc"`
	Model      string     `json:"model"`
	Reason     string     `json:"reason,omitempty"` // why the agent asked (B19.9)
	Payee      *Payee     `json:"payee,omitempty"`  // who a payment goes to (B23.5); none for a request to a model
	Memo       string     `json:"memo,omitempty"`   // the payment's memo
	Status     string     `json:"status"`           // pending | approved | denied | used | expired
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	// ExpiresAt is when it stops being usable (B50.18): a pending one ApprovalTTL after it was filed, an approved one
	// ApprovalTTL after the tap.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// ApprovedByAgentID is the approver agent that approved it within its own limits (B45.28); empty for the owner.
	ApprovedByAgentID string `json:"approved_by_agent_id,omitempty"`
	DenyReason        string `json:"deny_reason,omitempty"` // what the owner wrote when declining (B55.18)
	// PaidFrom is what pays it, with its balance now (B57.11); null on one filed before it was kept.
	PaidFrom *PaidFrom `json:"paid_from"`
	// AsksAboveULXC is the approval amount it was above, and BudgetLeftAfterULXC the monthly limit less the month's
	// spend and this amount, both when it was filed; null with no monthly limit, and both on one filed before they were kept.
	AsksAboveULXC       *int64 `json:"asks_above_ulxc"`
	BudgetLeftAfterULXC *int64 `json:"budget_left_after_ulxc"`
	// RulesCurrency is the currency the agent's rules were held in when it was filed, and AsksAboveMinor and
	// BudgetLeftAfterMinor the same two figures in its minor units (B61.42) — what the sheet says; "" and nulls on one
	// filed before they were kept.
	RulesCurrency        string `json:"rules_currency"`
	AsksAboveMinor       *int64 `json:"asks_above_minor"`
	BudgetLeftAfterMinor *int64 `json:"budget_left_after_minor"`
	// Kind is "" for a request of the agent's, and ApprovalCardChallenge for a 3-D Secure challenge on its card (B59.22).
	Kind string `json:"kind,omitempty"`
	// ConnectClientID is the assistant whose payment filed it (B52.41), so an approval screen can name it; empty otherwise.
	ConnectClientID string `json:"connect_client_id,omitempty"`
	// Task is what the agent said the payment is for (B62.2), in its own words: shown after TaskSays, never as a fact.
	// "" when it gave none.
	Task string `json:"task"`
}

// agentApprovalColumns reads an approval, unaliased, from agent_approvals: the paying agent's name and the payer's balance now too.
const agentApprovalColumns = `id, agent_id, amount_ulxc, model, reason, payee_kind, payee_id, payee_name, memo,
	CASE WHEN status IN ('pending', 'approved') AND expires_at <= now() THEN 'expired' ELSE status END, created_at, decided_at,
	approved_by_agent_id, expires_at, deny_reason, paid_from_kind, paid_from_account_id, paid_from_currency, paid_from_amount_minor,
	COALESCE((SELECT name FROM agent_accounts WHERE id = agent_approvals.agent_id), ''),
	CASE paid_from_kind
		WHEN 'lxc' THEN (SELECT COALESCE(sum(balance_ulxc), 0) FROM agent_account_balances
			WHERE workspace_id = agent_approvals.workspace_id AND account = 'agent:' || agent_approvals.agent_id)
		WHEN 'account' THEN (SELECT COALESCE(sum(balance_minor), 0) FROM money_account_balances
			WHERE account_id = agent_approvals.paid_from_account_id AND agent_approvals.paid_from_funding IN ('', funding))
	END::bigint, asks_above_ulxc, budget_left_after_ulxc, kind, connect_client_id, rules_currency, asks_above_minor,
	budget_left_after_minor, task`

func scanAgentApproval(row pgx.Row) (AgentApproval, error) {
	var a AgentApproval
	var p Payee
	var from PaidFrom
	var kind *string
	var amountMinor *int64
	var agent string
	err := row.Scan(&a.ID, &a.AgentID, &a.AmountULXC, &a.Model, &a.Reason, &p.Kind, &p.ID, &p.Name, &a.Memo, &a.Status, &a.CreatedAt, &a.DecidedAt,
		&a.ApprovedByAgentID, &a.ExpiresAt, &a.DenyReason, &kind, &from.AccountID, &from.Currency, &amountMinor, &agent, &from.BalanceMinor,
		&a.AsksAboveULXC, &a.BudgetLeftAfterULXC, &a.Kind, &a.ConnectClientID, &a.RulesCurrency, &a.AsksAboveMinor, &a.BudgetLeftAfterMinor, &a.Task)
	if p.Kind != "" {
		a.Payee = &p
	}
	if kind != nil && amountMinor != nil {
		from.Kind, from.AmountMinor, from.Name = *kind, *amountMinor, paidFromName(*kind, agent, from.Currency)
		a.PaidFrom = &from
	}
	return a, err
}

// GetAgentApproval reads one of a workspace's approvals.
func (s *DualTokenStore) GetAgentApproval(ctx context.Context, workspaceID, approvalID string) (AgentApproval, error) {
	a, err := scanAgentApproval(s.pool.QueryRow(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals WHERE id = $1 AND workspace_id = $2`,
		approvalID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrApprovalNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: approval: %w", err)
	}
	return a, nil
}

// ListAgentApprovals reads a workspace's approvals, newest first.
func (s *DualTokenStore) ListAgentApprovals(ctx context.Context, workspaceID string) ([]AgentApproval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals WHERE workspace_id = $1
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: approvals: %w", err)
	}
	defer rows.Close()
	out := []AgentApproval{}
	for rows.Next() {
		a, err := scanAgentApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideAgentApproval approves or denies a pending approval still within its life.
func (s *DualTokenStore) DecideAgentApproval(ctx context.Context, workspaceID, approvalID string, approve bool) (AgentApproval, error) {
	status := "denied"
	if approve {
		status = "approved"
		if err := approvalsLiveKeyFirst(ctx, s.pool, workspaceID, []string{approvalID}); err != nil { // B58.7
			return AgentApproval{}, err
		}
	}
	a, err := scanAgentApproval(s.pool.QueryRow(ctx, `UPDATE agent_approvals SET status = $3, decided_at = now(), expires_at = now() + $4 * interval '1 microsecond',
		deny_reason = CASE WHEN $3 = 'denied' THEN $5 ELSE '' END
		WHERE id = $1 AND workspace_id = $2 AND status = 'pending' AND expires_at > now() AND kind = '' RETURNING `+agentApprovalColumns,
		approvalID, workspaceID, status, s.approvalLife().Microseconds(), denyReasonFrom(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrApprovalNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: decide approval: %w", err)
	}
	return a, nil
}

// defaultApprovalTTL is how long an approval lasts (B50.18, Nick 28 Oct): a request waits that long for its tap, and
// once approved may be used that long.
const defaultApprovalTTL = 24 * time.Hour

// SetApprovalTTL sets how long an approval lasts; 0 is defaultApprovalTTL.
func (s *DualTokenStore) SetApprovalTTL(d time.Duration) { s.approvalTTL = d }

func (s *DualTokenStore) approvalLife() time.Duration {
	if s.approvalTTL > 0 {
		return s.approvalTTL
	}
	return defaultApprovalTTL
}

// expireApprovals marks the agent's open approvals of fingerprint that are past their life 'expired', so a fresh one
// can be filed; approved is whether one of them had been approved.
func (s *DualTokenStore) expireApprovals(ctx context.Context, agentID, fingerprint string) (approved bool, err error) {
	err = s.pool.QueryRow(ctx, `WITH past AS (SELECT id, status FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2
			AND status IN ('pending', 'approved') AND expires_at <= now() FOR UPDATE),
		expired AS (UPDATE agent_approvals a SET status = 'expired' FROM past WHERE a.id = past.id RETURNING past.status)
		SELECT COALESCE(bool_or(status = 'approved'), false) FROM expired`, agentID, fingerprint).Scan(&approved)
	if err != nil {
		return false, fmt.Errorf("economy: expire approvals: %w", err)
	}
	return approved, nil
}
