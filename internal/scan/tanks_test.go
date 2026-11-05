package scan

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// The publish switch follows the threshold written down first: an index that tracks the stocks within it passes;
// one that does not fails, however it misses.
func TestTankBacktestFollowsThreshold(t *testing.T) {
	var weeks []eiaWeek
	stock := func(d time.Time) float64 {
		return 30000 + 8000*float64(d.Sub(tankTestFrom)%(200*oneDay))/float64(200*oneDay)
	}
	for d := time.Date(2022, 12, 30, 0, 0, 0, 0, time.UTC); d.Before(tankTestTo.AddDate(0, 0, 14)); d = d.AddDate(0, 0, 7) {
		weeks = append(weeks, eiaWeek{WeekEnding: d.Format(time.DateOnly), Stocks: stock(d)})
	}
	scenes := func(noise float64) []tankScene {
		var out []tankScene
		i := 0
		for d := time.Date(2023, 1, 4, 0, 30, 0, 0, time.UTC); d.Before(tankTestTo); d = d.AddDate(0, 0, 12) {
			v, _ := stocksAt(weeks, d)
			i++
			out = append(out, tankScene{At: d, Index: -v/10000 + noise*float64(i%7-3)})
		}
		return out
	}
	good := TankBacktest(scenes(0.01), weeks)
	if !good.Passed || good.Error > TankPublishMAE || good.Tested < tankMinTested || good.Persistence >= 0 {
		t.Fatalf("a tracking index: %+v", good)
	}
	bad := TankBacktest(scenes(2), weeks)
	if bad.Passed || !strings.HasPrefix(bad.Verdict, "failed") {
		t.Fatalf("a noisy index: %+v", bad)
	}
}

// The recorded pilot's result is the one pilot/cushing.plan.txt records: it failed, so it stays internal.
func TestTankPilotRecordedResult(t *testing.T) {
	scenes, weeks, err := loadTankPilot()
	if err != nil {
		t.Fatal(err)
	}
	r := TankBacktest(scenes, weeks)
	if r.Passed || r.Tested != 45 || math.Round(r.Error) != 5689 || math.Round(r.NormalError) != 6116 {
		t.Fatalf("the recorded pilot: %+v", r)
	}
}

// The recorded pilot is stored once, shown on the operator's page, and the switch reads its verdict.
func TestTankPilotStoredAndSwitch(t *testing.T) {
	s := modelStore(t)
	ctx := context.Background()
	m := NewModels(s)
	if _, err := s.pool.Exec(ctx, `DELETE FROM scan_backtests WHERE series = $1`, TankSeries); err != nil {
		t.Fatal(err)
	}
	if on, _, err := m.TankFillPublishable(ctx); err != nil || on {
		t.Fatalf("before the backtest: publishable %v, %v", on, err)
	}
	want, err := m.RunTankPilot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunTankPilot(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM scan_backtests WHERE series = $1`, TankSeries).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("stored %d rows, %v; want 1", rows, err)
	}
	reports, err := m.Reports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var shown *Report
	for i := range reports {
		if reports[i].Series == TankSeries {
			shown = &reports[i]
		}
	}
	if shown == nil || shown.Verdict != want.Verdict || shown.Tested != want.Tested {
		t.Fatalf("operator's page: %+v, want %+v", shown, want)
	}
	on, verdict, err := m.TankFillPublishable(ctx)
	if err != nil || on != want.Passed || verdict != want.Verdict {
		t.Fatalf("switch: %v %q %v; the backtest: %v %q", on, verdict, err, want.Passed, want.Verdict)
	}
}
