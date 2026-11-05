package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Scan's models (B52.24): what is unusual in a series and how sure we are. A series' normal is the mean of its points
// near the same date in each of the past five years, which takes out the yearly cycle; a point's anomaly is its
// distance from that normal in standard deviations; and the outlook is the normal a few steps ahead plus today's
// anomaly times its fitted persistence, with the spread of the model's past misses as its range. Every model is
// backtested on the series' own history before use, and Assess refuses a series whose latest backtest failed, so a
// failing model never feeds a report. Every figure Assess gives is stored as a derived point with its inputs.

// Family is a set of series and the model written down for them.
type Family struct {
	Prefix, Suffix string        // a series of the family starts with Prefix and ends with Suffix
	Step           time.Duration // how far apart the series' points are
	Horizon        int           // how many steps ahead the outlook reaches
	Window         time.Duration // the normal reads past years' points this far either side of the same date
	Why            string
}

const (
	oneDay  = 24 * time.Hour
	oneWeek = 7 * oneDay
)

// Families are the series Scan models, each with why the model suits it.
var Families = []Family{
	{Prefix: "eia.stocks.", Step: oneWeek, Horizon: 4, Window: 10 * oneDay,
		Why: "US oil stocks follow a yearly cycle of refinery runs and driving season, and a surplus or deficit against " +
			"it builds and drains over weeks, so this week's gap to the normal says something about the gap in four weeks."},
	{Prefix: "gie.agsi.", Suffix: ".full", Step: oneDay, Horizon: 14, Window: 7 * oneDay,
		Why: "EU storage fills from April and empties from October; how far ahead of or behind the usual fill a country " +
			"is changes slowly, because injection capacity is fixed, so it carries two weeks ahead."},
	{Prefix: "entsog.flow.", Step: oneDay, Horizon: 7, Window: 7 * oneDay,
		Why: "Pipeline flows into the EU follow the heating season, and a corridor running above or below its usual " +
			"level tends to stay there for days (contracts, maintenance), so it carries a week ahead."},
	{Prefix: "crop.nass.", Step: oneWeek, Horizon: 2, Window: 10 * oneDay,
		Why: "Crop-condition shares move with the season and with the weather of the last weeks, which changes slowly, " +
			"so a crop rated better or worse than usual for the week tends to stay so for the next two."},
}

// Unmodelled are the series Scan stores without a model, and why.
var Unmodelled = []struct {
	Prefix string `json:"prefix"`
	Why    string `json:"why"`
}{
	{"ecmwf.", "already a forecast: Scan shows it as ECMWF made it"},
	{"crop.wasde.", "monthly official projections: too few points a year for a backtest"},
	{"crop.mars.", "monthly official yield forecasts: too few points a year for a backtest"},
	{"crop.ndvi.", "already read against its own five-year normal (B52.22); clouds leave gaps a fixed-step outlook cannot use"},
	{"sec.", "filings are events, not levels: a buying cluster is reported as found, without a normal"},
	{"usaspending.", "contract awards are events, not levels"},
	{"tanks.", "a pilot with its own written-down backtest (B52.29, pilot/cushing.plan.txt): stored as " + TankSeries +
		"'s backtest, and kept out of reports unless it passed"},
	{"jets.", "a beta behind a switch that stays off (B52.30): counts of company flights, not yet modelled"},
	{"gie.", "only each country's fill (gie.agsi.<country>.full) is modelled; the other GIE figures follow from it"},
}

// What a model must show in its backtest before it is used, written down before any backtest ran.
const (
	// ModelID names the model; a backtest row records it, so a later model never inherits this one's verdict.
	ModelID = "seasonal-normal-ar1/v1"
	// minTrain is how many past outcomes a fit needs before its outlook is tested.
	minTrain = 20
	// minTested is how many independent outlooks (overlapping ones count as one) a backtest needs to judge a model.
	minTested = 10
	// rangeLow and rangeHigh are the percentiles of past misses an outlook's range spans: an 80% range.
	rangeLow, rangeHigh = 0.10, 0.90
)

// Report is one backtest: how the model did on the series' own history, and the parameters it uses from now on.
type Report struct {
	Series      string    `json:"series"`
	Model       string    `json:"model"`
	Method      string    `json:"method"`
	RanAt       time.Time `json:"ran_at"`
	DataFrom    time.Time `json:"data_from"`
	DataTo      time.Time `json:"data_to"`
	Tested      int       `json:"tested"`       // outlooks checked against what happened
	Independent float64   `json:"independent"`  // Tested, with overlapping outlooks counted once
	HitRate     float64   `json:"hit_rate"`     // share whose side of the normal was right
	Error       float64   `json:"error"`        // mean absolute error of the outlook's middle
	NormalError float64   `json:"normal_error"` // the same, had the outlook been the normal alone
	Coverage    float64   `json:"coverage"`     // share of outcomes inside the 80% range
	Passed      bool      `json:"passed"`
	Verdict     string    `json:"verdict"`
	Persistence float64   `json:"persistence"` // the share of today's anomaly expected Horizon steps ahead
	MissLow     float64   `json:"miss_low"`    // the 10th percentile of past misses
	MissHigh    float64   `json:"miss_high"`   // the 90th
	Finding     *Finding  `json:"finding,omitempty"`
}

// Finding is the latest point of a series against its normal, and its outlook. Points names the stored derived
// point behind every figure, so a report can trace each one to its inputs and their sources.
type Finding struct {
	At          time.Time        `json:"at"`
	Value       float64          `json:"value"`
	Unit        string           `json:"unit"`
	NormalLow   float64          `json:"normal_low"`  // the normal less one standard deviation
	NormalHigh  float64          `json:"normal_high"` // and plus one
	Z           float64          `json:"z"`           // the anomaly, in standard deviations
	OutlookAt   time.Time        `json:"outlook_at"`
	OutlookLow  float64          `json:"outlook_low"`
	OutlookHigh float64          `json:"outlook_high"`
	Points      map[string]int64 `json:"points"`
}

var (
	ErrNoModel      = errors.New("scan: no model is written down for this series")
	ErrNotTested    = errors.New("scan: the series' model has not been backtested")
	ErrModelRefused = errors.New("scan: the series' model failed its backtest")
	ErrNoNormal     = errors.New("scan: not enough past years for a normal")
)

// FamilyOf is the family a series belongs to.
func FamilyOf(series string) (Family, bool) {
	for _, f := range Families {
		if len(series) > len(f.Prefix)+len(f.Suffix) && series[:len(f.Prefix)] == f.Prefix &&
			series[len(series)-len(f.Suffix):] == f.Suffix {
			return f, true
		}
	}
	return Family{}, false
}

// Method is the model, as a report states it.
func (f Family) Method() string {
	return fmt.Sprintf("Normal: the mean of the series' points within %d days of the same date in each of the past %d "+
		"years (%d at least), which takes out the yearly cycle; the anomaly is the distance from it in standard deviations. "+
		"Outlook %s ahead: the normal then, plus today's anomaly times its persistence fitted on the series' past, with "+
		"the 10th to 90th percentile of the fit's past misses as its range. %s",
		int(f.Window/oneDay), normalYears, normalMinYears, ahead(f), f.Why)
}

func ahead(f Family) string {
	if f.Step == oneWeek {
		return fmt.Sprintf("%d weeks", f.Horizon)
	}
	return fmt.Sprintf("%d days", int(time.Duration(f.Horizon)*f.Step/oneDay))
}

// normalAt is the mean and standard deviation of pts within window of the same date in each of the past normalYears
// years, with the points it read; ok is false with fewer than normalMinYears years or no spread. pts is in time order.
func normalAt(pts []storedPoint, at time.Time, window time.Duration) (mean, sd float64, inputs []int64, ok bool) {
	var vals []float64
	years := 0
	for back := 1; back <= normalYears; back++ {
		same := at.AddDate(-back, 0, 0)
		i := sort.Search(len(pts), func(i int) bool { return !pts[i].at.Before(same.Add(-window)) })
		found := false
		for ; i < len(pts) && !pts[i].at.After(same.Add(window)); i++ {
			vals, inputs, found = append(vals, pts[i].value), append(inputs, pts[i].id), true
		}
		if found {
			years++
		}
	}
	if years < normalMinYears || len(vals) < 2 {
		return 0, 0, nil, false
	}
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	for _, v := range vals {
		sd += (v - mean) * (v - mean)
	}
	sd = math.Sqrt(sd / float64(len(vals)-1))
	return mean, sd, inputs, sd > 0
}

// pointAt is the index of the point within half a step of at, or -1.
func pointAt(pts []storedPoint, at time.Time, step time.Duration) int {
	i := sort.Search(len(pts), func(i int) bool { return !pts[i].at.Before(at.Add(-step / 2)) })
	if i < len(pts) && pts[i].at.Before(at.Add(step/2)) {
		return i
	}
	return -1
}

// pair is one outlook the model could have made: from an origin point to the point Horizon steps later.
type pair struct {
	origin, target     time.Time
	anomaly, outcome   float64 // the anomaly at the origin and at the target
	targetNormal, real float64 // the normal at the target and the value there
}

func pairs(f Family, pts []storedPoint) []pair {
	var out []pair
	for i, p := range pts {
		mean, _, _, ok := normalAt(pts, p.at, f.Window)
		if !ok {
			continue
		}
		j := pointAt(pts, p.at.Add(time.Duration(f.Horizon)*f.Step), f.Step)
		if j <= i {
			continue
		}
		tmean, _, _, ok := normalAt(pts, pts[j].at, f.Window)
		if !ok {
			continue
		}
		out = append(out, pair{origin: p.at, target: pts[j].at, anomaly: p.value - mean, outcome: pts[j].value - tmean,
			targetNormal: tmean, real: pts[j].value})
	}
	return out
}

// fit is the persistence (least squares through the origin, anomaly ahead on anomaly now) and the 10th and 90th
// percentiles of its misses.
func fit(ps []pair) (phi, lo, hi float64) {
	var sxy, sxx float64
	for _, p := range ps {
		sxy += p.anomaly * p.outcome
		sxx += p.anomaly * p.anomaly
	}
	if sxx > 0 {
		phi = sxy / sxx
	}
	misses := make([]float64, len(ps))
	for i, p := range ps {
		misses[i] = p.outcome - phi*p.anomaly
	}
	return phi, quantile(misses, rangeLow), quantile(misses, rangeHigh)
}

func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	i := int(pos)
	if i+1 >= len(s) {
		return s[i]
	}
	return s[i] + (pos-float64(i))*(s[i+1]-s[i])
}

// Backtest walks the series forward: at each origin it fits the model only on outcomes already known then, makes the
// outlook, and checks it against what happened. The model passes when, over at least minTested independent outlooks,
// it called the side of the normal better than a coin, its error is smaller than the normal's alone, and its 80% range
// held 80% of outcomes, within two standard errors.
func Backtest(f Family, series string, pts []storedPoint) Report {
	r := Report{Series: series, Model: ModelID, Method: f.Method()}
	if len(pts) > 0 {
		r.DataFrom, r.DataTo = pts[0].at, pts[len(pts)-1].at
	}
	all := pairs(f, pts)
	var hits, covered int
	var errSum, normalSum float64
	known := 0
	for _, p := range all {
		for known < len(all) && !all[known].target.After(p.origin) {
			known++
		}
		if known < minTrain {
			continue
		}
		phi, lo, hi := fit(all[:known])
		mid := p.targetNormal + phi*p.anomaly
		r.Tested++
		if phi*p.anomaly*p.outcome > 0 {
			hits++
		}
		if p.real >= mid+lo && p.real <= mid+hi {
			covered++
		}
		errSum += math.Abs(p.real - mid)
		normalSum += math.Abs(p.outcome)
	}
	r.Persistence, r.MissLow, r.MissHigh = fit(all)
	r.Independent = float64(r.Tested) / float64(f.Horizon)
	if r.Independent < minTested {
		r.Verdict = fmt.Sprintf("not enough history: %.0f independent outlooks to check, the model needs %d", r.Independent, minTested)
		return r
	}
	n := float64(r.Tested)
	r.HitRate, r.Error, r.NormalError, r.Coverage = float64(hits)/n, errSum/n, normalSum/n, float64(covered)/n
	slack := 2 * math.Sqrt(0.8*0.2/r.Independent)
	switch {
	case r.HitRate <= 0.5:
		r.Verdict = fmt.Sprintf("failed: it called the side of the normal %.0f%% of the time, no better than a coin", 100*r.HitRate)
	case r.Error >= r.NormalError:
		r.Verdict = fmt.Sprintf("failed: its error (%.4g) is no smaller than the normal's alone (%.4g)", r.Error, r.NormalError)
	case math.Abs(r.Coverage-0.8) > slack:
		r.Verdict = fmt.Sprintf("failed: its 80%% range held %.0f%% of outcomes, outside the %.0f–%.0f%% a calibrated range gives",
			100*r.Coverage, 100*(0.8-slack), 100*math.Min(1, 0.8+slack))
	default:
		r.Passed = true
		r.Verdict = fmt.Sprintf("passed: %d outlooks, side of the normal right %.0f%% of the time, error %.4g against the normal's %.4g, "+
			"80%% range held %.0f%%", r.Tested, 100*r.HitRate, r.Error, r.NormalError, 100*r.Coverage)
	}
	return r
}

// Models backtests every modelled series and assesses those whose model passed.
type Models struct{ store *Store }

func NewModels(store *Store) *Models { return &Models{store: store} }

func (m *Models) points(ctx context.Context, series string) ([]storedPoint, string, error) {
	rows, err := m.store.pool.Query(ctx, `SELECT id, observed_at, value, unit FROM scan_points WHERE kind = 'observed'
		AND series = $1 ORDER BY observed_at, id`, series)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var pts []storedPoint
	var unit string
	for rows.Next() {
		var p storedPoint
		if err := rows.Scan(&p.id, &p.at, &p.value, &unit); err != nil {
			return nil, "", err
		}
		pts = append(pts, p)
	}
	return pts, unit, rows.Err()
}

// Run backtests every series of every family and assesses each that passed, and stores the tank pilot's backtest.
// One failing is logged; the rest still run.
func (m *Models) Run(ctx context.Context) {
	if _, err := m.RunTankPilot(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("scan: tank pilot backtest failed", slog.String("err", err.Error()))
	}
	for _, f := range Families {
		rows, err := m.store.pool.Query(ctx, `SELECT DISTINCT series FROM scan_points WHERE kind = 'observed'
			AND starts_with(series, $1) AND right(series, length($2)) = $2`, f.Prefix, f.Suffix)
		if err != nil {
			slog.Warn("scan: listing series to model failed", slog.String("err", err.Error()))
			return
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			slog.Warn("scan: listing series to model failed", slog.String("err", err.Error()))
			return
		}
		for _, s := range list {
			if _, err := m.RunSeries(ctx, s); err != nil && ctx.Err() == nil {
				slog.Warn("scan: model run failed", slog.String("series", s), slog.String("err", err.Error()))
			}
		}
	}
}

// RunSeries backtests one series, stores the report and, when the model passed, assesses the latest point.
func (m *Models) RunSeries(ctx context.Context, series string) (Report, error) {
	f, ok := FamilyOf(series)
	if !ok {
		return Report{}, ErrNoModel
	}
	pts, _, err := m.points(ctx, series)
	if err != nil {
		return Report{}, err
	}
	r := Backtest(f, series, pts)
	var id int64
	if err := m.store.pool.QueryRow(ctx, `INSERT INTO scan_backtests (series, model, method, data_from, data_to, tested,
		independent, hit_rate, error, normal_error, coverage, passed, verdict, persistence, miss_low, miss_high)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id, ran_at`,
		r.Series, r.Model, r.Method, nullTime(r.DataFrom), nullTime(r.DataTo), r.Tested, r.Independent, r.HitRate,
		r.Error, r.NormalError, r.Coverage, r.Passed, r.Verdict, r.Persistence, r.MissLow, r.MissHigh).Scan(&id, &r.RanAt); err != nil {
		return r, err
	}
	if !r.Passed {
		return r, nil
	}
	fd, err := m.Assess(ctx, series)
	if err != nil {
		return r, err
	}
	b, _ := json.Marshal(fd)
	_, err = m.store.pool.Exec(ctx, `UPDATE scan_backtests SET finding = $2 WHERE id = $1`, id, string(b))
	r.Finding = &fd
	return r, err
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Assess reads the series' latest point against its normal and makes its outlook, storing every figure as a derived
// point with its inputs. It refuses a series with no model, no backtest, or whose latest backtest of this model failed.
func (m *Models) Assess(ctx context.Context, series string) (Finding, error) {
	f, ok := FamilyOf(series)
	if !ok {
		return Finding{}, ErrNoModel
	}
	var passed bool
	var verdict string
	var phi, lo, hi float64
	err := m.store.pool.QueryRow(ctx, `SELECT passed, verdict, persistence, miss_low, miss_high FROM scan_backtests
		WHERE series = $1 AND model = $2 ORDER BY ran_at DESC, id DESC LIMIT 1`, series, ModelID).
		Scan(&passed, &verdict, &phi, &lo, &hi)
	if errors.Is(err, pgx.ErrNoRows) {
		return Finding{}, ErrNotTested
	}
	if err != nil {
		return Finding{}, err
	}
	if !passed {
		return Finding{}, fmt.Errorf("%w: %s", ErrModelRefused, verdict)
	}
	pts, unit, err := m.points(ctx, series)
	if err != nil {
		return Finding{}, err
	}
	if len(pts) == 0 {
		return Finding{}, ErrNoNormal
	}
	last := pts[len(pts)-1]
	mean, sd, inputs, ok := normalAt(pts, last.at, f.Window)
	if !ok {
		return Finding{}, ErrNoNormal
	}
	target := last.at.Add(time.Duration(f.Horizon) * f.Step)
	tmean, _, tinputs, ok := normalAt(pts, target, f.Window)
	if !ok {
		return Finding{}, ErrNoNormal
	}
	mid := tmean + phi*(last.value-mean)
	fd := Finding{At: last.at, Value: last.value, Unit: unit, NormalLow: mean - sd, NormalHigh: mean + sd,
		Z: (last.value - mean) / sd, OutlookAt: target, OutlookLow: mid + lo, OutlookHigh: mid + hi, Points: map[string]int64{}}
	normal := fmt.Sprintf("normal: mean ± one standard deviation of %d points within %d days of the same date in past years",
		len(inputs), int(f.Window/oneDay))
	outlook := func(q string) string {
		return fmt.Sprintf("outlook (%s): normal at %s plus %.4g × today's anomaly, plus the %s of past misses",
			ModelID, target.Format(time.DateOnly), phi, q)
	}
	withLast := append([]int64{last.id}, inputs...)
	both := append(append([]int64{}, withLast...), tinputs...)
	for _, d := range []struct {
		name, unit string
		at         time.Time
		value      float64
		how        string
		in         []int64
	}{
		{"normal_low", unit, last.at, fd.NormalLow, normal + ", low end", inputs},
		{"normal_high", unit, last.at, fd.NormalHigh, normal + ", high end", inputs},
		{"z", "standard deviations", last.at, fd.Z, "anomaly: (value − normal) ÷ standard deviation; " + normal, withLast},
		{"outlook_low", unit, target, fd.OutlookLow, outlook("10th percentile"), both},
		{"outlook_high", unit, target, fd.OutlookHigh, outlook("90th percentile"), both},
	} {
		id, err := m.deriveOnce(ctx, Derivation{Series: series + ".model." + d.name, ObservedAt: d.at, Value: d.value,
			Unit: d.unit, Transforms: []string{d.how}, Inputs: d.in})
		if err != nil {
			return Finding{}, fmt.Errorf("%s: %w", d.name, err)
		}
		fd.Points[d.name] = id
	}
	return fd, nil
}

// deriveOnce stores a derived figure unless the same figure is already stored for that series and time.
func (m *Models) deriveOnce(ctx context.Context, d Derivation) (int64, error) {
	var id int64
	err := m.store.pool.QueryRow(ctx, `SELECT id FROM scan_points WHERE kind = 'derived' AND series = $1
		AND observed_at = $2 AND value = $3 ORDER BY id LIMIT 1`, d.Series, d.ObservedAt, d.Value).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return m.store.Derive(ctx, d)
	}
	return id, err
}

// Reports is the latest backtest of every series, with its finding when the model passed.
func (m *Models) Reports(ctx context.Context) ([]Report, error) {
	rows, err := m.store.pool.Query(ctx, `SELECT DISTINCT ON (series) series, model, method, ran_at,
		coalesce(data_from, ran_at), coalesce(data_to, ran_at), tested, independent, hit_rate, error, normal_error,
		coverage, passed, verdict, persistence, miss_low, miss_high, finding
		FROM scan_backtests ORDER BY series, ran_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		var r Report
		var finding []byte
		if err := rows.Scan(&r.Series, &r.Model, &r.Method, &r.RanAt, &r.DataFrom, &r.DataTo, &r.Tested, &r.Independent,
			&r.HitRate, &r.Error, &r.NormalError, &r.Coverage, &r.Passed, &r.Verdict, &r.Persistence, &r.MissLow,
			&r.MissHigh, &finding); err != nil {
			return nil, err
		}
		if finding != nil {
			r.Finding = &Finding{}
			if err := json.Unmarshal(finding, r.Finding); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Every runs the models now and then once every interval until ctx ends.
func (m *Models) Every(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		m.Run(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
