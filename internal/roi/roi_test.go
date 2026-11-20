package roi

import (
	"context"
	"go/build"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/costanomaly"
	"github.com/talyvor/lens/internal/forecast"
	"github.com/talyvor/lens/internal/metrics"
)

var fixedNow = time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

func monthWindows() (curStart, curEnd, prevStart time.Time) {
	cs, ce, _ := budgets.PeriodBounds("monthly", fixedNow)
	ps, _, _ := budgets.PeriodBounds("monthly", cs.AddDate(0, 0, -1))
	return cs, ce, ps
}

// mockSrc implements all six reporter source interfaces.
type mockSrc struct {
	reconcile      func(b budgets.Budget) float64
	unitCosts      func(kind string, since, until time.Time) []costanomaly.UnitCost
	budgetList     []budgets.Budget
	forecastFn     forecast.Forecast
	scan           costanomaly.ScanResult
	summary        *attribution.Summary
	reconcileCalls int
}

func (m *mockSrc) ReconcileSpent(_ context.Context, b budgets.Budget) (float64, error) {
	m.reconcileCalls++
	return m.reconcile(b), nil
}
func (m *mockSrc) UnitCostsWindow(_ context.Context, _, kind string, since, until time.Time) ([]costanomaly.UnitCost, error) {
	if m.unitCosts == nil {
		return nil, nil
	}
	return m.unitCosts(kind, since, until), nil
}
func (m *mockSrc) List(_ context.Context, _ string) ([]budgets.Budget, error) {
	return m.budgetList, nil
}
func (m *mockSrc) ProjectScope(_ context.Context, _ string, _ budgets.Scope, _, _ string) (forecast.Forecast, error) {
	return m.forecastFn, nil
}
func (m *mockSrc) ScanScope(_ context.Context, _, _ string) (costanomaly.ScanResult, error) {
	return m.scan, nil
}
func (m *mockSrc) GetSummary(_ context.Context, _ string, _ int) (*attribution.Summary, error) {
	return m.summary, nil
}

func newReporter(m *mockSrc, cfg Config) *Reporter {
	r := New(m, m, m, m, m, m, cfg)
	r.now = func() time.Time { return fixedNow }
	return r
}

func uc(id string, cost float64) costanomaly.UnitCost {
	return costanomaly.UnitCost{UnitID: id, CostUSD: cost}
}

// fullMock returns a mock with a normal month of data ($100 total).
func fullMock() *mockSrc {
	curStart, _, prevStart := monthWindows()
	return &mockSrc{
		reconcile: func(b budgets.Budget) float64 {
			if b.StartsAt != nil && b.StartsAt.Equal(curStart) {
				return 100
			}
			if b.StartsAt != nil && b.StartsAt.Equal(prevStart) {
				return 80
			}
			return 0
		},
		unitCosts: func(kind string, since, until time.Time) []costanomaly.UnitCost {
			switch {
			case kind == costanomaly.UnitTeam && since.Equal(curStart):
				return []costanomaly.UnitCost{uc("core", 60), uc("infra", 40)}
			case kind == costanomaly.UnitTeam && since.Equal(prevStart):
				return []costanomaly.UnitCost{uc("core", 50), uc("infra", 30)}
			case kind == costanomaly.UnitIssue && since.Equal(curStart):
				return []costanomaly.UnitCost{uc("ENG-1", 70), uc("ENG-2", 30)}
			}
			return nil
		},
		budgetList: []budgets.Budget{
			{Scope: budgets.ScopeWorkspace, ScopeID: "ws1", LimitUSD: 1000, SpentUSD: 100},
			{Scope: budgets.ScopeTeam, ScopeID: "core", LimitUSD: 50, SpentUSD: 60}, // over
		},
		forecastFn: forecast.Forecast{
			ProjectedTotalUSD: 200, ConfidenceNote: "based on 16 days of data — reasonable confidence",
			VsBudget: &forecast.VsBudget{LimitUSD: 1000, WillExceed: false},
		},
		scan: costanomaly.ScanResult{Anomalies: []costanomaly.Anomaly{
			{UnitID: "ENG-2", CostUSD: 30, Factor: 3.5, Severity: costanomaly.SeverityWarn, Explanation: "statistical flag"},
			{UnitID: "ENG-9", CostUSD: 90, Factor: 9.0, Severity: costanomaly.SeverityHigh, Explanation: "statistical flag"},
		}},
		summary: &attribution.Summary{ByAuthor: []attribution.AuthorCost{
			{Author: "alice", CostUSD: 70, Requests: 10},
			{Author: "bob", CostUSD: 30, Requests: 5},
		}},
	}
}

func TestGenerateReport_ComposesAllSections(t *testing.T) {
	rep, err := newReporter(fullMock(), Config{}).GenerateReport(context.Background(), "ws1", "monthly")
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalSpendUSD != 100 {
		t.Fatalf("total: got %.2f want 100", rep.TotalSpendUSD)
	}
	if len(rep.SpendByTeam) != 2 || rep.SpendByTeam[0].Team != "core" {
		t.Fatalf("by-team: %+v", rep.SpendByTeam)
	}
	if len(rep.SpendByFeature) != 2 || rep.SpendByFeature[0].IssueID != "ENG-1" {
		t.Fatalf("by-feature: %+v", rep.SpendByFeature)
	}
	if rep.ForecastSummary.ProjectedTotalUSD != 200 || rep.ForecastSummary.ConfidenceNote == "" {
		t.Fatalf("forecast summary not carried through: %+v", rep.ForecastSummary)
	}
	// Anomalies sorted by factor desc, framing carried.
	if len(rep.Anomalies) != 2 || rep.Anomalies[0].UnitID != "ENG-9" {
		t.Fatalf("anomalies not sorted by factor: %+v", rep.Anomalies)
	}
	// Budget status: the over-limit team budget is flagged.
	var sawOver bool
	for _, b := range rep.BudgetStatus {
		if b.ScopeID == "core" && b.Status == "over" {
			sawOver = true
		}
	}
	if !sawOver {
		t.Fatalf("expected core team budget flagged 'over': %+v", rep.BudgetStatus)
	}
}

func TestSpendByTeam_PercentagesAndDeltas(t *testing.T) {
	rep, _ := newReporter(fullMock(), Config{}).GenerateReport(context.Background(), "ws1", "monthly")
	var sumPct float64
	deltas := map[string]float64{}
	for _, t := range rep.SpendByTeam {
		sumPct += t.Pct
		deltas[t.Team] = t.DeltaVsPrevUSD
	}
	if sumPct < 99.9 || sumPct > 100.1 {
		t.Fatalf("team pct should sum to ~100, got %.2f", sumPct)
	}
	if deltas["core"] != 10 || deltas["infra"] != 10 { // 60-50, 40-30
		t.Fatalf("deltas vs prev wrong: %+v", deltas)
	}
	// Previous-period comparison: 100 vs 80 → +25%.
	if pc := rep.PrevPeriodComparison; pc.DeltaUSD != 20 || pc.PctChange < 24.9 || pc.PctChange > 25.1 {
		t.Fatalf("prev comparison wrong: %+v", pc)
	}
}

func TestTopN_Truncation(t *testing.T) {
	curStart, _, _ := monthWindows()
	m := fullMock()
	// 15 issues; expect top 10 by cost.
	m.unitCosts = func(kind string, since, until time.Time) []costanomaly.UnitCost {
		if kind == costanomaly.UnitIssue && since.Equal(curStart) {
			var list []costanomaly.UnitCost
			for i := 0; i < 15; i++ {
				list = append(list, uc("ENG-"+string(rune('A'+i)), float64(i+1)))
			}
			return list
		}
		return nil
	}
	rep, _ := newReporter(m, Config{TopN: 10}).GenerateReport(context.Background(), "ws1", "monthly")
	if len(rep.SpendByFeature) != 10 {
		t.Fatalf("top-N truncation: got %d want 10", len(rep.SpendByFeature))
	}
	if rep.SpendByFeature[0].CostUSD != 15 { // highest first
		t.Fatalf("top feature should be the most expensive: %+v", rep.SpendByFeature[0])
	}
}

func TestEngineerBreakdown_GatedByFlag(t *testing.T) {
	ctx := context.Background()

	off, _ := newReporter(fullMock(), Config{IncludeEngineerBreakdown: false}).GenerateReport(ctx, "ws1", "monthly")
	if off.EngineerBreakdownEnabled || len(off.SpendByEngineer) != 0 {
		t.Fatalf("engineer breakdown must be OMITTED when flag false: %+v", off.SpendByEngineer)
	}
	if off.EngineerBreakdownNote == "" {
		t.Fatal("disabled breakdown should carry the explanatory note")
	}

	on, _ := newReporter(fullMock(), Config{IncludeEngineerBreakdown: true}).GenerateReport(ctx, "ws1", "monthly")
	if !on.EngineerBreakdownEnabled || len(on.SpendByEngineer) != 2 || on.SpendByEngineer[0].Author != "alice" {
		t.Fatalf("engineer breakdown must be PRESENT when flag true: %+v", on.SpendByEngineer)
	}
}

func TestThinData_HonestInsufficientReport(t *testing.T) {
	m := fullMock()
	m.reconcile = func(b budgets.Budget) float64 { return 0 } // no spend at all
	rep, _ := newReporter(m, Config{}).GenerateReport(context.Background(), "ws1", "monthly")
	if !rep.InsufficientData || rep.DataNote == "" {
		t.Fatalf("zero spend must yield an explicit insufficient-data report: %+v", rep)
	}
	if len(rep.SpendByTeam) != 0 || len(rep.SpendByFeature) != 0 {
		t.Fatalf("thin-data report must not present hollow breakdowns: %+v", rep)
	}
}

func TestGenerateReport_CachesWithinTTL(t *testing.T) {
	ctx := context.Background()
	m := fullMock()
	clock := fixedNow
	r := New(m, m, m, m, m, m, Config{})
	r.now = func() time.Time { return clock }

	_, _ = r.GenerateReport(ctx, "ws1", "monthly")
	_, _ = r.GenerateReport(ctx, "ws1", "monthly")
	if m.reconcileCalls != 2 { // one generation = cur + prev reconcile
		t.Fatalf("within TTL report must be generated once (2 reconcile calls), got %d", m.reconcileCalls)
	}
	clock = fixedNow.Add(defaultTTL + time.Second)
	_, _ = r.GenerateReport(ctx, "ws1", "monthly")
	if m.reconcileCalls != 4 {
		t.Fatalf("after TTL expiry report must regenerate, got %d reconcile calls", m.reconcileCalls)
	}
}

func TestMetrics_BoundedCardinality(t *testing.T) {
	ctx := context.Background()
	r := newReporter(fullMock(), Config{})
	for i := 0; i < 5; i++ {
		_, _ = r.GenerateReport(ctx, "ws1", "monthly")
	}
	// Only the bounded {period} label — never a per-workspace series.
	if n := testutil.CollectAndCount(metrics.ROIReportsGeneratedTotal); n > 3 {
		t.Fatalf("roi_report_generated_total has %d series — unbounded labels leaked", n)
	}
}

func TestROIHasNoHotPathDependency(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	forbidden := map[string]bool{
		"github.com/talyvor/lens/internal/proxy": true,
		"github.com/talyvor/lens/internal/api":   true,
		"net/http":                               true,
	}
	for _, imp := range pkg.Imports {
		if forbidden[imp] {
			t.Errorf("roi must not import %q — it must stay read-only and off the request/hot path", imp)
		}
	}
}

// B67.1 (CodeQL go/reflected-xss #19, #20, #23): the period came off the query string and was echoed into the
// report, Markdown included, whatever it said; a period the report does not know is reported as the month it is.
func TestGenerateReport_UnknownPeriodIsReportedAsTheMonth(t *testing.T) {
	rep, err := newReporter(fullMock(), Config{}).GenerateReport(context.Background(), "ws1", "<script>alert(1)</script>")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Period != "monthly" {
		t.Fatalf("period = %q, want monthly", rep.Period)
	}
	if md := RenderMarkdown(rep); strings.Contains(md, "<script>") {
		t.Fatalf("the query's period reached the report:\n%s", md)
	}
}
