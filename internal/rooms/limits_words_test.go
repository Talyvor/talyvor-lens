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
	studio := on(plans.BusinessStudio, "free")
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"public rooms":     {s.refuse(studio, "public_rooms", "your", 3), "Studio allows 3 public rooms, and you have 3 open. Company allows 25."},
		"private rooms":    {s.refuse(studio, "private_rooms", "your", 0), "Studio allows no private rooms. Company allows 10."},
		"company public":   {s.refuse(on(plans.BusinessCompany, "team"), "public_rooms", "your", 25), "Company allows 25 public rooms, and you have 25 open. Group allows unlimited public rooms."},
		"solo's public":    {s.refuse(on(plans.PersonalSolo, "free"), "public_rooms", "your", 3), "Solo allows 3 public rooms, and you have 3 open. Crew allows 25."},
		"a member joining": {s.refuse(studio, "members_per_room", "The room owner’s", 50), "The room owner’s plan, Studio, allows 50 members in a room."},
		"an agent":         {s.refuse(on(plans.PersonalCrew, "team"), "agents_per_room", "The room owner’s", 50), "The room owner’s plan, Crew, allows 50 agents in a room."},
		"a takeover":       {s.refuse(studio, "public_rooms", "The new owner’s", 3), "The new owner’s plan, Studio, allows 3 public rooms."},
		"over the budget":  {s.judgeBudget(studio, 101*usdULXC), "Studio allows a room a budget of up to $100 a month. Company allows $5,000."},
		"no budget":        {s.judgeBudget(studio, 0), "This room has no budget yet. Its owner sets one in Room settings: on Studio, up to $100 a month."},
		"group budget":     {s.judgeBudget(on(plans.BusinessGroup, "business"), 50_001*usdULXC), "Group allows a room a budget of up to $50,000 a month. Custom allows any budget."},
	} {
		if c.err == nil || c.err.Error() != c.want || strings.Contains(c.err.Error(), LimitsSetting) || strings.Contains(c.err.Error(), "LENS_") {
			t.Errorf("%s: %v, want %q", name, c.err, c.want)
		}
	}
}
