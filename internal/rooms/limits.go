package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"

	"github.com/talyvor/lens/internal/plans"
)

// LimitsSetting is rooms_plan_limits (B32.29): how many rooms each plan opens and how large they grow. Every value
// is a proposal for Nicolai; -1 is unlimited.
const (
	LimitsSetting = "rooms_plan_limits"
	LimitsEnv     = "LENS_ROOMS_PLAN_LIMITS"
)

// Limits is what one plan's rooms may hold. The plan is the room owner's: a member's own plan never matters.
type Limits struct {
	PublicRooms      int64 `json:"public_rooms"`        // rooms a workspace owns that are public and not closed
	PrivateRooms     int64 `json:"private_rooms"`       // rooms a workspace owns that are private and not closed
	MembersPerRoom   int64 `json:"members_per_room"`    // the owner included
	AgentsPerRoom    int64 `json:"agents_per_room"`     // every member's agents together
	RoomBudgetMaxUSD int64 `json:"room_budget_max_usd"` // the room wallet's largest monthly limit (B32.32)
}

// DefaultLimits are the proposal in B32.29.
func DefaultLimits() map[string]Limits {
	return map[string]Limits{
		plans.Free:       {PublicRooms: 3, PrivateRooms: 0, MembersPerRoom: 50, AgentsPerRoom: 10, RoomBudgetMaxUSD: 100},
		plans.Team:       {PublicRooms: 25, PrivateRooms: 10, MembersPerRoom: 250, AgentsPerRoom: 50, RoomBudgetMaxUSD: 5000},
		plans.Business:   {PublicRooms: -1, PrivateRooms: 100, MembersPerRoom: 1000, AgentsPerRoom: 250, RoomBudgetMaxUSD: 50000},
		plans.Enterprise: {PublicRooms: -1, PrivateRooms: -1, MembersPerRoom: -1, AgentsPerRoom: -1, RoomBudgetMaxUSD: -1},
	}
}

// LoadLimits reads rooms_plan_limits from getenv: unset is DefaultLimits; set, it names every plan in plans.Order.
func LoadLimits(getenv func(string) string) (map[string]Limits, error) {
	v := strings.TrimSpace(getenv(LimitsEnv))
	if v == "" {
		return DefaultLimits(), nil
	}
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()
	var m map[string]Limits
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("rooms: %s must be a JSON object of each plan's room limits, like the default in lens.env.example: %w", LimitsEnv, err)
	}
	for name := range m {
		if !limitsPlan(name) {
			return nil, fmt.Errorf("rooms: %s names %q; the plans are %s", LimitsEnv, name, strings.Join(plans.Order, ", "))
		}
	}
	for _, name := range plans.Order {
		l, ok := m[name]
		if !ok {
			return nil, fmt.Errorf("rooms: %s does not name the %s plan", LimitsEnv, name)
		}
		for _, n := range []int64{l.PublicRooms, l.PrivateRooms, l.MembersPerRoom, l.AgentsPerRoom, l.RoomBudgetMaxUSD} {
			if n < plans.Unlimited {
				return nil, fmt.Errorf("rooms: %s[%q] has %d; each limit is a count, or -1 for unlimited", LimitsEnv, name, n)
			}
		}
	}
	return m, nil
}

func limitsPlan(name string) bool {
	for _, p := range plans.Order {
		if p == name {
			return true
		}
	}
	return false
}

var (
	limitsOnce    sync.Once
	limitsCurrent map[string]Limits
	limitsErr     error
)

func loadLimits() { limitsCurrent, limitsErr = LoadLimits(os.Getenv) }

// CheckLimits reports a malformed LENS_ROOMS_PLAN_LIMITS; Lens will not start with one.
func CheckLimits() error {
	limitsOnce.Do(loadLimits)
	return limitsErr
}

// CurrentLimits is every plan's room limits as this process runs with them, read from its environment once.
func CurrentLimits() map[string]Limits {
	limitsOnce.Do(loadLimits)
	if limitsErr != nil {
		return DefaultLimits()
	}
	return maps.Clone(limitsCurrent)
}

// limitsPlanOf is the plan in plans.Order whose room limits a workspace on plan takes: plus, pro and max take
// team's, byok takes business's, and any plan the setting does not name takes free's.
func limitsPlanOf(plan string) string {
	switch plan {
	case "plus", "pro", "max":
		return plans.Team
	case "byok":
		return plans.Business
	case plans.Free, plans.Team, plans.Business, plans.Enterprise:
		return plan
	}
	return plans.Free
}

// PlanLimits is a workspace's plan and the room limits it has.
type PlanLimits struct {
	Plan    string `json:"plan"`     // as plans.PlanOf answers it — the live subscription's plan, or free
	LimitAs string `json:"limit_as"` // the plan whose rooms_plan_limits it takes
	Limits
}

// LimitsOf is ws's plan and its room limits.
func (s *Store) LimitsOf(ctx context.Context, q plans.Querier, ws string) (PlanLimits, error) {
	plan, err := plans.PlanOf(ctx, q, ws)
	if err != nil {
		return PlanLimits{}, err
	}
	as := limitsPlanOf(plan)
	return PlanLimits{Plan: plan, LimitAs: as, Limits: s.limits[as]}, nil
}

// ErrPlanLimit is the room owner's plan not allowing one more of something under rooms_plan_limits.
var ErrPlanLimit = errors.New("rooms: past the plan's rooms_plan_limits")

// PlanLimitError is one refusal under rooms_plan_limits. Its message says in words what the plan allows and what the
// plan that would allow more does; the setting is a field of its own (B55.29).
type PlanLimitError struct {
	Setting string `json:"setting"` // rooms_plan_limits
	Plan    string `json:"plan"`
	Limit   string `json:"limit"` // public_rooms, private_rooms, members_per_room, agents_per_room or room_budget_max_usd
	Max     int64  `json:"max"`
	Allows  string `json:"allows,omitempty"` // the next plan that allows more; "" when only a contract would
	Detail  string `json:"detail"`
}

func (e *PlanLimitError) Error() string { return e.Detail }

// Is makes every PlanLimitError an ErrPlanLimit.
func (e *PlanLimitError) Is(target error) bool { return target == ErrPlanLimit }

var limitNouns = map[string]string{
	"public_rooms": "public rooms", "private_rooms": "private rooms",
	"members_per_room": "members in a room", "agents_per_room": "agents in a room",
}

func (l Limits) count(limit string) int64 {
	switch limit {
	case "public_rooms":
		return l.PublicRooms
	case "private_rooms":
		return l.PrivateRooms
	case "members_per_room":
		return l.MembersPerRoom
	}
	return l.AgentsPerRoom
}

// refuse answers nil while used is below p's limit, and the refusal of one more once it is not. whose says whose
// plan it is: "your" for the actor's own ("Free allows 3 public rooms, and you have 3 open. Team allows 25."), or
// "The room owner’s" or "The new owner’s" for someone else's ("The room owner’s plan, Free, allows 50 members in a
// room.").
func (s *Store) refuse(p PlanLimits, limit, whose string, used int64) error {
	ceiling := p.count(limit)
	if ceiling == plans.Unlimited || used < ceiling {
		return nil
	}
	e := &PlanLimitError{Setting: LimitsSetting, Plan: p.Plan, Limit: limit, Max: ceiling}
	past := false
	for _, name := range plans.Order {
		if n := s.limits[name].count(limit); past && (n == plans.Unlimited || n > ceiling) {
			e.Allows = name
			break
		}
		past = past || name == p.LimitAs
	}
	noun := limitNouns[limit]
	allows := fmt.Sprintf("allows %s %s", plans.Thousands(ceiling), noun)
	if ceiling == 0 {
		allows = "allows no " + noun
	}
	if whose != "your" {
		e.Detail = fmt.Sprintf("%s plan, %s, %s.", whose, plans.Name(p.Plan), allows)
		return e
	}
	e.Detail = fmt.Sprintf("%s %s", plans.Name(p.Plan), allows)
	if ceiling > 0 {
		e.Detail += fmt.Sprintf(", and you have %s open", plans.Thousands(used))
	}
	switch n := s.limits[e.Allows].count(limit); {
	case e.Allows == "":
		e.Detail += ". A contract with Talyvor sets more."
	case n == plans.Unlimited:
		e.Detail += fmt.Sprintf(". %s allows unlimited %s.", plans.Name(e.Allows), noun)
	default:
		e.Detail += fmt.Sprintf(". %s allows %s.", plans.Name(e.Allows), plans.Thousands(n))
	}
	return e
}

// Usage is ws's plan, its room limits and the rooms it has open under them.
type Usage struct {
	PlanLimits
	PublicRoomsOpen  int64 `json:"public_rooms_open"`
	PrivateRoomsOpen int64 `json:"private_rooms_open"`
}

// Usage answers ws's plan, its room limits and how many of its rooms count against them.
func (s *Store) Usage(ctx context.Context, ws string) (Usage, error) {
	var u Usage
	var err error
	if u.PlanLimits, err = s.LimitsOf(ctx, s.pool, ws); err != nil {
		return u, fmt.Errorf("rooms: limits: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE visibility = 'public'), count(*) FILTER (WHERE visibility = 'private')
		FROM rooms WHERE owner_workspace_id = $1 AND status <> 'closed'`, ws).Scan(&u.PublicRoomsOpen, &u.PrivateRoomsOpen); err != nil {
		return u, fmt.Errorf("rooms: limits: %w", err)
	}
	return u, nil
}
