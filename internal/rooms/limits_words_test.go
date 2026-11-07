package rooms

import (
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/plans"
)

// B55.29 — a room limit and a room's budget are refused in a plain sentence with the plan's name and the next plan's
// allowance, never the setting's name.
func TestRoomRefusals_PlainWords(t *testing.T) {
	s := &Store{limits: DefaultLimits()}
	on := func(plan, limitAs string) PlanLimits {
		return PlanLimits{Plan: plan, LimitAs: limitAs, Limits: s.limits[limitAs]}
	}
	free := on(plans.Free, plans.Free)
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"public rooms":     {s.refuse(free, "public_rooms", "your", 3), "Free allows 3 public rooms, and you have 3 open. Team allows 25."},
		"private rooms":    {s.refuse(free, "private_rooms", "your", 0), "Free allows no private rooms. Team allows 10."},
		"business public":  {s.refuse(on(plans.Team, plans.Team), "public_rooms", "your", 25), "Team allows 25 public rooms, and you have 25 open. Business allows unlimited public rooms."},
		"a member joining": {s.refuse(free, "members_per_room", "The room owner’s", 50), "The room owner’s plan, Free, allows 50 members in a room."},
		"an agent":         {s.refuse(on("max", plans.Team), "agents_per_room", "The room owner’s", 50), "The room owner’s plan, Max, allows 50 agents in a room."},
		"a takeover":       {s.refuse(free, "public_rooms", "The new owner’s", 3), "The new owner’s plan, Free, allows 3 public rooms."},
		"over the budget":  {s.judgeBudget(free, 101*usdULXC), "Free allows a room a budget of up to $100 (1,000 LXC) a month. Team allows $5,000."},
		"no budget":        {s.judgeBudget(free, 0), "This room has no budget yet. Its owner sets one in Room settings: on Free, up to $100 (1,000 LXC) a month."},
		"business budget":  {s.judgeBudget(on(plans.Business, plans.Business), 50_001*usdULXC), "Business allows a room a budget of up to $50,000 (500,000 LXC) a month. Enterprise allows any budget."},
	} {
		if c.err == nil || c.err.Error() != c.want || strings.Contains(c.err.Error(), LimitsSetting) || strings.Contains(c.err.Error(), "LENS_") {
			t.Errorf("%s: %v, want %q", name, c.err, c.want)
		}
	}
}
