package scan

import (
	"context"
	"strings"
	"testing"
)

func checkedDraft() Draft {
	return Draft{Series: "eia.stocks.cushing-crude", Title: "US oil stocks: cushing crude",
		WhatChanged:     "US oil stocks: cushing crude read 431.2 thousand barrels on 30 October 2026, 2.1 standard deviations above its normal.",
		WhyItMatters:    "The model's likely range for 27 November 2026 is 420.5 to 441.0 thousand barrels.",
		Confidence:      "Tested on 340 past outlooks, it was on the right side of the normal 66% of the time.",
		WhatWouldChange: "The view would change if the coming readings moved back towards the normal.",
		Sources:         []string{"Source: U.S. Energy Information Administration"},
		Figures: []Figure{{Name: "value", Text: "431.2"}, {Name: "at", Text: "30 October 2026"}, {Name: "z", Text: "2.1"},
			{Name: "outlook_at", Text: "27 November 2026"}, {Name: "outlook_low", Text: "420.5"},
			{Name: "outlook_high", Text: "441.0"}, {Name: "tested", Text: "340"}, {Name: "hit_rate", Text: "66"}},
		Box: HowMade{MadeBy: MadeBy, Data: []string{"US EIA open data: US government public domain, credit required"},
			Method: "seasonal normal", Backtest: "passed", Conflicts: "None.", TrackRecord: TrackRecordPath}}
}

// A draft that says buy, gives a price target, prints a number the engine did not give, or lacks a part of its box
// is refused by the code rules; the clean draft passes.
func TestCheckRefusesAdviceTargetsForeignNumbersAndAMissingBox(t *testing.T) {
	if p := Check(checkedDraft()); len(p) != 0 {
		t.Fatalf("a clean draft was refused: %v", p)
	}
	for name, edit := range map[string]func(*Draft){
		"buy":             func(d *Draft) { d.WhyItMatters = "A good moment to buy crude futures." },
		"short":           func(d *Draft) { d.WhatWouldChange = "Traders may go short." },
		"price target":    func(d *Draft) { d.WhyItMatters = "Our price target for WTI is 431.2." },
		"money figure":    func(d *Draft) { d.WhyItMatters = "WTI may reach $431.2 a barrel." },
		"foreign number":  func(d *Draft) { d.WhatChanged = "Stocks read 425.0 thousand barrels." },
		"tailored":        func(d *Draft) { d.WhatWouldChange = "Consider your portfolio." },
		"link":            func(d *Draft) { d.WhatWouldChange = "More at https://example.test/x." },
		"no conflicts":    func(d *Draft) { d.Box.Conflicts = "" },
		"no track record": func(d *Draft) { d.Box.TrackRecord = "" },
		"no data":         func(d *Draft) { d.Box.Data = nil },
		"no sources":      func(d *Draft) { d.Sources = nil },
	} {
		d := checkedDraft()
		edit(&d)
		if p := Check(d); len(p) == 0 {
			t.Errorf("%s: passed the code rules", name)
		}
	}
}

type fakeInferer map[string]string // model → reply

func (f fakeInferer) Infer(_ context.Context, model, _ string) (string, error) { return f[model], nil }

// The writer model's words stand only when they pass the code rules; a draft that says buy leaves the template.
func TestWriterModelWordingFallsBackToTheTemplate(t *testing.T) {
	d := checkedDraft()
	ok := `{"what_changed": "Cushing crude stocks were 431.2 thousand barrels on 30 October 2026, 2.1 standard deviations above normal.",
		"why_it_matters": "Its model puts them at 420.5 to 441.0 thousand barrels by 27 November 2026.",
		"confidence": "Its outlooks were on the right side of the normal 66% of the time over 340 tests.",
		"what_would_change": "Readings moving back towards the normal would change the view."}`
	w := NewWriter(nil, fakeInferer{"writer": "```json\n" + ok + "\n```"}, "writer", "", "None.")
	md, used := w.worded(context.Background(), d, backtestRow{series: d.Series})
	if !used || !strings.HasPrefix(md.WhatChanged, "Cushing crude stocks") {
		t.Fatalf("a clean model draft was not used: %v %+v", used, md)
	}
	w = NewWriter(nil, fakeInferer{"writer": strings.Replace(ok, "would change the view", "is a signal to buy", 1)},
		"writer", "", "None.")
	if md, used := w.worded(context.Background(), d, backtestRow{series: d.Series}); used || md.WhatWouldChange != d.WhatWouldChange {
		t.Fatalf("a model draft saying buy was used: %+v", md)
	}
	w = NewWriter(nil, fakeInferer{"writer": strings.Replace(ok, "431.2", "435.0", 1)}, "writer", "", "None.")
	if _, used := w.worded(context.Background(), d, backtestRow{series: d.Series}); used {
		t.Fatal("a model draft with a number the engine never gave was used")
	}
}
