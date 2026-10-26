package economy

// agent_rule_templates.go — B28.29: rules a new agent can start from, in one click or one call. Each
// template names every rule Lens holds — a zero (no limit), an empty list and an empty map included, and the
// time zone named — so applying one replaces the agent's rules whole: SetAgentRules keeps a rule its input
// leaves nil, and a template leaves none nil, so the agent then holds exactly the template. The amounts are
// the ones Agent Wallets has offered since B28.305.

// RuleTemplate is a named set of rules an agent can start from.
type RuleTemplate struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Summary string     `json:"summary"` // one sentence on the agent it suits and what its rules do
	Rules   AgentRules `json:"rules"`
}

// ruleTemplate is the template's rules with every rule named: those it sets, and no rule for the rest.
func ruleTemplate(id, name, summary string, set func(*AgentRules)) RuleTemplate {
	var hourly, weekly, rpm, commitment int64
	var maySubscribe bool
	r := AgentRules{HourlyLimitULXC: &hourly, WeeklyLimitULXC: &weekly, RequestsPerMinute: &rpm,
		AllowedModels: []string{}, AllowedProviders: []string{}, AllowedListings: []string{},
		AllowedPayees: []string{}, BlockedPayees: []string{}, ModelDailyLimitsULXC: map[string]int64{},
		PayeeDailyLimitsULXC: map[string]int64{}, Timezone: "UTC",
		MaxCommitmentULXC: &commitment, AllowedLicences: []string{}, MaySubscribe: &maySubscribe,
		PayeeMonthlyLimitsULXC: map[string]int64{}, PayeeAllowances: map[string]PayeeAllowance{}}
	set(&r)
	return RuleTemplate{ID: id, Name: name, Summary: summary, Rules: r}
}

const oneLXC = 1_000_000

// RuleTemplates are the templates, a fresh copy on each call: a caller may change the one it is given.
func RuleTemplates() []RuleTemplate {
	return []RuleTemplate{
		ruleTemplate("support-bot", "Support bot",
			"Answers customers all day in many small requests: a tight cap on each, an hourly cap that stops a loop, and a pause on unusual spend.",
			func(r *AgentRules) {
				r.MaxPerRequestULXC, *r.HourlyLimitULXC, r.DailyLimitULXC = oneLXC/2, 20*oneLXC, 200*oneLXC
				r.MonthlyLimitULXC, r.ApprovalAboveULXC, r.PauseOnUnusualSpend = 4000*oneLXC, 5*oneLXC, true
			}),
		ruleTemplate("researcher", "Researcher",
			"Reads and writes long documents in bursts: room for large requests, a weekly budget, and a person asked above 20 LXC.",
			func(r *AgentRules) {
				r.MaxPerRequestULXC, r.DailyLimitULXC, *r.WeeklyLimitULXC = 10*oneLXC, 300*oneLXC, 1000*oneLXC
				r.MonthlyLimitULXC, r.ApprovalAboveULXC = 3000*oneLXC, 20*oneLXC
			}),
		ruleTemplate("coder", "Coder",
			"Works in long sessions on capable models: an hourly cap for a runaway loop, a daily budget, and a pause on unusual spend.",
			func(r *AgentRules) {
				r.MaxPerRequestULXC, *r.HourlyLimitULXC, r.DailyLimitULXC = 5*oneLXC, 50*oneLXC, 400*oneLXC
				r.MonthlyLimitULXC, r.ApprovalAboveULXC, r.PauseOnUnusualSpend = 6000*oneLXC, 10*oneLXC, true
			}),
	}
}

// RuleTemplateByID is the template with that id; false when there is none.
func RuleTemplateByID(id string) (RuleTemplate, bool) {
	for _, t := range RuleTemplates() {
		if t.ID == id {
			return t, true
		}
	}
	return RuleTemplate{}, false
}
