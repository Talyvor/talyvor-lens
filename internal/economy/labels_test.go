package economy

import (
	"testing"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B56.7 — a label says Live only when the money can be real, and changes only with the facts.
func TestLabel_LiveOnlyWhenTheMoneyCanBeReal(t *testing.T) {
	spend, _ := CapabilityByKey("spend_on_talyvor")
	fx, _ := CapabilityByKey(CapabilityFX)
	accounts, _ := CapabilityByKey(CapabilityCurrencyAccounts)
	none, _ := CapabilityByKey("rules_approvals_statements_pots")
	fxRail := partnerRail(partners.ServiceFX)
	accountRails := []string{partnerRail(partners.ServiceAccount), moneyRail(money.CapAccounts)}
	live := func(rails ...string) map[string]bool {
		m := map[string]bool{}
		for _, r := range rails {
			m[r] = true
		}
		return m
	}
	cleared := func(keys ...string) map[string]bool { return live(keys...) }
	for _, tc := range []struct {
		name string
		c    Capability
		f    LabelFacts
		want string
	}{
		{"GREEN, card payments test", spend, LabelFacts{}, LabelSandbox},
		{"GREEN, card payments live", spend, LabelFacts{Live: live(RailCards)}, LabelLive},
		{"RED, live rail, no clearance", fx, LabelFacts{Live: live(fxRail)}, LabelSandbox},
		{"RED, cleared, test rail", fx, LabelFacts{Cleared: cleared(CapabilityFX)}, LabelSandbox},
		{"RED, cleared, live rail", fx, LabelFacts{Cleared: cleared(CapabilityFX), Live: live(fxRail)}, LabelLive},
		{"RED, cleared, one of two rails live", accounts, LabelFacts{Cleared: cleared(CapabilityCurrencyAccounts), Live: live(accountRails[0])}, LabelSandbox},
		{"RED, cleared, every rail live", accounts, LabelFacts{Cleared: cleared(CapabilityCurrencyAccounts), Live: live(accountRails...)}, LabelLive},
		{"no money, no label", none, LabelFacts{Live: live(RailCards)}, ""},
	} {
		if got := Label(tc.c, tc.f); got != tc.want {
			t.Errorf("%s: Label = %q, want %q", tc.name, got, tc.want)
		}
	}

	// For a workspace: a B30 capability on a plan with no live money, and anything in a synthetic workspace, is Sandbox.
	for _, tc := range []struct {
		name                 string
		c                    Capability
		liveMoney, synthetic bool
		want                 string
	}{
		{"B30 on Free", fx, false, false, LabelSandbox},
		{"B30 on a live-money plan", fx, true, false, LabelLive},
		{"GREEN on Free", spend, false, false, LabelLive},
		{"synthetic", spend, true, true, LabelSandbox},
	} {
		if got := workspaceLabel(LabelLive, tc.c, tc.liveMoney, tc.synthetic); got != tc.want {
			t.Errorf("%s: workspaceLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// B56.7 — every capability names its rails, so a new one cannot go unlabelled by accident.
func TestLabel_EveryCapabilityNamesItsRails(t *testing.T) {
	for _, c := range Capabilities {
		if _, ok := capabilityRails[c.Key]; !ok {
			t.Errorf("%s has no entry in capabilityRails: name the rails its money moves on", c.Key)
		}
	}
	if len(capabilityRails) != len(Capabilities) {
		t.Errorf("capabilityRails has %d entries for %d capabilities", len(capabilityRails), len(Capabilities))
	}
}
