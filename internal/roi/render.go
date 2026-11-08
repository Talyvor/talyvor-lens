package roi

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"

	"github.com/talyvor/lens/internal/brand"
)

// RenderMarkdown renders the report as Markdown — for pasting into
// email/Slack/docs. All figures present; projections/flags keep their
// framing.
func RenderMarkdown(rep ExecReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Executive AI-Cost Report — %s\n\n", rep.WorkspaceID)
	fmt.Fprintf(&b, "**Period:** %s (%s → %s)  \n", rep.Period, rep.PeriodStart.Format("2006-01-02"), rep.PeriodEnd.Format("2006-01-02"))
	fmt.Fprintf(&b, "**Generated:** %s  \n", rep.GeneratedAt.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "**Total AI spend:** %s\n\n", usd(rep.TotalSpendUSD))

	if rep.InsufficientData {
		fmt.Fprintf(&b, "> ⚠️ %s\n", rep.DataNote)
		return b.String()
	}

	pc := rep.PrevPeriodComparison
	fmt.Fprintf(&b, "**vs previous period:** %s (%+.1f%%) — previous %s\n\n", signedUSD(pc.DeltaUSD), pc.PctChange, usd(pc.PrevTotalUSD))

	b.WriteString("## Spend by team\n\n")
	if len(rep.SpendByTeam) == 0 {
		b.WriteString("_No team-attributed spend._\n\n")
	} else {
		b.WriteString("| Team | Cost | % | Δ vs prev |\n|---|---|---|---|\n")
		for _, t := range rep.SpendByTeam {
			fmt.Fprintf(&b, "| %s | %s | %.1f%% | %s |\n", t.Team, usd(t.CostUSD), t.Pct, signedUSD(t.DeltaVsPrevUSD))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Spend by feature (issue)\n\n")
	if len(rep.SpendByFeature) == 0 {
		b.WriteString("_No issue-attributed spend._\n\n")
	} else {
		b.WriteString("| Issue | Cost | % |\n|---|---|---|\n")
		for _, f := range rep.SpendByFeature {
			fmt.Fprintf(&b, "| %s | %s | %.1f%% |\n", f.IssueID, usd(f.CostUSD), f.Pct)
		}
		b.WriteString("\n")
	}

	if rep.EngineerBreakdownEnabled {
		b.WriteString("## Spend by engineer\n\n")
		b.WriteString("_Cost attribution (which work a dollar landed against), not a performance judgment._\n\n")
		b.WriteString("| Engineer | Cost | Requests |\n|---|---|---|\n")
		for _, e := range rep.SpendByEngineer {
			fmt.Fprintf(&b, "| %s | %s | %d |\n", e.Author, usd(e.CostUSD), e.Requests)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Cost per feature — trend\n\n")
	b.WriteString("| Month | Avg cost / issue | Issues |\n|---|---|---|\n")
	for _, p := range rep.CostPerFeatureTrend {
		fmt.Fprintf(&b, "| %s | %s | %d |\n", p.Period, usd(p.AvgCostUSD), p.FeatureCount)
	}
	b.WriteString("\n")

	b.WriteString("## Budget status\n\n")
	if len(rep.BudgetStatus) == 0 {
		b.WriteString("_No budgets configured._\n\n")
	} else {
		b.WriteString("| Scope | ID | Spent / Limit | Util | Status |\n|---|---|---|---|---|\n")
		for _, s := range rep.BudgetStatus {
			util := "—"
			if s.LimitUSD > 0 {
				util = fmt.Sprintf("%.0f%%", s.Utilization*100)
			}
			fmt.Fprintf(&b, "| %s | %s | %s / %s | %s | %s |\n", s.Scope, s.ScopeID, usd(s.SpentUSD), usd(s.LimitUSD), util, s.Status)
		}
		b.WriteString("\n")
	}

	fs := rep.ForecastSummary
	b.WriteString("## Forward forecast\n\n")
	if fs.InsufficientData {
		fmt.Fprintf(&b, "_Projection unavailable: %s_\n\n", fs.ConfidenceNote)
	} else {
		fmt.Fprintf(&b, "Projected period total: **≈ %s** (a projection, not a guarantee)  \n", usd(fs.ProjectedTotalUSD))
		if fs.LimitUSD > 0 {
			verdict := "within budget"
			if fs.WillExceed {
				verdict = fmt.Sprintf("projected to EXCEED budget by %s", usd(fs.ProjectedOverageUSD))
			}
			fmt.Fprintf(&b, "vs budget %s: %s  \n", usd(fs.LimitUSD), verdict)
		}
		fmt.Fprintf(&b, "_%s_\n\n", fs.ConfidenceNote)
	}

	b.WriteString("## Top cost outliers\n\n")
	if len(rep.Anomalies) == 0 {
		b.WriteString("_None flagged (or baseline too small to judge)._\n\n")
	} else {
		b.WriteString("_Statistical flags, not verdicts._\n\n")
		b.WriteString("| Unit | Cost | × median | Severity |\n|---|---|---|---|\n")
		for _, a := range rep.Anomalies {
			fmt.Fprintf(&b, "| %s | %s | %.1f× | %s |\n", a.UnitID, usd(a.CostUSD), a.Factor, a.Severity)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func usd(v float64) string       { return fmt.Sprintf("$%.2f", v) }
func signedUSD(v float64) string { return fmt.Sprintf("%+.2f", v) } //nolint:gocritic — sign is intentional

// htmlReportTmpl is a self-contained, printable HTML document — inline CSS,
// no external assets, print-to-PDF-able by the recipient. It wears the brand
// (internal/brand): dark on screen unless the viewer prefers light, light on
// paper, the flat mark beside the title, IBM Plex Mono for every figure.
var htmlReportTmpl = template.Must(template.New("roi").Funcs(template.FuncMap{
	"usd":    func(v float64) string { return fmt.Sprintf("$%.2f", v) },
	"pct":    func(v float64) string { return fmt.Sprintf("%.1f%%", v) },
	"signed": func(v float64) string { return fmt.Sprintf("%+.2f", v) },
	"util": func(limit, u float64) string {
		if limit <= 0 {
			return "—"
		}
		return fmt.Sprintf("%.0f%%", u*100)
	},
}).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Executive AI-Cost Report — {{.WorkspaceID}}</title>
<style>
` + brand.TokensCSS + brand.PrintCSS + `
  *{box-sizing:border-box}
  body{margin:0;padding:40px 24px;background:var(--tv-canvas);color:var(--tv-ink);
       font-family:var(--tv-font-sans);font-size:15px;line-height:24px;-webkit-font-smoothing:antialiased}
  main{max-width:880px;margin:0 auto}
  .num{font-variant-numeric:tabular-nums}
  header{display:flex;align-items:flex-start;gap:16px;margin-bottom:24px}
  header .tv-mark{width:40px;height:40px;margin-top:4px}
  .eyebrow{font-family:var(--tv-font-sans);font-size:12px;line-height:16px;font-weight:600;letter-spacing:.04em;text-transform:uppercase;color:var(--tv-label)}
  h1{margin:4px 0 6px;font-size:30px;line-height:34px;font-weight:750;letter-spacing:-.015em}
  header .tv-rule{margin-top:14px}
  .meta{color:var(--tv-ink-muted);font-size:13px;line-height:20px}
  .meta .num{white-space:nowrap}
  section{background:var(--tv-surface);border:1px solid var(--tv-line);border-radius:var(--tv-radius-md);
          padding:18px 20px;margin-bottom:16px;break-inside:avoid}
  section h2{margin:0 0 8px}
  .headline{background:var(--tv-raised)}
  .total{font-family:var(--tv-font-display);font-variant-numeric:tabular-nums;font-size:40px;line-height:44px;
         font-weight:750;letter-spacing:-.03em;margin:6px 0 4px}
  table{border-collapse:collapse;width:100%;font-size:14px;line-height:20px}
  th,td{text-align:left;padding:9px 8px;border-bottom:1px solid var(--tv-line);vertical-align:top}
  tr:last-child td{border-bottom:0}
  th{color:var(--tv-label);font-weight:600}
  td.num{text-align:right;font-variant-numeric:tabular-nums}
  th.num{text-align:right}
  td.id{font-family:var(--tv-font-mono);overflow-wrap:anywhere}
  .proj{font-variant-numeric:tabular-nums;font-weight:600}
  .note{color:var(--tv-ink-muted);font-size:13px;line-height:20px;font-style:italic;margin:0 0 4px}
  td.note{text-align:left}
  .pill{display:inline-flex;align-items:center;gap:7px;white-space:nowrap}
  .pill::before{content:"";width:7px;height:7px;flex:none;border-radius:var(--tv-radius-pill);background:var(--tv-ink-muted)}
  .pill.ok::before{background:var(--tv-positive)}
  .pill.warn::before{background:var(--tv-caution)}
  .pill.over::before,.pill.high::before{background:var(--tv-critical)}
  .banner{display:flex;align-items:center;gap:12px;margin:0;font-weight:500}
  .banner::before{content:"";width:10px;height:10px;flex:none;border-radius:var(--tv-radius-pill);background:var(--tv-caution)}
  footer{color:var(--tv-ink-muted);font-size:13px;line-height:20px;margin-top:28px}
  @media (max-width:600px){
    body{padding:28px 16px}
    h1{font-size:24px;line-height:30px}
    header .tv-mark{width:32px;height:32px}
    section{padding:14px 12px}
    .total{font-size:28px;line-height:34px}
    table{font-size:13px}
    th,td{padding:8px 5px}
    td.split span{display:block}
  }
  @page{margin:16mm}
  @media print{
    body{padding:0}
    section{border-color:var(--tv-line-strong)}
  }
</style></head><body>
<main>
<header>
  ` + brand.Mark + `
  <div>
    <div class="eyebrow">Talyvor Lens · ROI report</div>
    <h1>Executive AI-Cost Report</h1>
    <div class="meta"><span class="num">{{.WorkspaceID}}</span> · {{.Period}} · <span class="num">{{.PeriodStart.Format "2006-01-02"}} → {{.PeriodEnd.Format "2006-01-02"}}</span> · generated <span class="num">{{.GeneratedAt.Format "2006-01-02 15:04 MST"}}</span></div>
    <span class="tv-rule"></span>
  </div>
</header>

<section class="headline">
  <h2 class="eyebrow">Total AI spend</h2>
  <div class="total">{{usd .TotalSpendUSD}}</div>
{{if .InsufficientData}}
  <p class="banner">{{.DataNote}}</p>
</section>
{{else}}
  <div class="meta">vs previous period: <span class="num">{{signed .PrevPeriodComparison.DeltaUSD}} ({{printf "%+.1f%%" .PrevPeriodComparison.PctChange}})</span> — previous <span class="num">{{usd .PrevPeriodComparison.PrevTotalUSD}}</span></div>
</section>

<section>
<h2 class="eyebrow">Spend by team</h2>
<table><tr><th>Team</th><th class="num">Cost</th><th class="num">%</th><th class="num">Δ vs prev</th></tr>
{{range .SpendByTeam}}<tr><td>{{.Team}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{pct .Pct}}</td><td class="num">{{signed .DeltaVsPrevUSD}}</td></tr>{{else}}<tr><td colspan="4" class="note">No team-attributed spend.</td></tr>{{end}}
</table>
</section>

<section>
<h2 class="eyebrow">Spend by feature (issue)</h2>
<table><tr><th>Issue</th><th class="num">Cost</th><th class="num">%</th></tr>
{{range .SpendByFeature}}<tr><td class="id">{{.IssueID}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{pct .Pct}}</td></tr>{{else}}<tr><td colspan="3" class="note">No issue-attributed spend.</td></tr>{{end}}
</table>
</section>

{{if .EngineerBreakdownEnabled}}
<section>
<h2 class="eyebrow">Spend by engineer</h2>
<p class="note">Cost attribution (which work a dollar landed against), not a performance judgment.</p>
<table><tr><th>Engineer</th><th class="num">Cost</th><th class="num">Requests</th></tr>
{{range .SpendByEngineer}}<tr><td>{{.Author}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{.Requests}}</td></tr>{{end}}
</table>
</section>
{{end}}

<section>
<h2 class="eyebrow">Cost per feature — trend</h2>
<table><tr><th>Month</th><th class="num">Avg cost / issue</th><th class="num">Issues</th></tr>
{{range .CostPerFeatureTrend}}<tr><td class="id">{{.Period}}</td><td class="num">{{usd .AvgCostUSD}}</td><td class="num">{{.FeatureCount}}</td></tr>{{end}}
</table>
</section>

<section>
<h2 class="eyebrow">Budget status</h2>
<table><tr><th>Scope</th><th>ID</th><th class="num">Spent / Limit</th><th class="num">Util</th><th>Status</th></tr>
{{range .BudgetStatus}}<tr><td>{{.Scope}}</td><td class="id">{{.ScopeID}}</td><td class="num split"><span>{{usd .SpentUSD}} /</span> <span>{{usd .LimitUSD}}</span></td><td class="num">{{util .LimitUSD .Utilization}}</td><td><span class="pill {{.Status}}">{{.Status}}</span></td></tr>{{else}}<tr><td colspan="5" class="note">No budgets configured.</td></tr>{{end}}
</table>
</section>

<section>
<h2 class="eyebrow">Forward forecast</h2>
{{with .ForecastSummary}}
  {{if .InsufficientData}}
    <p class="note">Projection unavailable: {{.ConfidenceNote}}</p>
  {{else}}
    <p><span class="proj">≈ {{usd .ProjectedTotalUSD}}</span> projected period total (a projection, not a guarantee).
    {{if gt .LimitUSD 0.0}} vs budget <span class="num">{{usd .LimitUSD}}</span>: {{if .WillExceed}}<span class="pill over">projected to exceed by <span class="num">{{usd .ProjectedOverageUSD}}</span></span>{{else}}<span class="pill ok">within budget</span>{{end}}{{end}}</p>
    <p class="note">{{.ConfidenceNote}}</p>
  {{end}}
{{end}}
</section>

<section>
<h2 class="eyebrow">Top cost outliers</h2>
<p class="note">Statistical flags, not verdicts — a high multiple of the median is a fact, not a judgment that anything is wrong.</p>
<table><tr><th>Unit</th><th class="num">Cost</th><th class="num">× median</th><th>Severity</th></tr>
{{range .Anomalies}}<tr><td class="id">{{.UnitID}}</td><td class="num">{{usd .CostUSD}}</td><td class="num">{{printf "%.1f×" .Factor}}</td><td><span class="pill {{.Severity}}">{{.Severity}}</span></td></tr>{{else}}<tr><td colspan="4" class="note">None flagged (or baseline too small to judge).</td></tr>{{end}}
</table>
</section>
{{end}}
<footer>Print to PDF to share. Figures are read-only aggregations of recorded AI spend; projections and statistical flags are labeled as such.</footer>
</main>
</body></html>`))

// RenderHTML renders the self-contained printable HTML report.
func RenderHTML(rep ExecReport) (string, error) {
	var buf bytes.Buffer
	if err := htmlReportTmpl.Execute(&buf, rep); err != nil {
		return "", err
	}
	return buf.String(), nil
}
