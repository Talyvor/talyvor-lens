package roi

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/brand"
	"github.com/talyvor/lens/internal/budgets"
)

func TestRenderHTML_SelfContainedAndComplete(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{IncludeEngineerBreakdown: true}).GenerateReport(context.Background(), "ws1", "monthly")
	html, err := RenderHTML(rep)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	// Valid, complete document.
	for _, want := range []string{"<!DOCTYPE html>", "</html>", "Executive AI-Cost Report"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	// Key sections present.
	for _, want := range []string{"Spend by team", "Spend by feature", "Spend by engineer", "Cost per feature", "Budget status", "Forward forecast", "Top cost outliers"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing section %q", want)
		}
	}
	// Self-contained: no external assets.
	for _, bad := range []string{"http://", "https://", "src=", "<link", "<script"} {
		if strings.Contains(html, bad) {
			t.Errorf("HTML must be self-contained; found %q", bad)
		}
	}
	// Statistical-flag framing carried through.
	if !strings.Contains(html, "not verdicts") && !strings.Contains(html, "not a judgment") {
		t.Error("HTML should frame anomalies as flags, not judgments")
	}
}

// The report wears Pocket (B57.14): the tokens, the light theme on paper, and the
// symbol — and none of the colours it wore before.
func TestRenderHTML_WearsTheBrand(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{}).GenerateReport(context.Background(), "ws1", "monthly")
	html, err := RenderHTML(rep)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	for name, want := range map[string]string{"tokens": brand.TokensCSS, "print theme": brand.PrintCSS, "mark": brand.Mark} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing the brand %s", name)
		}
	}
	if !strings.Contains(html, "#FF7A1A") || !strings.Contains(html, `"Bricolage Grotesque"`) {
		t.Error("HTML is not in Pocket's colours and type")
	}
	for _, old := range []string{"#060a12", "#0f7a6c", "#1a1a2e", "#e0e0ea", "#137333", "#a06000", "#c5221f", "#f0d000", "#fef7e0"} {
		// The symbol's own file carries the logo kit's colours (B57.81).
		if strings.Contains(strings.ToLower(strings.Replace(html, brand.Mark, "", 1)), old) {
			t.Errorf("HTML still carries the pre-brand colour %s", old)
		}
	}
}

func TestRenderHTML_EngineerHiddenWhenFlagOff(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{IncludeEngineerBreakdown: false}).GenerateReport(context.Background(), "ws1", "monthly")
	html, _ := RenderHTML(rep)
	if strings.Contains(html, "Spend by engineer") {
		t.Error("engineer section must be hidden in HTML when the flag is off")
	}
}

func TestRenderMarkdown_ContainsFigures(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{}).GenerateReport(context.Background(), "ws1", "monthly")
	md := RenderMarkdown(rep)
	for _, want := range []string{
		"# Executive AI-Cost Report",
		"$100.00",       // total
		"Spend by team", // section
		"core",          // a team
		"Statistical flags, not verdicts",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	// Engineer section omitted when flag off.
	if strings.Contains(md, "Spend by engineer") {
		t.Error("markdown engineer section must be omitted when flag off")
	}
}

func TestRender_ThinDataNotHollow(t *testing.T) {
	m := fullMock()
	m.reconcile = func(b budgets.Budget) float64 { return 0 } // no spend
	rep, _ := newReporter(m, Config{}).GenerateReport(context.Background(), "ws1", "monthly")

	html, err := RenderHTML(rep)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(html, rep.DataNote) || !strings.Contains(html, "banner") {
		t.Error("thin-data HTML should show the insufficient-data banner, not hollow tables")
	}
	// No populated breakdown tables in a thin-data report.
	if strings.Contains(html, "Spend by team") {
		t.Error("thin-data HTML must not render breakdown sections")
	}

	md := RenderMarkdown(rep)
	if !strings.Contains(md, rep.DataNote) || strings.Contains(md, "Spend by team") {
		t.Error("thin-data markdown should show the note, not hollow sections")
	}
}
