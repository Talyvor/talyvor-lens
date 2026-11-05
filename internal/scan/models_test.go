package scan

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)

func TestNormalAndAnomalyMaths(t *testing.T) {
	at := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	pts := []storedPoint{
		{1, at.AddDate(-3, 0, 2), 10},
		{2, at.AddDate(-2, 0, -5), 12},
		{3, at.AddDate(-1, 0, 0), 14},
		{4, at.AddDate(-1, 0, 30), 99}, // outside the window: not part of the normal
		{5, at, 16},
	}
	mean, sd, inputs, ok := normalAt(pts, at, 10*oneDay)
	if !ok || mean != 12 || sd != 2 || len(inputs) != 3 {
		t.Fatalf("normal: mean %v sd %v inputs %v ok %v; want 12, 2, three points", mean, sd, inputs, ok)
	}
	if z := (pts[4].value - mean) / sd; z != 2 {
		t.Fatalf("anomaly: %v standard deviations, want 2", z)
	}
	if _, _, _, ok := normalAt(pts[1:], at, 10*oneDay); ok {
		t.Fatal("two past years made a normal; it needs three")
	}
	if q := quantile([]float64{4, 1, 3, 2, 5}, 0.10); math.Abs(q-1.4) > 1e-12 {
		t.Fatalf("10th percentile of 1..5: %v, want 1.4", q)
	}
}

// weekly is a series with a yearly cycle plus an anomaly that keeps phi of itself each week, and noise.
func weekly(years int, phi float64, seed int64) []storedPoint {
	rng := rand.New(rand.NewSource(seed))
	var pts []storedPoint
	a := 0.0
	for i := 0; i < years*52; i++ {
		at := t0.Add(time.Duration(i) * oneWeek)
		a = phi*a + rng.NormFloat64()
		season := 50 * math.Sin(2*math.Pi*float64(at.YearDay())/365)
		pts = append(pts, storedPoint{int64(i + 1), at, 400 + season + 10*a})
	}
	return pts
}

func TestBacktestPassesAPersistentAnomaly(t *testing.T) {
	f, _ := FamilyOf("eia.stocks.cushing-crude")
	r := Backtest(f, "eia.stocks.cushing-crude", weekly(8, 0.9, 1))
	if !r.Passed {
		t.Fatalf("a persistent anomaly failed: %s", r.Verdict)
	}
	// Four weeks of 0.9 a week is 0.66 of the anomaly; the seasonal normal from five noisy years blurs it a little.
	if r.Persistence < 0.4 || r.Persistence > 0.9 {
		t.Fatalf("persistence %v, want about 0.66", r.Persistence)
	}
	t.Log(r.Verdict)
}

func TestBacktestFailsAModelWithoutSkill(t *testing.T) {
	f, _ := FamilyOf("eia.stocks.cushing-crude")
	if r := Backtest(f, "s", weekly(8, 0, 2)); r.Passed {
		t.Fatalf("pure noise passed: %s", r.Verdict)
	} else {
		t.Log(r.Verdict)
	}
	if r := Backtest(f, "s", weekly(4, 0.9, 3)); r.Passed || !strings.HasPrefix(r.Verdict, "not enough history") {
		t.Fatalf("four years passed or failed for another reason: %s", r.Verdict)
	}
}

func TestEveryFamilyIsWrittenDown(t *testing.T) {
	for _, f := range Families {
		if f.Why == "" || f.Horizon < 1 || f.Step <= 0 || f.Window <= 0 || f.Window+time.Duration(f.Horizon)*f.Step >= 300*oneDay {
			t.Errorf("%s: a family needs its reason, a horizon and a window well inside a year", f.Prefix)
		}
	}
	if _, ok := FamilyOf("gie.agsi.de.injection"); ok {
		t.Fatal("only a country's fill is modelled")
	}
	if f, ok := FamilyOf("gie.agsi.de.full"); !ok || f.Horizon != 14 {
		t.Fatal("a country's fill has no model")
	}
}
