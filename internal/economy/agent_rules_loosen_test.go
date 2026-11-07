package economy

import "testing"

// B55.13 — which changes loosen an agent's rules beyond its limits: payees, hours, time zone, switches.
func TestB5513_WhatLoosensTheRules(t *testing.T) {
	yes, no := true, false
	was := AgentRules{AllowedPayees: []string{"agt_a"}, BlockedPayees: []string{"agt_x"}, ActiveFrom: "22:00", ActiveUntil: "06:00",
		Timezone: "Europe/London", PauseOnUnusualSpend: true, MaySubscribe: &no}
	for _, c := range []struct {
		name   string
		change func(r *AgentRules)
		looser bool
	}{
		{"a payee added to the allowed list", func(r *AgentRules) { r.AllowedPayees = []string{"agt_a", "agt_b"} }, true},
		{"the allowed list emptied", func(r *AgentRules) { r.AllowedPayees = nil }, true},
		{"a payee blocked", func(r *AgentRules) { r.BlockedPayees = []string{"agt_x", "agt_y"} }, false},
		{"a blocked payee removed", func(r *AgentRules) { r.BlockedPayees = nil }, true},
		{"hours narrowed across midnight", func(r *AgentRules) { r.ActiveFrom, r.ActiveUntil = "23:00", "05:00" }, false},
		{"hours widened", func(r *AgentRules) { r.ActiveFrom = "21:00" }, true},
		{"hours cleared", func(r *AgentRules) { r.ActiveFrom, r.ActiveUntil = "", "" }, true},
		{"the time zone changed", func(r *AgentRules) { r.Timezone = "America/New_York" }, true},
		{"pause on unusual spend turned off", func(r *AgentRules) { r.PauseOnUnusualSpend = false }, true},
		{"may subscribe turned on", func(r *AgentRules) { r.MaySubscribe = &yes }, true},
		{"a payee allowance added", func(r *AgentRules) { r.PayeeAllowances = map[string]PayeeAllowance{"agt_a": {ULXC: 1, Per: "month"}} }, true},
	} {
		now := was
		c.change(&now)
		looser := false
		for _, ch := range rulesChanges(was, now) {
			looser = looser || ch.looser
		}
		if looser != c.looser {
			t.Errorf("%s: looser = %v, want %v (%v)", c.name, looser, c.looser, rulesChanges(was, now))
		}
	}
}

// B54.29 — a payee allowance loosens the rules when it is new, raised, or moved from a month to a day; moving the same
// amount from a day to a month, or setting an allowance of 0 (none), does not.
func TestB5429_WhichAllowanceChangesLoosen(t *testing.T) {
	was := AgentRules{PayeeAllowances: map[string]PayeeAllowance{"agt_a": {ULXC: 5, Per: "day"}, "agt_m": {ULXC: 5, Per: "month"}}}
	for _, c := range []struct {
		name  string
		allow map[string]PayeeAllowance
		want  bool
	}{
		{"the same amount a month instead of a day", map[string]PayeeAllowance{"agt_a": {ULXC: 5, Per: "month"}}, false},
		{"an allowance of 0 for a new payee", map[string]PayeeAllowance{"agt_b": {ULXC: 0, Per: "day"}}, false},
		{"an allowance lowered", map[string]PayeeAllowance{"agt_a": {ULXC: 4, Per: "day"}}, false},
		{"an allowance raised", map[string]PayeeAllowance{"agt_a": {ULXC: 6, Per: "day"}}, true},
		{"the same amount a day instead of a month", map[string]PayeeAllowance{"agt_m": {ULXC: 5, Per: "day"}}, true},
		{"a new payee allowed", map[string]PayeeAllowance{"agt_b": {ULXC: 1, Per: "month"}}, true},
	} {
		now := AgentRules{PayeeAllowances: map[string]PayeeAllowance{"agt_a": was.PayeeAllowances["agt_a"], "agt_m": was.PayeeAllowances["agt_m"]}}
		for id, a := range c.allow {
			now.PayeeAllowances[id] = a
		}
		looser := false
		for _, ch := range rulesChanges(was, now) {
			looser = looser || ch.looser
		}
		if looser != c.want {
			t.Errorf("%s: looser = %v, want %v (%v)", c.name, looser, c.want, rulesChanges(was, now))
		}
	}
}
