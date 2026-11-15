package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
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

// Tiers are the room limits' own four steps, named as the 5 Oct plans were. A plan takes the tier at its step on its
// ladder (B61.57): Solo and Studio free's, Crew and Company team's, Fleet and Group business's, Custom enterprise's.
var Tiers = []string{"free", "team", "business", "enterprise"}

// DefaultLimits are the proposal in B32.29.
func DefaultLimits() map[string]Limits {
	return map[string]Limits{
		"free":       {PublicRooms: 3, PrivateRooms: 0, MembersPerRoom: 50, AgentsPerRoom: 10, RoomBudgetMaxUSD: 100},
		"team":       {PublicRooms: 25, PrivateRooms: 10, MembersPerRoom: 250, AgentsPerRoom: 50, RoomBudgetMaxUSD: 5000},
		"business":   {PublicRooms: -1, PrivateRooms: 100, MembersPerRoom: 1000, AgentsPerRoom: 250, RoomBudgetMaxUSD: 50000},
		"enterprise": {PublicRooms: -1, PrivateRooms: -1, MembersPerRoom: -1, AgentsPerRoom: -1, RoomBudgetMaxUSD: -1},
	}
}

// LoadLimits reads rooms_plan_limits from getenv: unset is DefaultLimits; set, it names every one of Tiers.
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
			return nil, fmt.Errorf("rooms: %s names %q; the plans are %s", LimitsEnv, name, strings.Join(Tiers, ", "))
		}
	}
	for _, name := range Tiers {
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
	for _, p := range Tiers {
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

// limitsPlanOf is the tier whose room limits a workspace on plan takes: the one at the plan's step on its ladder, and
// free's for a plan that is none.
func limitsPlanOf(plan string) string {
	if i := plans.Step(plan); i >= 0 {
		return Tiers[i]
	}
	return Tiers[0]
}

// planAt is the plan at tier's step on plan's own ladder, the one a refusal names as allowing more; "" past its end.
func planAt(plan, tier string) string {
	ladder := plans.BusinessLadder
	if slices.Contains(plans.PersonalLadder, plan) {
		ladder = plans.PersonalLadder
	}
	if i := slices.Index(Tiers, tier); i >= 0 && i < len(ladder) {
		return ladder[i]
	}
	return ""
}

// PlanLimits is a workspace's plan and the room limits it has.
type PlanLimits struct {
	Plan    string `json:"plan"`     // as plans.PlanOf answers it
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
	past, tier := false, ""
	for _, name := range Tiers {
		if n := s.limits[name].count(limit); past && (n == plans.Unlimited || n > ceiling) {
			e.Allows, tier = planAt(p.Plan, name), name
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
	switch n := s.limits[tier].count(limit); {
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
