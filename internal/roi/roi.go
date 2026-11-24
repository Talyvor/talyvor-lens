// Package roi generates the executive ROI report — the artifact a VP of
// Engineering forwards to a CFO: AI spend by team / feature / engineer, the
// cost-per-shipped-feature trend, budget status, the forward forecast, and
// the top cost outliers, in one summary.
//
// It is REPORTING/AGGREGATION: read-only, off the request hot path
// (enforced by an import-parsing structural test), and it ORCHESTRATES the
// existing services (budgets, forecast, costanomaly, attribution) rather
// than running a fourth analytics layer — the Reporter holds no SQL of its
// own. Reports are cached with a TTL because the aggregation is expensive.
//
// Honesty is load-bearing here:
//   - Every figure is traceable to its source service, and the report
//     states the window it covers + when it was generated.
//   - Per-engineer spend is SENSITIVE (named people) and OFF by default —
//     an explicit opt-in. It is a cost ATTRIBUTION, not a performance
//     judgment; the wording says so.
//   - Forecast figures carry forecast's confidence note; anomalies carry
//     costanomaly's "statistical flag, not a verdict" framing.
//   - A thin-data period yields an explicit insufficient-data report, not
//     confident-looking hollow numbers.
//
// Delivery (emailing the report) is Track's job — Lens sends no email.
package roi

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/costanomaly"
	"github.com/talyvor/lens/internal/forecast"
	"github.com/talyvor/lens/internal/metrics"
)

const (
	defaultTopN        = 10
	defaultTrendMonths = 6
	defaultTTL         = time.Hour
	maxAnomalies       = 10
)

// ─── source interfaces (satisfied by the concrete services; mocked in tests) ───

// spendReader is costanomaly.Store — windowed cost-by-unit.
type spendReader interface {
	UnitCostsWindow(ctx context.Context, ws, unitKind string, since, until time.Time) ([]costanomaly.UnitCost, error)
}

// totalReader + budgetLister are budgets.Store.
type totalReader interface {
	ReconcileSpent(ctx context.Context, b budgets.Budget) (float64, error)
}
type budgetLister interface {
	List(ctx context.Context, workspaceID string) ([]budgets.Budget, error)
}

// forecastReader is forecast.Forecaster.
type forecastReader interface {
	ProjectScope(ctx context.Context, ws string, scope budgets.Scope, scopeID, period string) (forecast.Forecast, error)
}

// anomalyReader is costanomaly.Detector.
type anomalyReader interface {
	ScanScope(ctx context.Context, ws, scope string) (costanomaly.ScanResult, error)
}

// engineerReader is attribution.Store (author breakdown).
type engineerReader interface {
	GetSummary(ctx context.Context, workspaceID string, days int) (*attribution.Summary, error)
}

// ─── report shape ───

type TeamSpend struct {
	Team           string  `json:"team"`
	CostUSD        float64 `json:"cost_usd"`
	Pct            float64 `json:"pct"`
	DeltaVsPrevUSD float64 `json:"delta_vs_prev_usd"`
}

type FeatureSpend struct {
	IssueID string  `json:"issue_id"`
	CostUSD float64 `json:"cost_usd"`
	Pct     float64 `json:"pct"`
}

// EngineerSpend attributes cost to a named author. This is a COST
// ATTRIBUTION, not a productivity/performance metric — see the package doc.
type EngineerSpend struct {
	Author   string  `json:"author"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int     `json:"requests"`
}

type TrendPoint struct {
	Period       string  `json:"period"`
	AvgCostUSD   float64 `json:"avg_cost_usd"`
	FeatureCount int     `json:"feature_count"`
}

type BudgetStatusRow struct {
	Scope       string  `json:"scope"`
	ScopeID     string  `json:"scope_id"`
	LimitUSD    float64 `json:"limit_usd"`
	SpentUSD    float64 `json:"spent_usd"`
	Utilization float64 `json:"utilization"`
	Status      string  `json:"status"` // ok | warn | over | unlimited
}

// ForecastSummary carries forecast's projection AND its confidence note —
// these are projections, never guarantees.
type ForecastSummary struct {
	ProjectedTotalUSD   float64 `json:"projected_total_usd"`
	LimitUSD            float64 `json:"limit_usd,omitempty"`
	WillExceed          bool    `json:"will_exceed"`
	ProjectedOverageUSD float64 `json:"projected_overage_usd,omitempty"`
	ConfidenceNote      string  `json:"confidence_note"`
	InsufficientData    bool    `json:"insufficient_data"`
}

// AnomalyRow is a flagged cost outlier — a statistical flag, not a verdict
// (the explanation carries that framing).
type AnomalyRow struct {
	UnitID      string  `json:"unit_id"`
	CostUSD     float64 `json:"cost_usd"`
	Factor      float64 `json:"factor"`
	Severity    string  `json:"severity"`
	Explanation string  `json:"explanation"`
}

type PrevComparison struct {
	PrevTotalUSD float64 `json:"prev_total_usd"`
	DeltaUSD     float64 `json:"delta_usd"`
	PctChange    float64 `json:"pct_change"`
}

// ExecReport is the full structured report. Every figure is sourced from an
// existing service; field names use "projected"/"est" where a number is a
// projection rather than a realized fact.
type ExecReport struct {
	WorkspaceID   string    `json:"workspace_id"`
	Period        string    `json:"period"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	GeneratedAt   time.Time `json:"generated_at"`
	TotalSpendUSD float64   `json:"total_spend_usd"`

	SpendByTeam     []TeamSpend     `json:"spend_by_team"`
	SpendByFeature  []FeatureSpend  `json:"spend_by_feature"`
	SpendByEngineer []EngineerSpend `json:"spend_by_engineer,omitempty"`

	EngineerBreakdownEnabled bool   `json:"engineer_breakdown_enabled"`
	EngineerBreakdownNote    string `json:"engineer_breakdown_note,omitempty"`

	CostPerFeatureTrend  []TrendPoint      `json:"cost_per_feature_trend"`
	BudgetStatus         []BudgetStatusRow `json:"budget_status"`
	ForecastSummary      ForecastSummary   `json:"forecast_summary"`
	Anomalies            []AnomalyRow      `json:"anomalies"`
	PrevPeriodComparison PrevComparison    `json:"prev_period_comparison"`

	InsufficientData bool   `json:"insufficient_data"`
	DataNote         string `json:"data_note,omitempty"`
}

// Config tunes the reporter. Zero fields fall back to defaults.
type Config struct {
	IncludeEngineerBreakdown bool
	TopN                     int
	TrendMonths              int
	TTL                      time.Duration
}

const engineerDisabledNote = "Per-engineer cost attribution is disabled by default. It is a cost ATTRIBUTION (which work a dollar landed against), not a productivity or performance judgment. Enable explicitly via LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN=true."

type cacheEntry struct {
	rep ExecReport
	at  time.Time
}

// Reporter composes the existing services into the executive report. Safe
// for concurrent use.
type Reporter struct {
	spend    spendReader
	total    totalReader
	budgets  budgetLister
	forecast forecastReader
	anomaly  anomalyReader
	engineer engineerReader

	cfg Config
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New wires the reporter from the concrete services. In production:
// spend/anomaly = costanomaly Store/Detector, total/budgets = budgets.Store,
// forecast = forecast.Forecaster, engineer = attribution.Store.
func New(spend spendReader, total totalReader, budgetList budgetLister, fc forecastReader, an anomalyReader, eng engineerReader, cfg Config) *Reporter {
	if cfg.TopN <= 0 {
		cfg.TopN = defaultTopN
	}
	if cfg.TrendMonths <= 0 {
		cfg.TrendMonths = defaultTrendMonths
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	return &Reporter{
		spend: spend, total: total, budgets: budgetList,
		forecast: fc, anomaly: an, engineer: eng,
		cfg: cfg, now: time.Now, cache: make(map[string]cacheEntry),
	}
}

// GenerateReport builds (or returns a cached) ExecReport for the workspace +
// period (monthly default). Cached for cfg.TTL — the aggregation is
// expensive, so dashboard/API polls don't regenerate it per request.
func (r *Reporter) GenerateReport(ctx context.Context, ws, period string) (ExecReport, error) {
	// Only the periods a report has (budgets.PeriodBounds) are kept; any other value, "total" included, is the month.
	// The period comes off the query string, so what is reported (and cached) is always one of these constants (B67.1).
	switch period {
	case "weekly":
		period = "weekly"
	default:
		period = "monthly"
	}
	now := r.now()
	key := ws + "|" + period

	r.mu.Lock()
	if e, ok := r.cache[key]; ok && now.Sub(e.at) < r.cfg.TTL {
		rep := e.rep
		r.mu.Unlock()
		return rep, nil
	}
	r.mu.Unlock()

	start := time.Now() // real wall-clock for the duration metric
	rep, err := r.generate(ctx, ws, period, now)
	if err != nil {
		return rep, err
	}
	metrics.ROIReportGenerated(period)
	metrics.ObserveROIReportDuration(time.Since(start))

	r.mu.Lock()
	r.cache[key] = cacheEntry{rep: rep, at: now}
	r.mu.Unlock()
	return rep, nil
}

func (r *Reporter) generate(ctx context.Context, ws, period string, now time.Time) (ExecReport, error) {
	curStart, curEnd, ok := budgets.PeriodBounds(period, now)
	if !ok {
		// "total"/open-ended isn't a report period; fall back to the month.
		period = "monthly"
		curStart, curEnd, _ = budgets.PeriodBounds(period, now)
	}
	// Previous contiguous period: bounds anchored just before curStart.
	prevStart, prevEnd, _ := budgets.PeriodBounds(period, curStart.AddDate(0, 0, -1))

	rep := ExecReport{
		WorkspaceID: ws, Period: period,
		PeriodStart: curStart, PeriodEnd: curEnd, GeneratedAt: now,
		EngineerBreakdownEnabled: r.cfg.IncludeEngineerBreakdown,
	}

	// Total (authoritative token_events sum) — current + previous windows.
	total, err := r.total.ReconcileSpent(ctx, workspaceBudget(ws, curStart, curEnd))
	if err != nil {
		return rep, err
	}
	rep.TotalSpendUSD = total
	prevTotal, err := r.total.ReconcileSpent(ctx, workspaceBudget(ws, prevStart, prevEnd))
	if err != nil {
		return rep, err
	}
	rep.PrevPeriodComparison = PrevComparison{
		PrevTotalUSD: prevTotal,
		DeltaUSD:     total - prevTotal,
		PctChange:    pctChange(total, prevTotal),
	}

	// Thin data: no spend this period → an honest skeleton, not hollow numbers.
	if total <= 0 {
		rep.InsufficientData = true
		rep.DataNote = "No AI spend recorded for this workspace in this period — nothing to report yet."
		if !r.cfg.IncludeEngineerBreakdown {
			rep.EngineerBreakdownNote = engineerDisabledNote
		}
		return rep, nil
	}

	// Spend by team (current + previous for the delta).
	teamCur, err := r.spend.UnitCostsWindow(ctx, ws, costanomaly.UnitTeam, curStart, curEnd)
	if err != nil {
		return rep, err
	}
	teamPrev, err := r.spend.UnitCostsWindow(ctx, ws, costanomaly.UnitTeam, prevStart, prevEnd)
	if err != nil {
		return rep, err
	}
	rep.SpendByTeam = buildTeamSpend(teamCur, teamPrev, total)

	// Spend by feature (issue), top N.
	feat, err := r.spend.UnitCostsWindow(ctx, ws, costanomaly.UnitIssue, curStart, curEnd)
	if err != nil {
		return rep, err
	}
	rep.SpendByFeature = buildFeatureSpend(feat, total, r.cfg.TopN)

	// Spend by engineer — sensitive, opt-in only.
	if r.cfg.IncludeEngineerBreakdown {
		days := daysBetween(curStart, now)
		sum, err := r.engineer.GetSummary(ctx, ws, days)
		if err != nil {
			return rep, err
		}
		if sum != nil {
			rep.SpendByEngineer = buildEngineerSpend(sum.ByAuthor, r.cfg.TopN)
		}
	} else {
		rep.EngineerBreakdownNote = engineerDisabledNote
	}

	// Cost-per-feature trend (always monthly buckets, last TrendMonths).
	rep.CostPerFeatureTrend, err = r.costPerFeatureTrend(ctx, ws, now)
	if err != nil {
		return rep, err
	}

	// Budget status (all budgets in the workspace).
	budgetList, err := r.budgets.List(ctx, ws)
	if err != nil {
		return rep, err
	}
	rep.BudgetStatus = buildBudgetStatus(budgetList)

	// Forward forecast (workspace scope) — carries the confidence note.
	fc, err := r.forecast.ProjectScope(ctx, ws, budgets.ScopeWorkspace, ws, period)
	if err != nil {
		return rep, err
	}
	rep.ForecastSummary = buildForecastSummary(fc)

	// Top cost outliers (issue scope) — statistical flags, not verdicts.
	scan, err := r.anomaly.ScanScope(ctx, ws, costanomaly.UnitIssue)
	if err != nil {
		return rep, err
	}
	rep.Anomalies = buildAnomalies(scan, maxAnomalies)

	return rep, nil
}

func (r *Reporter) costPerFeatureTrend(ctx context.Context, ws string, now time.Time) ([]TrendPoint, error) {
	out := make([]TrendPoint, 0, r.cfg.TrendMonths)
	// Oldest → newest so the trend reads left-to-right.
	for i := r.cfg.TrendMonths - 1; i >= 0; i-- {
		anchor := now.AddDate(0, -i, 0)
		ms, me, ok := budgets.PeriodBounds("monthly", anchor)
		if !ok {
			continue
		}
		issues, err := r.spend.UnitCostsWindow(ctx, ws, costanomaly.UnitIssue, ms, me)
		if err != nil {
			return nil, err
		}
		var sum float64
		for _, c := range issues {
			sum += c.CostUSD
		}
		avg := 0.0
		if len(issues) > 0 {
			avg = sum / float64(len(issues))
		}
		out = append(out, TrendPoint{Period: ms.Format("2006-01"), AvgCostUSD: avg, FeatureCount: len(issues)})
	}
	return out, nil
}

// ─── pure builders (independently testable) ───

func workspaceBudget(ws string, start, end time.Time) budgets.Budget {
	return budgets.Budget{WorkspaceID: ws, Scope: budgets.ScopeWorkspace, ScopeID: ws, Period: "total", StartsAt: &start, EndsAt: &end}
}

func pctChange(cur, prev float64) float64 {
	if prev <= 0 {
		return 0
	}
	return (cur - prev) / prev * 100
}

func buildTeamSpend(cur, prev []costanomaly.UnitCost, total float64) []TeamSpend {
	prevByID := make(map[string]float64, len(prev))
	for _, p := range prev {
		prevByID[p.UnitID] = p.CostUSD
	}
	out := make([]TeamSpend, 0, len(cur))
	for _, c := range cur {
		pct := 0.0
		if total > 0 {
			pct = c.CostUSD / total * 100
		}
		out = append(out, TeamSpend{
			Team: c.UnitID, CostUSD: c.CostUSD, Pct: pct,
			DeltaVsPrevUSD: c.CostUSD - prevByID[c.UnitID],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out
}

func buildFeatureSpend(cur []costanomaly.UnitCost, total float64, topN int) []FeatureSpend {
	out := make([]FeatureSpend, 0, len(cur))
	for _, c := range cur {
		pct := 0.0
		if total > 0 {
			pct = c.CostUSD / total * 100
		}
		out = append(out, FeatureSpend{IssueID: c.UnitID, CostUSD: c.CostUSD, Pct: pct})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

func buildEngineerSpend(authors []attribution.AuthorCost, topN int) []EngineerSpend {
	out := make([]EngineerSpend, 0, len(authors))
	for _, a := range authors {
		out = append(out, EngineerSpend{Author: a.Author, CostUSD: a.CostUSD, Requests: a.Requests})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

func buildBudgetStatus(list []budgets.Budget) []BudgetStatusRow {
	out := make([]BudgetStatusRow, 0, len(list))
	for _, b := range list {
		row := BudgetStatusRow{
			Scope: string(b.Scope), ScopeID: b.ScopeID,
			LimitUSD: b.LimitUSD, SpentUSD: b.SpentUSD,
		}
		switch {
		case b.LimitUSD <= 0:
			row.Status = "unlimited"
		default:
			row.Utilization = b.SpentUSD / b.LimitUSD
			switch {
			case row.Utilization >= 1:
				row.Status = "over"
			case row.Utilization >= 0.8:
				row.Status = "warn"
			default:
				row.Status = "ok"
			}
		}
		out = append(out, row)
	}
	return out
}

func buildForecastSummary(fc forecast.Forecast) ForecastSummary {
	fs := ForecastSummary{
		ProjectedTotalUSD: fc.ProjectedTotalUSD,
		ConfidenceNote:    fc.ConfidenceNote,
		InsufficientData:  fc.InsufficientData,
	}
	if fc.VsBudget != nil {
		fs.LimitUSD = fc.VsBudget.LimitUSD
		fs.WillExceed = fc.VsBudget.WillExceed
		fs.ProjectedOverageUSD = fc.VsBudget.ProjectedOverageUSD
	}
	return fs
}

func buildAnomalies(scan costanomaly.ScanResult, max int) []AnomalyRow {
	if scan.InsufficientBaseline {
		return nil
	}
	rows := make([]AnomalyRow, 0, len(scan.Anomalies))
	for _, a := range scan.Anomalies {
		rows = append(rows, AnomalyRow{
			UnitID: a.UnitID, CostUSD: a.CostUSD, Factor: a.Factor,
			Severity: string(a.Severity), Explanation: a.Explanation,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Factor > rows[j].Factor })
	if max > 0 && len(rows) > max {
		rows = rows[:max]
	}
	return rows
}

func daysBetween(start, end time.Time) int {
	d := int(end.Sub(start).Hours() / 24)
	if d < 1 {
		return 1
	}
	return d
}

// ReportSummary is the compact headline view for the dashboard.
type ReportSummary struct {
	WorkspaceID        string      `json:"workspace_id"`
	Period             string      `json:"period"`
	GeneratedAt        time.Time   `json:"generated_at"`
	TotalSpendUSD      float64     `json:"total_spend_usd"`
	PctChangeVsPrev    float64     `json:"pct_change_vs_prev"`
	TopTeams           []TeamSpend `json:"top_teams"`
	ProjectedTotalUSD  float64     `json:"projected_total_usd"`
	ForecastWillExceed bool        `json:"forecast_will_exceed"`
	BudgetsOverCount   int         `json:"budgets_over_count"`
	AnomalyCount       int         `json:"anomaly_count"`
	InsufficientData   bool        `json:"insufficient_data"`
}

// Summarize extracts the dashboard headline from a full report (pure).
func Summarize(rep ExecReport) ReportSummary {
	s := ReportSummary{
		WorkspaceID: rep.WorkspaceID, Period: rep.Period, GeneratedAt: rep.GeneratedAt,
		TotalSpendUSD: rep.TotalSpendUSD, PctChangeVsPrev: rep.PrevPeriodComparison.PctChange,
		ProjectedTotalUSD:  rep.ForecastSummary.ProjectedTotalUSD,
		ForecastWillExceed: rep.ForecastSummary.WillExceed,
		AnomalyCount:       len(rep.Anomalies),
		InsufficientData:   rep.InsufficientData,
	}
	if len(rep.SpendByTeam) > 3 {
		s.TopTeams = rep.SpendByTeam[:3]
	} else {
		s.TopTeams = rep.SpendByTeam
	}
	for _, b := range rep.BudgetStatus {
		if b.Status == "over" {
			s.BudgetsOverCount++
		}
	}
	return s
}

// GenerateSummary returns the compact dashboard summary (uses the cached
// report under the hood).
func (r *Reporter) GenerateSummary(ctx context.Context, ws, period string) (ReportSummary, error) {
	rep, err := r.GenerateReport(ctx, ws, period)
	if err != nil {
		return ReportSummary{}, err
	}
	return Summarize(rep), nil
}

// StartSchedule reuses the codebase's goroutine-ticker pattern to warm the
// report cache for the given workspaces every interval. It only
// pre-generates (and caches) — DELIVERY is Track's job; Lens sends no email.
// Exits when ctx is cancelled.
func (r *Reporter) StartSchedule(ctx context.Context, interval time.Duration, workspaces []string) {
	if interval <= 0 || len(workspaces) == 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, ws := range workspaces {
					_, _ = r.GenerateReport(ctx, ws, "monthly")
				}
			}
		}
	}()
}
