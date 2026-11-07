package economy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// B52.16 — the gym passes a playbook whose wording carries only the engine's figure, and fails one whose wording
// names a rival offer the owner did not allow; the engine's own settings, written as a playbook, plan exactly what the engine does.
func TestB5216_PlaybookGym(t *testing.T) {
	good := []byte(`{"deal_type": "renewal", "rounds": 3, "open_pct": 40, "limit_pct": 80, "wording": {
		"ask": "Hi. We would like your best price for {what}.", "counter": "Thanks. For {what} we can do {offer}.",
		"walk_away": "Thanks for your time on {what}. We will leave it there."}}`)
	pb, err := ParseNegotiationPlaybook(good)
	if err != nil {
		t.Fatal(err)
	}
	if r := PlayNegotiationGym(pb); r.Failed != "" || r.Deals == 0 || r.ScoreBPS <= 0 {
		t.Fatalf("a clean playbook's gym = %+v; want it passed with deals and a saving", r)
	}
	pb.Wording.Counter = "Thanks. Another supplier is cheaper, so we offer {offer}."
	if r := PlayNegotiationGym(pb); !strings.Contains(r.Failed, "another offer the owner did not allow") {
		t.Fatalf("a playbook naming a rival offer: gym = %+v; want it failed on that", r)
	}
	if _, err := ParseNegotiationPlaybook([]byte(`{"deal_type": "renewal", "rounds": 3, "open_pct": 40, "limit_pct": 80,
		"wording": {"ask": "a", "counter": "b", "walk_away": "c"}, "walk_away_minor": 1}`)); err == nil {
		t.Fatal("a playbook carrying a field of a mandate was read; want it refused")
	}
	for _, w := range []string{"Thanks. We accept {offer}.", "Thanks. {offer}, or write to sales＠rival.example.", "See rival.example for {offer}.",
		"{offer}, or write to sales at rival dot example."} {
		pb.Wording.Counter = w
		raw, _ := json.Marshal(pb)
		if _, err := ParseNegotiationPlaybook(raw); err == nil {
			t.Fatalf("wording %q was read; want it refused: it commits the owner or points elsewhere", w)
		}
	}

	mt := NegotiationMandateTerms{Task: NegotiationTask{Kind: NegotiationRenewal, What: "x"}, Side: NegotiationBuy, Currency: "GBP",
		TargetMinor: 80_000, WalkAwayMinor: 100_000, Counterparties: []string{"a"}, Authority: AuthorityAccept}
	want, _ := NegotiationPlanFor(mt, nil)
	got, _ := NegotiationPlaybook{DealType: NegotiationRenewal, Rounds: 4, OpenPct: 50, LimitPct: 75}.Plan(mt, nil)
	if want.OpeningMinor != got.OpeningMinor || !slices.Equal(want.Schedule, got.Schedule) {
		t.Fatalf("the engine's settings as a playbook plan %+v; the engine plans %+v", got, want)
	}
}
