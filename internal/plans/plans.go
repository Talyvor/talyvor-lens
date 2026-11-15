// Package plans is what each plan unlocks and costs — Nicolai's decision of 15 Nov 2026 (B61.57), which replaced the
// 5 Oct price card (B32.12): two ladders, one for a personal account (Solo, Crew, Fleet) and one for a business
// (Studio, Company, Group, and Custom by contract), chosen by the account's type. Two settings, LENS_PLAN_GATES
// (agents, seats, own provider keys and the rest) and LENS_PLAN_PRICES_MINOR (each plan's monthly price in GBP, EUR
// and USD), whose defaults in lens.env.example are his values;
// every feature a plan gates asks For which gates a workspace has, and a refusal says in words what the plan allows
// and the plan that would allow more, with the setting in a field of its own (B55.29). A gate whose feature does not exist yet is enforced by the item that builds it, which flips its entry in Built.
package plans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Setting is the environment variable the gates are read from.
const Setting = "LENS_PLAN_GATES"

// Unlimited is a limit with no ceiling.
const Unlimited = -1

// The plans, by the key every setting and table names them by (B61.57). The names are Talyvor's own.
const (
	PersonalSolo    = "personal_solo"
	PersonalCrew    = "personal_crew"
	PersonalFleet   = "personal_fleet"
	BusinessStudio  = "business_studio"
	BusinessCompany = "business_company"
	BusinessGroup   = "business_group"
	BusinessCustom  = "business_custom" // by contract, set by the operator, never chosen in the app
)

// The two account types a ladder is for (economy.AccountTypePersonal and economy.AccountTypeBusiness).
const (
	AccountPersonal = "personal"
	AccountBusiness = "business"
)

// PersonalLadder and BusinessLadder are each type's plans from the smallest up: a refusal looks up its own ladder
// for the plan that would allow more, and an account moves only along its own.
var (
	PersonalLadder = []string{PersonalSolo, PersonalCrew, PersonalFleet}
	BusinessLadder = []string{BusinessStudio, BusinessCompany, BusinessGroup, BusinessCustom}
)

// Order is every plan: the personal ladder, then the business one.
var Order = append(slices.Clone(PersonalLadder), BusinessLadder...)

// Ladder is the plans an account of accountType chooses among; any type but personal is a business's.
func Ladder(accountType string) []string {
	if accountType == AccountPersonal {
		return PersonalLadder
	}
	return BusinessLadder
}

// Step is plan's place on its ladder from 0, the first; -1 for no plan.
func Step(plan string) int {
	if i := slices.Index(PersonalLadder, plan); i >= 0 {
		return i
	}
	return slices.Index(BusinessLadder, plan)
}

// Successor is the plan an account of accountType on a 5 Oct plan is on now (B61.57): free the first step of its
// ladder, team the second, business the third, enterprise Custom — a personal one Fleet. byok took team's gates and the
// chat plans plus, pro and max team's fees, so each goes to the second step too, where it pays no more than it did and
// loses no agent. "" for a plan that is none of them.
func Successor(accountType, old string) string {
	ladder := Ladder(accountType)
	switch old {
	case "free":
		return ladder[0]
	case "team", "byok", "plus", "pro", "max":
		return ladder[1]
	case "business":
		return ladder[2]
	case "enterprise":
		return ladder[len(ladder)-1]
	}
	return ""
}

// What a plan says about the workspace's own provider keys.
const (
	OwnKeysNone     = "none"     // never
	OwnKeysAddOn    = "add_on"   // only with the BYOK add-on
	OwnKeysIncluded = "included" // always
)

// Gates is what one plan unlocks.
type Gates struct {
	Agents  int64  `json:"agents"`            // agents a workspace may have; -1 is unlimited
	Seats   int64  `json:"seats"`             // members a workspace may have; -1 is unlimited
	OwnKeys string `json:"own_provider_keys"` // none, add_on or included
	// false keeps every capability B30 registers, and every RED or AMBER one, on test money, even with a
	// clearance; true takes each live as it is cleared. Every plan's default is true (B61.57): the clearance, the
	// verification level and the country decide, not the plan.
	LiveMoney           bool `json:"live_money"`
	SlackTeamsApprovals bool `json:"slack_teams_approvals"`
	SSO                 bool `json:"sso"`
	AuditExport         bool `json:"audit_export"`
	Edge                bool `json:"edge"` // Talyvor Edge
}

// Built is, by each gate's name in GET /v1/public/plan-gates, whether Lens enforces it today (B56.8). The item that
// builds a switch flips its entry in the same change; a test holds each entry to whether code outside this package
// reads that gate, itself or through a method of this package it calls.
var Built = map[string]bool{
	"agents": true, "seats": true, "own_provider_keys": true, "live_money": true,
	"slack_teams_approvals": true, "sso": true, "audit_export": true, "edge": false,
}

// Defaults are Nicolai's values (B61.57): agents 3, 10 and 25 for a person and 3, 25 and unlimited for a company, one
// person on each personal plan; what the 5 Oct plans unlocked beyond money follows the same step of the business
// ladder, and a personal plan has none of it.
func Defaults() map[string]Gates {
	return map[string]Gates{
		PersonalSolo:    {Agents: 3, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		PersonalCrew:    {Agents: 10, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		PersonalFleet:   {Agents: 25, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		BusinessStudio:  {Agents: 3, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		BusinessCompany: {Agents: 25, Seats: 5, OwnKeys: OwnKeysAddOn, LiveMoney: true, SlackTeamsApprovals: true},
		BusinessGroup:   {Agents: Unlimited, Seats: 25, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true},
		BusinessCustom:  {Agents: Unlimited, Seats: Unlimited, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true, Edge: true},
	}
}

// PricesSetting is the environment variable each plan's monthly price is read from.
const PricesSetting = "LENS_PLAN_PRICES_MINOR"

// Currencies are the currencies a plan is priced in. A stablecoin account pays the USD figure (PriceIn).
var Currencies = []string{"GBP", "EUR", "USD"}

// DefaultPrices are Nicolai's monthly prices in minor units (B61.57), by plan and currency. Custom is by contract and
// has none.
func DefaultPrices() map[string]map[string]int64 {
	return map[string]map[string]int64{
		PersonalSolo:    {"GBP": 0, "EUR": 0, "USD": 0},
		PersonalCrew:    {"GBP": 399, "EUR": 399, "USD": 399},
		PersonalFleet:   {"GBP": 799, "EUR": 899, "USD": 999},
		BusinessStudio:  {"GBP": 1000, "EUR": 1000, "USD": 1000},
		BusinessCompany: {"GBP": 3500, "EUR": 3500, "USD": 3500},
		BusinessGroup:   {"GBP": 12500, "EUR": 12500, "USD": 12500},
	}
}

// LoadPrices reads the prices from getenv: unset is the defaults; set, it names every plan but Custom, each in every
// one of Currencies, in whole minor units of 0 or more.
func LoadPrices(getenv func(string) string) (map[string]map[string]int64, error) {
	v := strings.TrimSpace(getenv(PricesSetting))
	if v == "" {
		return DefaultPrices(), nil
	}
	var m map[string]map[string]int64
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return nil, fmt.Errorf("plans: %s must be a JSON object of each plan's monthly price in minor units by currency, like the default in lens.env.example: %w", PricesSetting, err)
	}
	for name := range m {
		if !known(name) || name == BusinessCustom {
			return nil, fmt.Errorf("plans: %s names %q; the priced plans are every one of %s but %s, which is by contract", PricesSetting, name, strings.Join(Order, ", "), BusinessCustom)
		}
	}
	for _, name := range Order {
		if name == BusinessCustom {
			continue
		}
		prices, ok := m[name]
		if !ok || len(prices) != len(Currencies) {
			return nil, fmt.Errorf("plans: %s must price the %s plan in %s, and only those", PricesSetting, name, strings.Join(Currencies, ", "))
		}
		for _, c := range Currencies {
			if n, ok := prices[c]; !ok || n < 0 {
				return nil, fmt.Errorf("plans: %s[%q][%q] must be whole minor units, 0 or more", PricesSetting, name, c)
			}
		}
	}
	return m, nil
}

// Load reads the gates from getenv: unset is the defaults; set, it names every plan in Order.
func Load(getenv func(string) string) (map[string]Gates, error) {
	v := strings.TrimSpace(getenv(Setting))
	if v == "" {
		return Defaults(), nil
	}
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()
	var m map[string]Gates
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plans: %s must be a JSON object of each plan's gates, like the default in lens.env.example: %w", Setting, err)
	}
	for name := range m {
		if !known(name) {
			return nil, fmt.Errorf("plans: %s names %q; the plans are %s", Setting, name, strings.Join(Order, ", "))
		}
	}
	for _, name := range Order {
		g, ok := m[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("plans: %s does not name the %s plan", Setting, name)
		case g.Agents < Unlimited || g.Seats < Unlimited:
			return nil, fmt.Errorf("plans: %s[%q] has agents %d and seats %d; each is a count, or -1 for unlimited", Setting, name, g.Agents, g.Seats)
		case g.OwnKeys != OwnKeysNone && g.OwnKeys != OwnKeysAddOn && g.OwnKeys != OwnKeysIncluded:
			return nil, fmt.Errorf("plans: %s[%q].own_provider_keys is %q; it is none, add_on or included", Setting, name, g.OwnKeys)
		}
	}
	return m, nil
}

func known(plan string) bool {
	for _, p := range Order {
		if p == plan {
			return true
		}
	}
	return false
}

var (
	once          sync.Once
	current       map[string]Gates
	currentPrices map[string]map[string]int64
	loadErr       error
)

func load() {
	if current, loadErr = Load(os.Getenv); loadErr == nil {
		currentPrices, loadErr = LoadPrices(os.Getenv)
	}
}

// Check reports a malformed LENS_PLAN_GATES or LENS_PLAN_PRICES_MINOR; Lens will not start with one.
func Check() error {
	once.Do(load)
	return loadErr
}

// CurrentPrices is every plan's monthly price as this process runs with them, read from its environment once.
func CurrentPrices() map[string]map[string]int64 {
	once.Do(load)
	if loadErr != nil {
		return DefaultPrices()
	}
	out := make(map[string]map[string]int64, len(currentPrices))
	for k, v := range currentPrices {
		out[k] = maps.Clone(v)
	}
	return out
}

// PriceIn is plan's monthly price in currency under prices, in minor units, and the currency it is charged in: a
// stablecoin account's is the USD figure. false for Custom, by contract, and a currency no plan is priced in.
func PriceIn(prices map[string]map[string]int64, plan, currency string) (int64, string, bool) {
	if currency == "USDC" {
		currency = "USD"
	}
	n, ok := prices[plan][currency]
	return n, currency, ok
}

// Paid reports whether plan costs anything a month in any currency under prices; Custom, by contract, always does.
func Paid(prices map[string]map[string]int64, plan string) bool {
	if plan == BusinessCustom {
		return true
	}
	for _, n := range prices[plan] {
		if n > 0 {
			return true
		}
	}
	return false
}

// Current is every plan's gates as this process runs with them, read from its environment once.
func Current() map[string]Gates {
	once.Do(load)
	if loadErr != nil {
		return Defaults()
	}
	return maps.Clone(current)
}

// Querier is a pool or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PlanOf answers which plan workspaceID is on (B61.57): Custom while a business is on the operator's contract; else the
// plan the account chose, or the migration moved it to (workspaces.plan); else the successor of the 5 Oct plan a paying
// subscription bills (trialing, active or past_due — unpaid is Stripe having given up); else the first step of its
// account type's ladder. A workspace Lens has no row for is a business.
func PlanOf(ctx context.Context, db Querier, workspaceID string) (string, error) {
	w, err := Of(ctx, db, workspaceID)
	return w.Plan, err
}

// Workspace is a workspace's plan and the gates it has.
type Workspace struct {
	Plan        string `json:"plan"`         // as PlanOf answers it
	AccountType string `json:"account_type"` // personal or business: the ladder Plan is on
	GatedAs     string `json:"gated_as"`     // the plan whose gates it takes: Plan, since B61.57
	BYOKAddOn   bool   `json:"byok_add_on"`  // its subscription carries the BYOK Price
	OnContract  bool   `json:"on_contract"`  // a business on the operator's contract, so on Custom
	Gates
}

// Of is workspaceID's plan and its gates under the gates this process runs with.
func Of(ctx context.Context, db Querier, workspaceID string) (Workspace, error) {
	return OfUnder(ctx, db, workspaceID, Current())
}

// OfUnder is workspaceID's plan, as PlanOf answers it, and its gates under gates. A BYOK subscription alone includes
// own keys; an operator's contract's own agents and seats, where it set them, replace Custom's. A synthetic workspace
// takes its plan's gates but never live money, whatever its plan says (B35.1): its money is test money.
func OfUnder(ctx context.Context, db Querier, workspaceID string, gates map[string]Gates) (Workspace, error) {
	var w Workspace
	var contract, chosen, subscribed *string
	var agents, seats *int64
	var synthetic bool
	err := db.QueryRow(ctx, `
		SELECT COALESCE((SELECT account_type FROM workspaces WHERE id = $1), 'business'),
			(SELECT plan FROM workspace_contracts WHERE workspace_id = $1),
			(SELECT plan FROM workspaces WHERE id = $1),
			(SELECT plan FROM subscriptions
			 WHERE workspace_id = $1 AND status IN ('trialing','active','past_due')),
			EXISTS (SELECT 1 FROM subscriptions WHERE workspace_id = $1 AND byok
				AND status IN ('trialing','active','past_due')),
			(SELECT agents FROM workspace_contracts WHERE workspace_id = $1),
			(SELECT seats FROM workspace_contracts WHERE workspace_id = $1),
			COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID).
		Scan(&w.AccountType, &contract, &chosen, &subscribed, &w.BYOKAddOn, &agents, &seats, &synthetic)
	if err != nil {
		return Workspace{}, fmt.Errorf("plans: the plan of %s: %w", workspaceID, err)
	}
	ladder := Ladder(w.AccountType)
	onContract := contract != nil && w.AccountType != AccountPersonal
	switch {
	case onContract:
		w.Plan = BusinessCustom
	case chosen != nil && slices.Contains(ladder, *chosen):
		w.Plan = *chosen
	case subscribed != nil && Successor(w.AccountType, *subscribed) != "":
		w.Plan = Successor(w.AccountType, *subscribed)
	default:
		w.Plan = ladder[0]
	}
	w.GatedAs = w.Plan
	g := gates[w.Plan]
	w.OnContract = onContract
	if subscribed != nil && *subscribed == "byok" && !onContract {
		g.OwnKeys = OwnKeysIncluded
	}
	if onContract && agents != nil {
		g.Agents = *agents
	}
	if onContract && seats != nil {
		g.Seats = *seats
	}
	if synthetic {
		g.LiveMoney = false
	}
	w.Gates = g
	return w, nil
}

// OwnKeysAllowed reports whether the workspace may save and be served on its own provider keys.
func (w Workspace) OwnKeysAllowed() bool {
	return w.OwnKeys == OwnKeysIncluded || (w.OwnKeys == OwnKeysAddOn && w.BYOKAddOn)
}

// Within reports whether n is within limit.
func Within(n, limit int64) bool { return limit == Unlimited || n <= limit }

// Allowing is the first plan on the workspace's own ladder, past its own, whose gates under gates satisfy ok; "" when
// none.
func (w Workspace) Allowing(gates map[string]Gates, ok func(Gates) bool) string {
	past := false
	for _, p := range Ladder(w.AccountType) {
		if past && ok(gates[p]) {
			return p
		}
		past = past || p == w.GatedAs
	}
	return ""
}

// names are the plans as people read them (B61.57).
var names = map[string]string{
	PersonalSolo: "Solo", PersonalCrew: "Crew", PersonalFleet: "Fleet",
	BusinessStudio: "Studio", BusinessCompany: "Company", BusinessGroup: "Group", BusinessCustom: "Custom",
	"byok": "BYOK",
}

// Name is a plan as people read it: Solo, Crew, Fleet, Studio, Company, Group and Custom; a Stripe plan by its own name.
func Name(plan string) string {
	if n, ok := names[plan]; ok {
		return n
	}
	if plan == "" {
		return plan
	}
	return strings.ToUpper(plan[:1]) + plan[1:]
}

// Thousands writes n with a comma between each group of three digits.
func Thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Refusal is something the workspace's plan does not unlock. Its message says, in words, what the plan allows and
// what the plan that would allow more does; the setting it comes from is Setting.
type Refusal struct {
	Setting string `json:"setting,omitempty"` // LENS_PLAN_GATES; "" where the refusal names no setting
	Plan    string `json:"plan"`
	Gate    string `json:"gate"`             // agents, seats, own_provider_keys, live_money, audit_export, slack_teams_approvals or sso
	Limit   *int64 `json:"limit,omitempty"`  // for agents and seats
	Allows  string `json:"allows,omitempty"` // the plan that would allow it; "" when only a contract would
	Detail  string `json:"detail"`
}

func (r *Refusal) Error() string { return r.Detail }

// Is makes every Refusal an ErrRefused.
func (r *Refusal) Is(target error) bool { return target == ErrRefused }

// ErrRefused is the plan not unlocking something.
var ErrRefused = errors.New("plans: the plan does not allow this")

// RefuseCount is the refusal of one more agent or seat than the workspace's plan allows: "Studio allows 3 agents, and
// all 3 are in use. Company allows 25." Past a personal ladder's last plan nothing allows more; past a business's,
// a contract does.
func (w Workspace) RefuseCount(gates map[string]Gates, gate string, limit int64) *Refusal {
	noun := map[string]string{"agents": "agent", "seats": "seat"}[gate]
	allows := w.Allowing(gates, func(g Gates) bool {
		n := map[string]int64{"agents": g.Agents, "seats": g.Seats}[gate]
		return n == Unlimited || n > limit
	})
	used := map[string]string{"agents": "in use", "seats": "taken"}[gate]
	var detail string
	switch limit {
	case 0:
		detail = fmt.Sprintf("%s allows no %ss.", Name(w.Plan), noun)
	case 1:
		detail = fmt.Sprintf("%s allows 1 %s, and it is %s.", Name(w.Plan), noun, used)
	default:
		detail = fmt.Sprintf("%s allows %s %ss, and all %s are %s.", Name(w.Plan), Thousands(limit), noun, Thousands(limit), used)
	}
	switch n := gates[allows].countOf(gate); {
	case allows == "" && w.AccountType == AccountPersonal:
	case allows == "":
		detail += " A contract with Talyvor sets more."
	case n == Unlimited:
		detail += fmt.Sprintf(" %s allows unlimited %ss.", Name(allows), noun)
	default:
		detail += fmt.Sprintf(" %s allows %s.", Name(allows), Thousands(n))
	}
	return &Refusal{Setting: Setting, Plan: w.Plan, Gate: gate, Limit: &limit, Allows: allows, Detail: detail}
}

func (g Gates) countOf(gate string) int64 {
	if gate == "seats" {
		return g.Seats
	}
	return g.Agents
}

// CheckSeats refuses members past the seats the workspace's plan allows; nil when they fit.
func (w Workspace) CheckSeats(members int64) error {
	if Within(members, w.Seats) {
		return nil
	}
	return w.RefuseCount(Current(), "seats", w.Seats)
}

// RefuseOwnKeys is the refusal of an own provider key on a plan that does not allow one: "Studio does not include your
// own provider keys. Company has them with the BYOK add-on, and Group includes them."
func (w Workspace) RefuseOwnKeys(gates map[string]Gates) *Refusal {
	r := &Refusal{Setting: Setting, Plan: w.Plan, Gate: "own_provider_keys"}
	included := w.Allowing(gates, func(g Gates) bool { return g.OwnKeys == OwnKeysIncluded })
	if w.OwnKeys == OwnKeysAddOn {
		r.Allows = included
		r.Detail = fmt.Sprintf("%s has your own provider keys only with the BYOK add-on.", Name(w.Plan))
		if r.Allows != "" {
			r.Detail += fmt.Sprintf(" %s includes them.", Name(r.Allows))
		}
		return r
	}
	r.Allows = w.Allowing(gates, func(g Gates) bool { return g.OwnKeys != OwnKeysNone })
	r.Detail = fmt.Sprintf("%s does not include your own provider keys.", Name(w.Plan))
	switch {
	case r.Allows != "" && gates[r.Allows].OwnKeys == OwnKeysAddOn && included != "":
		r.Detail += fmt.Sprintf(" %s has them with the BYOK add-on, and %s includes them.", Name(r.Allows), Name(included))
	case r.Allows != "" && gates[r.Allows].OwnKeys == OwnKeysAddOn:
		r.Detail += fmt.Sprintf(" %s has them with the BYOK add-on.", Name(r.Allows))
	case r.Allows != "":
		r.Detail += fmt.Sprintf(" %s includes them.", Name(r.Allows))
	}
	return r
}

// RefuseLiveMoney is the refusal of live money for a capability the workspace's plan keeps on test money, where
// LENS_PLAN_GATES turns a plan's live money off: "On Studio, Escrow takes no real money. On Company it does, once it
// is cleared."
func (w Workspace) RefuseLiveMoney(gates map[string]Gates, capability string) *Refusal {
	r := &Refusal{Setting: Setting, Plan: w.Plan, Gate: "live_money", Allows: w.Allowing(gates, func(g Gates) bool { return g.LiveMoney })}
	r.Detail = fmt.Sprintf("On %s, %s takes no real money.", Name(w.Plan), capability)
	if r.Allows != "" {
		r.Detail += fmt.Sprintf(" On %s it does, once it is cleared.", Name(r.Allows))
	}
	return r
}

// RefuseSlackTeams is the refusal of Slack and Teams approvals on a plan without them: "Studio does not include Slack
// and Teams approvals. Company includes them." It names no setting, like RefuseAuditExport.
func (w Workspace) RefuseSlackTeams(gates map[string]Gates) *Refusal {
	r := &Refusal{Plan: w.Plan, Gate: "slack_teams_approvals", Allows: w.Allowing(gates, func(g Gates) bool { return g.SlackTeamsApprovals })}
	r.Detail = fmt.Sprintf("%s does not include Slack and Teams approvals.", Name(w.Plan))
	if r.Allows != "" {
		r.Detail += fmt.Sprintf(" %s includes them.", Name(r.Allows))
	}
	return r
}

// RefuseAuditExport is the refusal of audit export on a plan without it: "Company does not include audit export. Group
// includes it." It names no setting (B56.5).
func (w Workspace) RefuseAuditExport(gates map[string]Gates) *Refusal {
	r := &Refusal{Plan: w.Plan, Gate: "audit_export", Allows: w.Allowing(gates, func(g Gates) bool { return g.AuditExport })}
	r.Detail = fmt.Sprintf("%s does not include audit export.", Name(w.Plan))
	if r.Allows != "" {
		r.Detail += fmt.Sprintf(" %s includes it.", Name(r.Allows))
	}
	return r
}

// RefuseSSO is the refusal of single sign-on on a plan without it: "Company does not include single sign-on. Group
// includes it." It names no setting, like RefuseAuditExport.
func (w Workspace) RefuseSSO(gates map[string]Gates) *Refusal {
	r := &Refusal{Plan: w.Plan, Gate: "sso", Allows: w.Allowing(gates, func(g Gates) bool { return g.SSO })}
	r.Detail = fmt.Sprintf("%s does not include single sign-on.", Name(w.Plan))
	if r.Allows != "" {
		r.Detail += fmt.Sprintf(" %s includes it.", Name(r.Allows))
	}
	return r
}

// ChangeRefusal is a plan change the owner may not make, with why in words (B61.57). Unknown is true for a plan
// that does not exist, the one refusal that is the asker's mistake rather than the account's state.
type ChangeRefusal struct {
	Plan    string `json:"plan"` // the plan the account is on and stays on
	To      string `json:"to"`
	Unknown bool   `json:"-"`
	Detail  string `json:"detail"`
}

func (r *ChangeRefusal) Error() string { return r.Detail }

// ErrNoWorkspace is a plan change for a workspace Lens has no row for.
var ErrNoWorkspace = errors.New("plans: no such workspace")

// Tx is the transaction a plan change runs in.
type Tx interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Change moves workspaceID to plan to in tx and answers the plan it was on (B61.57). The owner chooses any plan of
// their own ladder but Custom, which only a contract sets; an account on a contract stays on it. While paidRefused
// (Lens's Stripe key is live and no plan is charged yet) a plan with a price is refused. A move down is refused while
// the account has more agents than the plan allows, or more people — members, when the caller counts them — saying
// how many to archive or remove. It takes the workspace's agents lock, so no agent is created past the new count while
// it decides. Nothing is charged: the change takes effect when tx commits, and its caller writes the audit row in tx.
func Change(ctx context.Context, tx Tx, workspaceID, to string, members *int64, paidRefused bool) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agents:' || $1, 0))`, workspaceID); err != nil {
		return "", fmt.Errorf("plans: lock the workspace's agents: %w", err)
	}
	w, err := Of(ctx, tx, workspaceID)
	if err != nil {
		return "", err
	}
	refuse := func(unknown bool, format string, a ...any) (string, error) {
		return w.Plan, &ChangeRefusal{Plan: w.Plan, To: to, Unknown: unknown, Detail: fmt.Sprintf(format, a...)}
	}
	ladder := Ladder(w.AccountType)
	choices := make([]string, 0, len(ladder))
	for _, p := range ladder {
		if p != BusinessCustom {
			choices = append(choices, Name(p))
		}
	}
	gates := Current()
	g := gates[to]
	switch {
	case Step(to) < 0:
		return refuse(true, "There is no plan called %q.", to)
	case to == BusinessCustom:
		return refuse(false, "Custom is set by a contract with Talyvor.")
	case !slices.Contains(ladder, to):
		return refuse(false, "A %s account chooses %s.", w.AccountType, strings.Join(choices[:len(choices)-1], ", ")+" or "+choices[len(choices)-1])
	case w.OnContract:
		return refuse(false, "This account is on a contract with Talyvor, which sets its plan.")
	case to == w.Plan:
		return refuse(false, "This account is already on %s.", Name(to))
	case paidRefused && Paid(CurrentPrices(), to):
		return refuse(false, "%s cannot be chosen yet: paid plans open once Talyvor can take their price. This account stays on %s.", Name(to), Name(w.Plan))
	}
	var agents int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_accounts WHERE workspace_id = $1 AND archived_at IS NULL AND kind = 'agent'`,
		workspaceID).Scan(&agents); err != nil {
		return "", fmt.Errorf("plans: count agents: %w", err)
	}
	if !Within(agents, g.Agents) {
		return refuse(false, "%s allows %s, and this account has %s. Archive %s first.", Name(to), count(g.Agents, "agent"), Thousands(agents), count(agents-g.Agents, "agent"))
	}
	if members != nil && !Within(*members, g.Seats) {
		return refuse(false, "%s allows %s, and this account has %s. Remove %s first.", Name(to), count(g.Seats, "person"), Thousands(*members), count(*members-g.Seats, "person"))
	}
	tag, err := tx.Exec(ctx, `UPDATE workspaces SET plan = $2, updated_at = NOW() WHERE id = $1`, workspaceID, to)
	if err != nil {
		return "", fmt.Errorf("plans: change the plan of %s: %w", workspaceID, err)
	}
	if tag.RowsAffected() != 1 {
		return "", ErrNoWorkspace
	}
	return w.Plan, nil
}

// count is n of noun in words: "1 agent", "3 agents", "1 person", "5 people".
func count(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if noun == "person" {
		return Thousands(n) + " people"
	}
	return Thousands(n) + " " + noun + "s"
}
