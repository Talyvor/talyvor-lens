package plans

import (
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// envExample is the commented value lens.env.example gives setting.
func envExample(t *testing.T, setting string) string {
	t.Helper()
	raw, err := os.ReadFile("../../lens.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "# "+setting+"="); ok {
			return v
		}
	}
	t.Fatalf("lens.env.example gives no %s", setting)
	return ""
}

// B61.57 — the values lens.env.example gives LENS_PLAN_GATES and LENS_PLAN_PRICES_MINOR are Nicolai's, the defaults;
// the defaults are his figures for both ladders; a malformed one, or one naming the 5 Oct plans, is refused.
func TestLoad_TheEnvExampleIsNicolaisValues(t *testing.T) {
	example := envExample(t, Setting)
	got, err := Load(func(name string) string { return map[string]string{Setting: example}[name] })
	if err != nil || !reflect.DeepEqual(got, Defaults()) {
		t.Fatalf("lens.env.example's %s loads as %+v, %v; want the defaults %+v", Setting, got, err, Defaults())
	}
	want := map[string]Gates{
		PersonalSolo:    {Agents: 3, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		PersonalCrew:    {Agents: 10, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		PersonalFleet:   {Agents: 25, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		BusinessStudio:  {Agents: 3, Seats: 1, OwnKeys: OwnKeysNone, LiveMoney: true},
		BusinessCompany: {Agents: 25, Seats: 5, OwnKeys: OwnKeysAddOn, LiveMoney: true, SlackTeamsApprovals: true},
		BusinessGroup:   {Agents: -1, Seats: 25, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true},
		BusinessCustom:  {Agents: -1, Seats: -1, OwnKeys: OwnKeysIncluded, LiveMoney: true, SlackTeamsApprovals: true, SSO: true, AuditExport: true, Edge: true},
	}
	if !reflect.DeepEqual(Defaults(), want) {
		t.Fatalf("the defaults are %+v, want Nicolai's %+v", Defaults(), want)
	}
	for name, v := range map[string]string{
		"not JSON":       `{`,
		"a plan short":   `{"personal_solo":{"agents":3,"seats":1,"own_provider_keys":"none"}}`,
		"an odd plan":    strings.Replace(example, `"personal_solo"`, `"personal_gratis"`, 1),
		"3 of the 5 Oct": `{"free":{"agents":3,"seats":1,"own_provider_keys":"none"},"team":{"agents":25,"seats":5,"own_provider_keys":"add_on"},"business":{"agents":-1,"seats":25,"own_provider_keys":"included"}}`,
		"agents -2":      strings.Replace(example, `"agents":3`, `"agents":-2`, 1),
		"own keys typo":  strings.Replace(example, `"own_provider_keys":"none"`, `"own_provider_keys":"no"`, 1),
		"a typo'd gate":  strings.Replace(example, `"sso":false`, `"ssso":false`, 1),
	} {
		if _, err := Load(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), Setting) {
			t.Errorf("%s: Load = %v, want refused naming %s", name, err, Setting)
		}
	}

	prices := envExample(t, PricesSetting)
	gotPrices, err := LoadPrices(func(name string) string { return map[string]string{PricesSetting: prices}[name] })
	if err != nil || !reflect.DeepEqual(gotPrices, DefaultPrices()) {
		t.Fatalf("lens.env.example's %s loads as %v, %v; want the defaults %v", PricesSetting, gotPrices, err, DefaultPrices())
	}
	wantPrices := map[string]map[string]int64{
		PersonalSolo:    {"GBP": 0, "EUR": 0, "USD": 0},
		PersonalCrew:    {"GBP": 399, "EUR": 399, "USD": 399},
		PersonalFleet:   {"GBP": 799, "EUR": 899, "USD": 999},
		BusinessStudio:  {"GBP": 1000, "EUR": 1000, "USD": 1000},
		BusinessCompany: {"GBP": 3500, "EUR": 3500, "USD": 3500},
		BusinessGroup:   {"GBP": 12500, "EUR": 12500, "USD": 12500},
	}
	if !reflect.DeepEqual(DefaultPrices(), wantPrices) {
		t.Fatalf("the default prices are %v, want Nicolai's %v", DefaultPrices(), wantPrices)
	}
	for name, v := range map[string]string{
		"not JSON":         `{`,
		"Custom priced":    strings.Replace(prices, `}}`, `},"business_custom":{"GBP":1,"EUR":1,"USD":1}}`, 1),
		"a currency short": strings.Replace(prices, `"GBP":399,`, ``, 1),
		"a negative price": strings.Replace(prices, `"GBP":399`, `"GBP":-1`, 1),
		"an odd currency":  strings.Replace(prices, `"GBP":399`, `"JPY":399`, 1),
	} {
		if _, err := LoadPrices(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), PricesSetting) {
			t.Errorf("%s: LoadPrices = %v, want refused naming %s", name, err, PricesSetting)
		}
	}
	if n, cur, ok := PriceIn(DefaultPrices(), PersonalFleet, "USDC"); !ok || n != 999 || cur != "USD" {
		t.Fatalf("Fleet for a stablecoin account = %d %s %v, want the USD figure, 999", n, cur, ok)
	}
	if _, _, ok := PriceIn(DefaultPrices(), BusinessCustom, "GBP"); ok || !Paid(DefaultPrices(), BusinessCustom) || Paid(DefaultPrices(), PersonalSolo) {
		t.Fatal("Custom has a price, or is free, or Solo is paid; want Custom by contract and Solo free")
	}
}

// B61.57 — each 5 Oct plan's successor on each ladder, as the migration and the testers' route read it.
func TestSuccessor_EveryOldPlan(t *testing.T) {
	for old, want := range map[string][2]string{
		"free": {PersonalSolo, BusinessStudio}, "plus": {PersonalCrew, BusinessCompany}, "pro": {PersonalCrew, BusinessCompany},
		"max": {PersonalCrew, BusinessCompany}, "team": {PersonalCrew, BusinessCompany}, "byok": {PersonalCrew, BusinessCompany},
		"business": {PersonalFleet, BusinessGroup}, "enterprise": {PersonalFleet, BusinessCustom}, "gratis": {"", ""},
	} {
		if p, b := Successor(AccountPersonal, old), Successor(AccountBusiness, old); p != want[0] || b != want[1] {
			t.Errorf("%s's successors = %q, %q; want %q, %q", old, p, b, want[0], want[1])
		}
	}
}

// B55.29, B61.57 — each gate's refusal is a plain sentence with the plan's name and the next plan's allowance on its
// own ladder; the setting is its own field and never in the words; past a business's last plan a contract sets more,
// and past a person's nothing does.
func TestRefusals_PlainWords(t *testing.T) {
	gates := Defaults()
	on := func(plan string) Workspace {
		kind := AccountBusiness
		if slices.Contains(PersonalLadder, plan) {
			kind = AccountPersonal
		}
		return Workspace{Plan: plan, AccountType: kind, GatedAs: plan, Gates: gates[plan]}
	}
	one := maps.Clone(gates)
	one[BusinessStudio] = Gates{Agents: 1, Seats: 1, OwnKeys: OwnKeysNone}
	contract := on(BusinessCustom)
	contract.Agents = 100
	for name, c := range map[string]struct {
		r    *Refusal
		want string
	}{
		"solo's agents":       {on(PersonalSolo).RefuseCount(gates, "agents", 3), "Solo allows 3 agents, and all 3 are in use. Crew allows 10."},
		"crew's agents":       {on(PersonalCrew).RefuseCount(gates, "agents", 10), "Crew allows 10 agents, and all 10 are in use. Fleet allows 25."},
		"fleet's agents":      {on(PersonalFleet).RefuseCount(gates, "agents", 25), "Fleet allows 25 agents, and all 25 are in use."},
		"studio's agents":     {on(BusinessStudio).RefuseCount(gates, "agents", 3), "Studio allows 3 agents, and all 3 are in use. Company allows 25."},
		"one agent":           {on(BusinessStudio).RefuseCount(one, "agents", 1), "Studio allows 1 agent, and it is in use. Company allows 25."},
		"studio's seat":       {on(BusinessStudio).RefuseCount(gates, "seats", 1), "Studio allows 1 seat, and it is taken. Company allows 5."},
		"company's seats":     {on(BusinessCompany).RefuseCount(gates, "seats", 5), "Company allows 5 seats, and all 5 are taken. Group allows 25."},
		"company's agents":    {on(BusinessCompany).RefuseCount(gates, "agents", 25), "Company allows 25 agents, and all 25 are in use. Group allows unlimited agents."},
		"a contract":          {contract.RefuseCount(gates, "agents", 100), "Custom allows 100 agents, and all 100 are in use. A contract with Talyvor sets more."},
		"studio's own keys":   {on(BusinessStudio).RefuseOwnKeys(gates), "Studio does not include your own provider keys. Company has them with the BYOK add-on, and Group includes them."},
		"company's own keys":  {on(BusinessCompany).RefuseOwnKeys(gates), "Company has your own provider keys only with the BYOK add-on. Group includes them."},
		"solo's own keys":     {on(PersonalSolo).RefuseOwnKeys(gates), "Solo does not include your own provider keys."},
		"studio's slack":      {on(BusinessStudio).RefuseSlackTeams(gates), "Studio does not include Slack and Teams approvals. Company includes them."},
		"crew's audit export": {on(PersonalCrew).RefuseAuditExport(gates), "Crew does not include audit export."},
		"company's sso":       {on(BusinessCompany).RefuseSSO(gates), "Company does not include single sign-on. Group includes it."},
	} {
		if c.r.Detail != c.want || strings.Contains(c.r.Detail, "LENS_") {
			t.Errorf("%s: %q, want %q", name, c.r.Detail, c.want)
		}
	}
}
