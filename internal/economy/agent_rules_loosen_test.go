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
