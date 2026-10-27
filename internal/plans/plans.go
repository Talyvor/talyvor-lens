// Package plans is what each plan unlocks — Nicolai's decision of 5 Oct 2026 (B32.12): agents, seats, own
// provider keys and live money. One setting, LENS_PLAN_GATES, whose default in lens.env.example is his values;
// every feature a plan gates asks For which gates a workspace has, and a refusal names the setting, the plan and
// the plan that would allow it. A gate whose feature does not exist yet is enforced by the item that builds it.
package plans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Setting is the environment variable the gates are read from.
const Setting = "LENS_PLAN_GATES"

// Unlimited is a limit with no ceiling.
const Unlimited = -1

// The plans that carry gates, in the order a refusal looks for one that would allow it.
const (
	Free       = "free"
	Team       = "team"
	Business   = "business"
	Enterprise = "enterprise"
)

// Order is the plans from the smallest up.
var Order = []string{Free, Team, Business, Enterprise}

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
	// clearance; true takes each live as it is cleared.
	LiveMoney           bool `json:"live_money"`
	SlackTeamsApprovals bool `json:"slack_teams_approvals"`
	SSO                 bool `json:"sso"`
	AuditExport         bool `json:"audit_export"`
	Edge                bool `json:"edge"` // Talyvor Edge
}

// Defaults are Nicolai's values.
func Defaults() map[string]Gates {
	return map[string]Gates{
		Free:       {Agents: 3, Seats: 1, OwnKeys: OwnKeysNone},
		Team:       {Agents: 25, Seats: 5, OwnKeys: OwnKeysAddOn, LiveMoney: true, SlackTeamsApprovals: true},
		Business:   {Agents: Unlimited, Seats: 25, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true},
		Enterprise: {Agents: Unlimited, Seats: Unlimited, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true, Edge: true},
	}
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
	once    sync.Once
	current map[string]Gates
	loadErr error
)

func load() { current, loadErr = Load(os.Getenv) }

// Check reports a malformed LENS_PLAN_GATES; Lens will not start with one.
func Check() error {
	once.Do(load)
	return loadErr
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

// PlanOf answers which plan workspaceID is on: enterprise while the operator has it on a contract; else the
// plan its paying subscription bills (trialing, active or past_due — unpaid is Stripe having given up); else,
// for a synthetic workspace, the plan the testers put it on (B35.1); else free. A subscription whose Price is no
// plan's answers free too: there is no plan to charge or gate it by.
func PlanOf(ctx context.Context, db Querier, workspaceID string) (string, error) {
	w, err := Of(ctx, db, workspaceID)
	return w.Plan, err
}

// Workspace is a workspace's plan and the gates it has.
type Workspace struct {
	Plan      string `json:"plan"`        // as PlanOf answers it: plus, pro, max and byok included
	GatedAs   string `json:"gated_as"`    // the plan in Order whose gates it takes
	BYOKAddOn bool   `json:"byok_add_on"` // its subscription carries the BYOK Price
	Gates
}

// Of is workspaceID's plan and its gates under the gates this process runs with.
func Of(ctx context.Context, db Querier, workspaceID string) (Workspace, error) {
	return OfUnder(ctx, db, workspaceID, Current())
}

// OfUnder is workspaceID's plan and its gates under gates. plus, pro and max are personal chat plans and take
// free's; byok alone takes team's, with own keys; an Enterprise contract's own agents and seats, where it set
// them, replace enterprise's. A synthetic workspace takes its plan's gates but never live money, whatever its
// plan says (B35.1): its money is test money.
func OfUnder(ctx context.Context, db Querier, workspaceID string, gates map[string]Gates) (Workspace, error) {
	var w Workspace
	var agents, seats *int64
	var synthetic bool
	err := db.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT plan FROM workspace_contracts WHERE workspace_id = $1),
			(SELECT plan FROM subscriptions
			 WHERE workspace_id = $1 AND status IN ('trialing','active','past_due')),
			(SELECT synthetic_plan FROM workspaces WHERE id = $1),
			$2),
			EXISTS (SELECT 1 FROM subscriptions WHERE workspace_id = $1 AND byok
				AND status IN ('trialing','active','past_due')),
			(SELECT agents FROM workspace_contracts WHERE workspace_id = $1),
			(SELECT seats FROM workspace_contracts WHERE workspace_id = $1),
			COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID, Free).
		Scan(&w.Plan, &w.BYOKAddOn, &agents, &seats, &synthetic)
	if err != nil {
		return Workspace{}, fmt.Errorf("plans: the plan of %s: %w", workspaceID, err)
	}
	switch w.GatedAs = w.Plan; w.Plan {
	case "plus", "pro", "max":
		w.GatedAs = Free
	case "byok":
		w.GatedAs = Team
	}
	g, ok := gates[w.GatedAs]
	if !ok {
		w.GatedAs, g = Free, gates[Free]
	}
	if w.Plan == "byok" {
		g.OwnKeys = OwnKeysIncluded
	}
	if agents != nil {
		g.Agents = *agents
	}
	if seats != nil {
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

// Allowing is the first plan in Order, past the workspace's own, whose gates under gates satisfy ok; "" when none.
func (w Workspace) Allowing(gates map[string]Gates, ok func(Gates) bool) string {
	past := false
	for _, p := range Order {
		if past && ok(gates[p]) {
			return p
		}
		past = past || p == w.GatedAs
	}
	return ""
}

// Refusal is something the workspace's plan does not unlock. Its message names LENS_PLAN_GATES, the plan, and
// the plan that would allow it.
type Refusal struct {
	Plan   string `json:"plan"`
	Gate   string `json:"gate"`             // agents, seats, own_provider_keys or live_money
	Limit  *int64 `json:"limit,omitempty"`  // for agents and seats
	Allows string `json:"allows,omitempty"` // the plan that would allow it; "" when only a contract would
	Detail string `json:"detail"`
}

func (r *Refusal) Error() string { return r.Detail }

// Is makes every Refusal an ErrRefused.
func (r *Refusal) Is(target error) bool { return target == ErrRefused }

// ErrRefused is the plan not unlocking something.
var ErrRefused = errors.New("plans: the plan does not allow this")

// RefuseCount is the refusal of one more agent or seat than the workspace's plan allows.
func (w Workspace) RefuseCount(gates map[string]Gates, gate string, limit int64) *Refusal {
	noun := map[string]string{"agents": "agent", "seats": "seat"}[gate]
	allows := w.Allowing(gates, func(g Gates) bool {
		n := map[string]int64{"agents": g.Agents, "seats": g.Seats}[gate]
		return n == Unlimited || n > limit
	})
	detail := fmt.Sprintf("%s: the %s plan allows %d %s", Setting, w.Plan, limit, plural(noun, limit))
	switch {
	case allows != "" && gates[allows].countOf(gate) == Unlimited:
		detail += fmt.Sprintf(" — the %s plan allows unlimited %ss", allows, noun)
	case allows != "":
		n := gates[allows].countOf(gate)
		detail += fmt.Sprintf(" — the %s plan allows %d %s", allows, n, plural(noun, n))
	default:
		detail += " — a contract with Talyvor sets more"
	}
	return &Refusal{Plan: w.Plan, Gate: gate, Limit: &limit, Allows: allows, Detail: detail}
}

func (g Gates) countOf(gate string) int64 {
	if gate == "seats" {
		return g.Seats
	}
	return g.Agents
}

func plural(noun string, n int64) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// CheckSeats refuses members past the seats the workspace's plan allows; nil when they fit.
func (w Workspace) CheckSeats(members int64) error {
	if Within(members, w.Seats) {
		return nil
	}
	return w.RefuseCount(Current(), "seats", w.Seats)
}

// RefuseOwnKeys is the refusal of an own provider key on a plan that does not allow one.
func (w Workspace) RefuseOwnKeys(gates map[string]Gates) *Refusal {
	r := &Refusal{Plan: w.Plan, Gate: "own_provider_keys"}
	if w.OwnKeys == OwnKeysAddOn {
		r.Allows = w.Allowing(gates, func(g Gates) bool { return g.OwnKeys == OwnKeysIncluded })
		r.Detail = fmt.Sprintf("%s: the %s plan has own provider keys only with the BYOK add-on — add it", Setting, w.Plan)
		if r.Allows != "" {
			r.Detail += fmt.Sprintf(", or move to the %s plan, which includes them", r.Allows)
		}
		return r
	}
	r.Allows = w.Allowing(gates, func(g Gates) bool { return g.OwnKeys != OwnKeysNone })
	r.Detail = fmt.Sprintf("%s: the %s plan does not include own provider keys", Setting, w.Plan)
	switch {
	case r.Allows != "" && gates[r.Allows].OwnKeys == OwnKeysAddOn:
		r.Detail += fmt.Sprintf(" — the %s plan has them with the BYOK add-on", r.Allows)
	case r.Allows != "":
		r.Detail += fmt.Sprintf(" — the %s plan includes them", r.Allows)
	}
	return r
}

// RefuseLiveMoney is the refusal of live money for a capability the workspace's plan keeps on test money.
func (w Workspace) RefuseLiveMoney(gates map[string]Gates, capability string) *Refusal {
	r := &Refusal{Plan: w.Plan, Gate: "live_money", Allows: w.Allowing(gates, func(g Gates) bool { return g.LiveMoney })}
	r.Detail = fmt.Sprintf("%s: on the %s plan, %s takes no real money, even with a clearance, and this would use real money",
		Setting, w.Plan, capability)
	if r.Allows != "" {
		r.Detail += fmt.Sprintf(" — on the %s plan it is live once cleared", r.Allows)
	}
	return r
}
