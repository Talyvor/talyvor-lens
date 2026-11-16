package economy

import "testing"

func b6140Looser(was, now AgentRules) bool {
	for _, c := range rulesChanges(was, now) {
		if c.looser {
			return true
		}
	}
	return false
}

// B61.40 — the conversion maximum and a payee's cap loosen when cleared, whether held in the rules' currency or in
// µLXC alone (euro rules from before B61.40), and tighten when set; an "always allow" loosens when raised in either
// form. So none is loosened without the owner's signature.
func TestB6140_ACapHeldInEitherFormLoosensWhenCleared(t *testing.T) {
	zero, fx, ulxc := int64(0), int64(5_000), int64(550_000_000)
	none := AgentRules{Currency: CurrencyEUR, FXDailyLimitMinor: &zero, FXDailyLimitULXC: &zero}
	for name, capped := range map[string]AgentRules{
		"conversion maximum in euros":      {Currency: CurrencyEUR, FXDailyLimitMinor: &fx, FXDailyLimitULXC: &ulxc},
		"conversion maximum in µLXC alone": {Currency: CurrencyEUR, FXDailyLimitMinor: &zero, FXDailyLimitULXC: &ulxc},
		"payee cap in euros":               {Currency: CurrencyEUR, PayeeDailyLimitsMinor: map[string]int64{"p": fx}, PayeeDailyLimitsULXC: map[string]int64{"p": ulxc}},
		"payee cap in µLXC alone":          {Currency: CurrencyEUR, PayeeDailyLimitsULXC: map[string]int64{"p": ulxc}},
	} {
		if !b6140Looser(capped, none) {
			t.Errorf("%s cleared: %+v, want it to loosen", name, rulesChanges(capped, none))
		}
		if b6140Looser(none, capped) {
			t.Errorf("%s set: %+v, want it to tighten", name, rulesChanges(none, capped))
		}
	}
	allow := func(a PayeeAllowance) AgentRules {
		return AgentRules{Currency: CurrencyEUR, PayeeAllowances: map[string]PayeeAllowance{"p": a}}
	}
	for name, step := range map[string][2]PayeeAllowance{
		"in µLXC alone, then in euros worth more": {{ULXC: ulxc, Per: "day"}, {Minor: fx, ULXC: ulxc + 1, Per: "day"}},
		"in euros, raised":                        {{Minor: fx, ULXC: ulxc, Per: "day"}, {Minor: fx + 1, ULXC: ulxc, Per: "day"}},
		"in euros, a month made a day":            {{Minor: fx, ULXC: ulxc, Per: "month"}, {Minor: fx, ULXC: ulxc, Per: "day"}},
	} {
		if !b6140Looser(allow(step[0]), allow(step[1])) {
			t.Errorf("always allow %s: %+v, want it to loosen", name, rulesChanges(allow(step[0]), allow(step[1])))
		}
	}
}
