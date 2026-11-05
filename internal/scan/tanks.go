package scan

// B52.29 — the tank pilot: how full Cushing's floating-roof tanks are, from Sentinel-1 radar, checked against EIA's
// weekly Cushing stocks. The method and the publish threshold are in pilot/cushing.plan.txt, written before the
// backtest ran. The scenes' readings are recorded in pilot/ (tanks_record_test.go re-reads them from the images), so
// the backtest is a fixed study: it is stored once per model as a scan_backtests row, the operator sees it at
// /v1/admin/scan/models, and TankFillPublishable is the switch that keeps the reading out of reports unless the
// backtest's error was inside the threshold.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// TankSeries is the pilot's series; TankModelID its model, so a later method never inherits this one's verdict.
	TankSeries  = "tanks.cushing.fill"
	TankModelID = "sar-floating-roof/v1"
	// TankPublishMAE is the publish threshold the plan wrote down first: the mean absolute error on the test scenes,
	// in thousand barrels.
	TankPublishMAE = 2500.0
	// tankMinTested is how many test scenes the plan needs to judge the model.
	tankMinTested = 20
)

var (
	// tankTestFrom and tankTestTo bound the test scenes; the fit uses only the scenes before tankTestFrom.
	tankTestFrom = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tankTestTo   = time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
)

//go:embed pilot/cushing-scenes.json
var tankScenesJSON []byte

//go:embed pilot/eia-cushing-weekly.json
var eiaCushingJSON []byte

// tankScene is one Sentinel-1 scene's reading of Cushing: the mean over its readable tanks of ln(far half's VV
// power ÷ near half's).
type tankScene struct {
	Item     string    `json:"item"`
	At       time.Time `json:"at"`
	Tanks    int       `json:"tanks_read"`
	Index    float64   `json:"index"`
	Platform string    `json:"platform,omitempty"`
}

type tankScenes struct {
	Source string      `json:"source"`
	Tanks  int         `json:"tanks"`
	Scenes []tankScene `json:"scenes"`
}

type eiaWeek struct {
	WeekEnding string  `json:"week_ending"`
	Stocks     float64 `json:"thousand_barrels"`
}

type eiaWeeks struct {
	Source string    `json:"source"`
	Stocks []eiaWeek `json:"stocks"`
}

// stocksAt is EIA's Cushing stocks at t, on the straight line between the week-ending Fridays either side of it.
func stocksAt(weeks []eiaWeek, t time.Time) (float64, bool) {
	for i := 1; i < len(weeks); i++ {
		a, errA := time.Parse(time.DateOnly, weeks[i-1].WeekEnding)
		b, errB := time.Parse(time.DateOnly, weeks[i].WeekEnding)
		if errA != nil || errB != nil || t.Before(a) || t.After(b) || b.Sub(a) > 8*oneDay {
			continue
		}
		f := float64(t.Sub(a)) / float64(b.Sub(a))
		return weeks[i-1].Stocks + f*(weeks[i].Stocks-weeks[i-1].Stocks), true
	}
	return 0, false
}

// TankBacktest fits stocks = a + b × index on the scenes before tankTestFrom and checks it on the scenes to
// tankTestTo, as the plan wrote down. It passes when the test error is within TankPublishMAE and smaller than the
// training mean's, over at least tankMinTested scenes. In the Report, Persistence is the fitted b, Error and
// NormalError are the model's and the baseline's mean absolute errors, HitRate is the share of test scenes the model
// put on the right side of the training mean, and Coverage the share inside the 80% range of the fit's misses.
func TankBacktest(scenes []tankScene, weeks []eiaWeek) Report {
	r := Report{Series: TankSeries, Model: TankModelID, Method: tankMethod}
	var train, test [][2]float64 // index, stocks
	for _, s := range scenes {
		v, ok := stocksAt(weeks, s.At)
		if !ok || !s.At.Before(tankTestTo) {
			continue
		}
		if r.DataFrom.IsZero() || s.At.Before(r.DataFrom) {
			r.DataFrom = s.At
		}
		if s.At.After(r.DataTo) {
			r.DataTo = s.At
		}
		if s.At.Before(tankTestFrom) {
			train = append(train, [2]float64{s.Index, v})
		} else {
			test = append(test, [2]float64{s.Index, v})
		}
	}
	if len(train) < 3 || len(test) < tankMinTested {
		r.Verdict = fmt.Sprintf("not enough scenes: %d to fit and %d to test, the plan needs %d to test",
			len(train), len(test), tankMinTested)
		return r
	}
	var mx, my float64
	for _, p := range train {
		mx, my = mx+p[0], my+p[1]
	}
	mx, my = mx/float64(len(train)), my/float64(len(train))
	var sxy, sxx float64
	for _, p := range train {
		sxy += (p[0] - mx) * (p[1] - my)
		sxx += (p[0] - mx) * (p[0] - mx)
	}
	if sxx > 0 {
		r.Persistence = sxy / sxx
	}
	predict := func(x float64) float64 { return my + r.Persistence*(x-mx) }
	misses := make([]float64, len(train))
	for i, p := range train {
		misses[i] = p[1] - predict(p[0])
	}
	r.MissLow, r.MissHigh = quantile(misses, rangeLow), quantile(misses, rangeHigh)
	var hits, covered int
	for _, p := range test {
		mid := predict(p[0])
		r.Error += math.Abs(p[1] - mid)
		r.NormalError += math.Abs(p[1] - my)
		if (mid-my)*(p[1]-my) > 0 {
			hits++
		}
		if p[1] >= mid+r.MissLow && p[1] <= mid+r.MissHigh {
			covered++
		}
	}
	n := float64(len(test))
	r.Tested, r.Independent = len(test), n
	r.Error, r.NormalError, r.HitRate, r.Coverage = r.Error/n, r.NormalError/n, float64(hits)/n, float64(covered)/n
	switch {
	case r.Error > TankPublishMAE:
		r.Verdict = fmt.Sprintf("failed: error %.0f thousand barrels on %d test scenes, outside the %.0f the plan wrote down first "+
			"(the training mean's alone: %.0f); the pilot stays internal", r.Error, r.Tested, TankPublishMAE, r.NormalError)
	case r.Error >= r.NormalError:
		r.Verdict = fmt.Sprintf("failed: error %.0f thousand barrels, no smaller than the training mean's alone (%.0f); "+
			"the pilot stays internal", r.Error, r.NormalError)
	default:
		r.Passed = true
		r.Verdict = fmt.Sprintf("passed: error %.0f thousand barrels on %d test scenes, inside the %.0f written down first "+
			"and smaller than the training mean's %.0f", r.Error, r.Tested, TankPublishMAE, r.NormalError)
	}
	return r
}

const tankMethod = "Pilot (pilot/cushing.plan.txt, written before the backtest): Sentinel-1 IW GRD scenes of relative " +
	"orbit 34 over Cushing, Oklahoma, VV at 10 m, read from Microsoft Planetary Computer; 305 floating-roof tanks at " +
	"least 40 m across from OpenStreetMap (© OpenStreetMap contributors, ODbL). Each tank's reading is ln(far half's " +
	"power ÷ near half's) across the tank, where the roof and far inner wall make a corner that is brighter the lower " +
	"the roof; a scene's index is the mean over its tanks. Fitted to EIA's weekly Cushing stocks (interpolated to each " +
	"scene) by least squares on 2023, tested on 1 Jan 2024 to 30 Jun 2025. Publishes only if the test error is at " +
	"most 2,500 thousand barrels and smaller than the 2023 mean's. Contains modified Copernicus Sentinel data."

func loadTankPilot() ([]tankScene, []eiaWeek, error) {
	var s tankScenes
	var e eiaWeeks
	if err := json.Unmarshal(tankScenesJSON, &s); err != nil {
		return nil, nil, fmt.Errorf("scan: tank scenes: %w", err)
	}
	if err := json.Unmarshal(eiaCushingJSON, &e); err != nil {
		return nil, nil, fmt.Errorf("scan: EIA Cushing: %w", err)
	}
	sort.Slice(s.Scenes, func(i, j int) bool { return s.Scenes[i].At.Before(s.Scenes[j].At) })
	return s.Scenes, e.Stocks, nil
}

// RunTankPilot stores the pilot's backtest, once per model: its data is fixed, so a second run would store the
// same row.
func (m *Models) RunTankPilot(ctx context.Context) (Report, error) {
	scenes, weeks, err := loadTankPilot()
	if err != nil {
		return Report{}, err
	}
	r := TankBacktest(scenes, weeks)
	err = m.store.pool.QueryRow(ctx, `INSERT INTO scan_backtests (series, model, method, data_from, data_to, tested,
		independent, hit_rate, error, normal_error, coverage, passed, verdict, persistence, miss_low, miss_high)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
		WHERE NOT EXISTS (SELECT 1 FROM scan_backtests WHERE series = $1 AND model = $2) RETURNING ran_at`,
		r.Series, r.Model, r.Method, nullTime(r.DataFrom), nullTime(r.DataTo), r.Tested, r.Independent, r.HitRate,
		r.Error, r.NormalError, r.Coverage, r.Passed, r.Verdict, r.Persistence, r.MissLow, r.MissHigh).Scan(&r.RanAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	return r, err
}

// TankFillPublishable is the publish switch: true only when the pilot's stored backtest of this model passed the
// threshold written down first. Anything that would put the tank reading in a report asks it first.
func (m *Models) TankFillPublishable(ctx context.Context) (bool, string, error) {
	var passed bool
	var verdict string
	err := m.store.pool.QueryRow(ctx, `SELECT passed, verdict FROM scan_backtests WHERE series = $1 AND model = $2
		ORDER BY ran_at DESC, id DESC LIMIT 1`, TankSeries, TankModelID).Scan(&passed, &verdict)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "the tank pilot has not been backtested", nil
	}
	return passed, verdict, err
}
