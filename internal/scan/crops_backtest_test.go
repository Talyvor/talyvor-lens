package scan

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"testing"
	"time"
)

// The crop-health backtest (testdata/crops/backtest-iowa-2023.plan.txt, written before the index was computed):
// over the 2023 season, central Iowa's weekly NDVI anomaly moves the same way as NASS's Iowa corn rating.

const (
	backtestScenes  = "testdata/crops/sentinel2-15TVG-2018-2023.json"
	backtestRatings = "testdata/crops/nass-iowa-corn-condition-2023.json"
)

type recordedScenes struct {
	Source string      `json:"source"`
	Scenes []ndviScene `json:"scenes"`
}

func TestCropHealthBacktestIowa2023(t *testing.T) {
	var rec recordedScenes
	readJSON(t, backtestScenes, &rec)
	var nass struct {
		Ratings []struct {
			WeekEnding      string `json:"week_ending"`
			Good, Excellent float64
		} `json:"ratings"`
	}
	readJSON(t, backtestRatings, &nass)

	byWeek := map[time.Time][]ndviScene{}
	for _, s := range rec.Scenes {
		byWeek[weekStart(s.At)] = append(byWeek[weekStart(s.At)], s)
	}
	weekly := map[time.Time]float64{}
	for w, scenes := range byWeek {
		if v, ok := weeklyNDVI(scenes); ok {
			weekly[w] = v
		}
	}
	var xs, ys []float64
	for _, r := range nass.Ratings {
		end, err := time.Parse("2006-01-02", r.WeekEnding)
		if err != nil {
			t.Fatal(err)
		}
		w := weekStart(end)
		v, ok := weekly[w]
		if !ok {
			t.Logf("week ending %s: %s", r.WeekEnding, NotEnoughClearSky)
			continue
		}
		normal, _, ok := ndviNormal(weekly, w)
		if !ok {
			t.Logf("week ending %s: no five-year normal", r.WeekEnding)
			continue
		}
		t.Logf("week ending %s: NDVI %.3f, normal %.3f, anomaly %+.3f; rated good or excellent %.0f%%",
			r.WeekEnding, v, normal, v-normal, r.Good+r.Excellent)
		xs, ys = append(xs, v-normal), append(ys, r.Good+r.Excellent)
	}
	r := pearson(xs, ys)
	t.Logf("Pearson r = %.3f over %d weeks", r, len(xs))
	if len(xs) < 8 || !(r > 0) {
		t.Fatalf("the index does not move with NASS's rating: r = %.3f over %d weeks (the plan needs r > 0 over 8 or more)", r, len(xs))
	}
}

func pearson(xs, ys []float64) float64 {
	n := float64(len(xs))
	var mx, my float64
	for i := range xs {
		mx, my = mx+xs[i]/n, my+ys[i]/n
	}
	var sxy, sxx, syy float64
	for i := range xs {
		sxy += (xs[i] - mx) * (ys[i] - my)
		sxx += (xs[i] - mx) * (xs[i] - mx)
		syy += (ys[i] - my) * (ys[i] - my)
	}
	return sxy / math.Sqrt(sxx*syy)
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// TestRecordCropBacktest re-reads the scenes behind the backtest from Earth Search. It runs only with
// SCAN_RECORD_BACKTEST=1 and rewrites testdata.
func TestRecordCropBacktest(t *testing.T) {
	if os.Getenv("SCAN_RECORD_BACKTEST") != "1" {
		t.Skip("set SCAN_RECORD_BACKTEST=1 to re-record from Earth Search")
	}
	ctx := context.Background()
	c := NewCropCollectors(nil, &http.Client{Timeout: 2 * time.Minute}, "")
	rec := recordedScenes{Source: "Earth Search sentinel-2-l2a, tile 15TVG, April to October 2018-2023, read by readScene. " +
		"Contains modified Copernicus Sentinel data."}
	for y := 2018; y <= 2023; y++ {
		items, err := c.searchScenes(ctx, "15TVG", time.Date(y, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(y, 11, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[time.Time]bool{}
		for _, it := range items {
			if seen[it.Properties.Datetime.UTC()] { // a reprocessed copy of a scene already read
				continue
			}
			seen[it.Properties.Datetime.UTC()] = true
			s, err := c.readScene(ctx, it)
			if err != nil {
				t.Logf("%s: %v", it.ID, err)
				continue
			}
			if s.NDVI != nil {
				v := math.Round(*s.NDVI*1e4) / 1e4
				s.NDVI = &v
			}
			s.ClearSky = math.Round(s.ClearSky*1e4) / 1e4
			rec.Scenes = append(rec.Scenes, s)
		}
		t.Logf("%d: %d scenes", y, len(seen))
	}
	b, _ := json.MarshalIndent(rec, "", " ")
	if err := os.WriteFile(backtestScenes, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
