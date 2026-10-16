package anomaly

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// hourlyRows builds a pgxmock rows mock for the hourly-aggregation
// query. Each entry produces one (hour, cost) row.
func hourlyRows(costs []float64) *pgxmock.Rows {
	r := pgxmock.NewRows([]string{"hour", "hourly_cost"})
	base := time.Now().UTC().Add(-time.Duration(len(costs)) * time.Hour)
	for i, c := range costs {
		r.AddRow(base.Add(time.Duration(i)*time.Hour), c)
	}
	return r
}

func TestComputeWindowStats_ReturnsCorrectMeanAndStdDev(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`date_trunc.*hour`).
		WithArgs("ws-1", "", "", "", 7).
		WillReturnRows(hourlyRows([]float64{1.0, 2.0, 3.0, 4.0}))

	d := newDetector(pool)
	stats, err := d.ComputeWindowStats(context.Background(), "ws-1", "", "", "", 7)
	if err != nil {
		t.Fatalf("ComputeWindowStats: %v", err)
	}
	if stats == nil {
		t.Fatal("stats must not be nil when rows exist")
	}
	if stats.Count != 4 {
		t.Errorf("Count = %d, want 4", stats.Count)
	}
	if math.Abs(stats.Mean-2.5) > 1e-9 {
		t.Errorf("Mean = %v, want 2.5", stats.Mean)
	}
	// Population stddev of [1,2,3,4] = sqrt(((1.5²+0.5²+0.5²+1.5²)/4)) = sqrt(1.25)
	wantStd := math.Sqrt(1.25)
	if math.Abs(stats.StdDev-wantStd) > 1e-9 {
		t.Errorf("StdDev = %v, want %v", stats.StdDev, wantStd)
	}
	if stats.Min != 1.0 || stats.Max != 4.0 {
		t.Errorf("Min/Max = %v/%v, want 1/4", stats.Min, stats.Max)
	}
}

func TestComputeWindowStats_ReturnsNilForInsufficientData(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`date_trunc.*hour`).
		WithArgs("ws-1", "", "", "", 7).
		WillReturnRows(hourlyRows(nil))

	d := newDetector(pool)
	stats, err := d.ComputeWindowStats(context.Background(), "ws-1", "", "", "", 7)
	if err != nil {
		t.Fatalf("ComputeWindowStats: %v", err)
	}
	if stats != nil {
		t.Errorf("stats = %+v, want nil when no rows", stats)
	}
}

// detectMocks sets up the three queries Detect issues: window-7, current-hour, window-1.
// weekCosts/dayCosts populate the hourly windows; currentHour is the current-hour scalar.
func detectMocks(t *testing.T, weekCosts, dayCosts []float64, currentHour float64) pgxmock.PgxPoolIface {
	t.Helper()
	pool := newPool(t)
	pool.ExpectQuery(`date_trunc.*hour`).
		WithArgs("ws", "", "", "", 7).
		WillReturnRows(hourlyRows(weekCosts))
	pool.ExpectQuery(`SUM\(cost_usd\)`).
		WithArgs("ws", "", "", "").
		WillReturnRows(pgxmock.NewRows([]string{"current_cost"}).AddRow(currentHour))
	pool.ExpectQuery(`date_trunc.*hour`).
		WithArgs("ws", "", "", "", 1).
		WillReturnRows(hourlyRows(dayCosts))
	return pool
}

// makeWeekCosts returns N hourly cost samples with mean ≈ wantMean and stddev ≈ wantStd.
// Two-point pattern of (mean-std) and (mean+std) repeats — exactly produces those moments.
func makeWeekCosts(n int, mean, std float64) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			out[i] = mean - std
		} else {
			out[i] = mean + std
		}
	}
	return out
}

func TestDetect_ReturnsSpikeWhenZAboveThree(t *testing.T) {
	weekCosts := makeWeekCosts(48, 1.0, 0.5) // mean=1.0, std=0.5
	dayCosts := makeWeekCosts(24, 1.0, 0.5)  // trend baseline matches
	pool := detectMocks(t, weekCosts, dayCosts, 5.0)
	d := newDetector(pool)

	anoms, err := d.Detect(context.Background(), "ws", "", "", "")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(anoms) == 0 {
		t.Fatal("expected at least one anomaly")
	}
	var spike *Anomaly
	for i := range anoms {
		if anoms[i].Type == AnomalySpike {
			spike = &anoms[i]
		}
	}
	if spike == nil {
		t.Fatalf("expected spike anomaly; got %+v", anoms)
	}
	if spike.DeviationSigma < 3.0 {
		t.Errorf("DeviationSigma = %v, want > 3", spike.DeviationSigma)
	}
}

func TestDetect_ReturnsUnusualWhenZBetweenTwoAndThree(t *testing.T) {
	weekCosts := makeWeekCosts(48, 1.0, 0.5)
	dayCosts := makeWeekCosts(24, 1.0, 0.5)
	pool := detectMocks(t, weekCosts, dayCosts, 2.25) // z = (2.25-1)/0.5 = 2.5
	d := newDetector(pool)

	anoms, err := d.Detect(context.Background(), "ws", "", "", "")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var unusual *Anomaly
	for i := range anoms {
		if anoms[i].Type == AnomalyUnusual {
			unusual = &anoms[i]
		}
	}
	if unusual == nil {
		t.Fatalf("expected unusual anomaly; got %+v", anoms)
	}
	if unusual.DeviationSigma < 2.0 || unusual.DeviationSigma > 3.0 {
		t.Errorf("DeviationSigma = %v, want in (2, 3]", unusual.DeviationSigma)
	}
}

func TestDetect_ReturnsTrendWhen24hAvgExceedsBaselineByFiftyPercent(t *testing.T) {
	weekCosts := makeWeekCosts(48, 1.0, 0.5)
	dayCosts := makeWeekCosts(24, 2.0, 0.1)          // last 24h mean = 2.0, baseline = 1.0 → 2x
	pool := detectMocks(t, weekCosts, dayCosts, 1.0) // current hour is normal
	d := newDetector(pool)

	anoms, err := d.Detect(context.Background(), "ws", "", "", "")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var trend *Anomaly
	for i := range anoms {
		if anoms[i].Type == AnomalyTrend {
			trend = &anoms[i]
		}
	}
	if trend == nil {
		t.Fatalf("expected trend anomaly; got %+v", anoms)
	}
}

func TestDetect_ReturnsEmptyWhenWithinNormalRange(t *testing.T) {
	weekCosts := makeWeekCosts(48, 1.0, 0.5)
	dayCosts := makeWeekCosts(24, 1.0, 0.5)
	pool := detectMocks(t, weekCosts, dayCosts, 1.2) // z = 0.4, well within normal
	d := newDetector(pool)

	anoms, err := d.Detect(context.Background(), "ws", "", "", "")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(anoms) != 0 {
		t.Errorf("expected no anomalies, got %+v", anoms)
	}
}

func TestDetect_ReturnsNilWhenCountBelow24(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`date_trunc.*hour`).
		WithArgs("ws", "", "", "", 7).
		WillReturnRows(hourlyRows(makeWeekCosts(10, 1.0, 0.5))) // only 10 buckets
	// No further queries should fire — Detect must short-circuit.
	d := newDetector(pool)

	anoms, err := d.Detect(context.Background(), "ws", "", "", "")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if anoms != nil {
		t.Errorf("expected nil anomalies on insufficient data, got %+v", anoms)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("queries beyond the window check fired: %v", err)
	}
}

func TestScanAll_AggregatesAcrossDimensions(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`DISTINCT.*workspace_id.*FROM token_events`).
		WithArgs("").
		WillReturnRows(pgxmock.NewRows([]string{"workspace_id", "team", "feature", "provider"}).
			AddRow("ws-1", "core", "search", "openai").
			AddRow("ws-2", "core", "summarise", "anthropic"))

	// Each dimension's Detect runs the week-window query; both return
	// insufficient data so no further queries fire per dim. This proves
	// ScanAll iterates every distinct dimension AND that the
	// insufficient-data short-circuit applies inside the loop.
	for i := 0; i < 2; i++ {
		pool.ExpectQuery(`date_trunc.*hour`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(hourlyRows(makeWeekCosts(5, 1.0, 0.5)))
	}

	d := newDetector(pool)
	anoms, err := d.ScanAll(context.Background())
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	// Both dimensions short-circuited at the count check → no anomalies.
	if len(anoms) != 0 {
		t.Errorf("expected 0 anomalies (insufficient data both dims), got %+v", anoms)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("ScanAll did not iterate every dimension: %v", err)
	}
}

func TestDetect_StdDevZeroSkipsZScoreDetection(t *testing.T) {
	// Constant cost ⇒ stddev = 0. A current-hour spike to 1000 must NOT
	// trigger spike/unusual (division by zero); trend can still fire.
	pool := newPool(t)
	weekCosts := makeWeekCosts(48, 1.0, 0.0)
	dayCosts := makeWeekCosts(24, 1.0, 0.0)
	pool.ExpectQuery(`date_trunc.*hour`).WithArgs("ws", "", "", "", 7).WillReturnRows(hourlyRows(weekCosts))
	pool.ExpectQuery(`SUM\(cost_usd\)`).WithArgs("ws", "", "", "").
		WillReturnRows(pgxmock.NewRows([]string{"current_cost"}).AddRow(float64(1000)))
	pool.ExpectQuery(`date_trunc.*hour`).WithArgs("ws", "", "", "", 1).WillReturnRows(hourlyRows(dayCosts))

	d := newDetector(pool)
	anoms, _ := d.Detect(context.Background(), "ws", "", "", "")
	for _, a := range anoms {
		if a.Type == AnomalySpike || a.Type == AnomalyUnusual {
			t.Errorf("stddev=0 must not yield z-score anomaly; got %+v", a)
		}
	}
	// Sanity: message must never include prompt-like content.
	for _, a := range anoms {
		if strings.Contains(strings.ToLower(a.Message), "prompt") {
			t.Errorf("anomaly message mentions 'prompt' — should be cost-only: %q", a.Message)
		}
	}
}
