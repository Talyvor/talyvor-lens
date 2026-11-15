package rooms

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// B32.29 — the value lens.env.example gives LENS_ROOMS_PLAN_LIMITS is the defaults; a malformed one is refused; plus,
// pro and max take team's limits, byok business's and any other plan free's.
func TestLoadLimits_TheEnvExampleIsTheDefaults(t *testing.T) {
	raw, err := os.ReadFile("../../lens.env.example")
	if err != nil {
		t.Fatal(err)
	}
	var example string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "# "+LimitsEnv+"="); ok {
			example = v
		}
	}
	got, err := LoadLimits(func(name string) string { return map[string]string{LimitsEnv: example}[name] })
	if err != nil || !reflect.DeepEqual(got, DefaultLimits()) {
		t.Fatalf("lens.env.example's %s loads as %+v, %v; want the defaults %+v", LimitsEnv, got, err, DefaultLimits())
	}
	for name, v := range map[string]string{
		"not JSON":       `{`,
		"a plan short":   `{"free":{"public_rooms":3,"private_rooms":0,"members_per_room":50,"agents_per_room":10,"room_budget_max_usd":100}}`,
		"an odd plan":    strings.Replace(example, `"free"`, `"gratis"`, 1),
		"a limit of -2":  strings.Replace(example, `"public_rooms":3`, `"public_rooms":-2`, 1),
		"a typo'd limit": strings.Replace(example, `"agents_per_room":10`, `"agent_per_room":10`, 1),
	} {
		if _, err := LoadLimits(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), LimitsEnv) {
			t.Errorf("%s: LoadLimits = %v, want refused naming %s", name, err, LimitsEnv)
		}
	}
	for plan, want := range map[string]string{"personal_solo": "free", "personal_crew": "team", "personal_fleet": "business",
		"business_studio": "free", "business_company": "team", "business_group": "business", "business_custom": "enterprise",
		"legacy": "free", "": "free"} {
		if got := limitsPlanOf(plan); got != want {
			t.Errorf("limitsPlanOf(%q) = %q, want %q", plan, got, want)
		}
	}
}
