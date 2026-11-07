package plans

import (
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
)

// B32.12 — the value lens.env.example gives LENS_PLAN_GATES is Nicolai's, the defaults; a malformed one is refused.
func TestLoad_TheEnvExampleIsNicolaisValues(t *testing.T) {
	raw, err := os.ReadFile("../../lens.env.example")
	if err != nil {
		t.Fatal(err)
	}
	var example string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "# "+Setting+"="); ok {
			example = v
		}
	}
	got, err := Load(func(name string) string { return map[string]string{Setting: example}[name] })
	if err != nil || !reflect.DeepEqual(got, Defaults()) {
		t.Fatalf("lens.env.example's %s loads as %+v, %v; want the defaults %+v", Setting, got, err, Defaults())
	}
	want := map[string]Gates{
		Free:       {Agents: 3, Seats: 1, OwnKeys: OwnKeysNone},
		Team:       {Agents: 25, Seats: 5, OwnKeys: OwnKeysAddOn, LiveMoney: true, SlackTeamsApprovals: true},
		Business:   {Agents: -1, Seats: 25, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true},
		Enterprise: {Agents: -1, Seats: -1, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true, Edge: true},
	}
	if !reflect.DeepEqual(Defaults(), want) {
		t.Fatalf("the defaults are %+v, want Nicolai's %+v", Defaults(), want)
	}
	for name, v := range map[string]string{
		"not JSON":      `{`,
		"a plan short":  `{"free":{"agents":3,"seats":1,"own_provider_keys":"none"}}`,
		"an odd plan":   strings.Replace(example, `"free"`, `"gratis"`, 1),
		"agents -2":     strings.Replace(example, `"agents":3`, `"agents":-2`, 1),
		"own keys typo": strings.Replace(example, `"own_provider_keys":"none"`, `"own_provider_keys":"no"`, 1),
		"a typo'd gate": strings.Replace(example, `"sso":false`, `"ssso":false`, 1),
	} {
		if _, err := Load(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), Setting) {
			t.Errorf("%s: Load = %v, want refused naming %s", name, err, Setting)
		}
	}
}

// B55.29 — each gate's refusal is a plain sentence with the plan's name and the next plan's allowance; the setting
// is its own field and never in the words; past the last plan a contract sets more.
func TestRefusals_PlainWords(t *testing.T) {
	gates := Defaults()
	on := func(plan, gatedAs string) Workspace {
		return Workspace{Plan: plan, GatedAs: gatedAs, Gates: gates[gatedAs]}
	}
	one := maps.Clone(gates)
	one[Free] = Gates{Agents: 1, Seats: 1, OwnKeys: OwnKeysNone}
	contract := on(Enterprise, Enterprise)
	contract.Agents = 100
	for name, c := range map[string]struct {
		r    *Refusal
		want string
	}{
		"free's agents":     {on(Free, Free).RefuseCount(gates, "agents", 3), "Free allows 3 agents, and all 3 are in use. Team allows 25."},
		"one agent":         {on(Free, Free).RefuseCount(one, "agents", 1), "Free allows 1 agent, and it is in use. Team allows 25."},
		"free's seat":       {on(Free, Free).RefuseCount(gates, "seats", 1), "Free allows 1 seat, and it is taken. Team allows 5."},
		"team's seats":      {on(Team, Team).RefuseCount(gates, "seats", 5), "Team allows 5 seats, and all 5 are taken. Business allows 25."},
		"team's agents":     {on(Team, Team).RefuseCount(gates, "agents", 25), "Team allows 25 agents, and all 25 are in use. Business allows unlimited agents."},
		"a personal plan":   {on("plus", Free).RefuseCount(gates, "agents", 3), "Plus allows 3 agents, and all 3 are in use. Team allows 25."},
		"a contract":        {contract.RefuseCount(gates, "agents", 100), "Enterprise allows 100 agents, and all 100 are in use. A contract with Talyvor sets more."},
		"free's own keys":   {on(Free, Free).RefuseOwnKeys(gates), "Free does not include your own provider keys. Team has them with the BYOK add-on, and Business includes them."},
		"team's own keys":   {on(Team, Team).RefuseOwnKeys(gates), "Team has your own provider keys only with the BYOK add-on. Business includes them."},
		"free's live money": {on(Free, Free).RefuseLiveMoney(gates, "Currency conversion"), "On Free, Currency conversion takes no real money. On Team it does, once it is cleared."},
		"pro's live money":  {on("pro", Free).RefuseLiveMoney(gates, "Escrow"), "On Pro, Escrow takes no real money. On Team it does, once it is cleared."},
	} {
		if c.r.Detail != c.want || c.r.Setting != Setting || strings.Contains(c.r.Detail, "LENS_") {
			t.Errorf("%s: %q (setting %q), want %q with setting %s", name, c.r.Detail, c.r.Setting, c.want, Setting)
		}
	}
}
