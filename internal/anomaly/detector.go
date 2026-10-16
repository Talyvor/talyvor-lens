// Package anomaly detects statistical cost anomalies on top of the
// raw token_events table. Threshold alerts (alerts.AlertManager) catch
// budget breaches; this detector catches the cases where spend isn't
// over the line but is sigma-σ above its own recent baseline.
package anomaly

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

const (
	// minBucketsForDetection is the floor on hourly samples — without
	// at least this many, the stddev is meaningless and we'd false-alarm
	// every new workspace on its first spike of activity.
	minBucketsForDetection = 24

	// Z-score thresholds. The bands are open-low/closed-high so a value
	// landing exactly at z=2 is normal, exactly at z=3 is still unusual,
	// only z > 3 is a spike. The boundary choice matches the spec.
	thresholdSpike   = 3.0
	thresholdUnusual = 2.0

	// trendMultiplier defines the 24h-vs-baseline ratio above which we
	// fire AnomalyTrend. 1.5x is the spec value — half-again as much
	// spend as the trailing 7-day baseline.
	trendMultiplier = 1.5

	// defaultScanInterval is how often StartMonitor wakes up.
	defaultScanInterval = time.Hour
)

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type AnomalyType string

const (
	AnomalySpike   AnomalyType = "spike"
	AnomalyTrend   AnomalyType = "trend"
	AnomalyUnusual AnomalyType = "unusual"
)

type Anomaly struct {
	ID             string      `json:"id"`
	WorkspaceID    string      `json:"workspace_id"`
	Team           string      `json:"team"`
	Feature        string      `json:"feature"`
	Provider       string      `json:"provider"`
	Type           AnomalyType `json:"type"`
	CurrentValue   float64     `json:"current_value"`
	BaselineValue  float64     `json:"baseline_value"`
	DeviationSigma float64     `json:"deviation_sigma"`
	Message        string      `json:"message"`
	DetectedAt     time.Time   `json:"detected_at"`
}

type WindowStats struct {
	Mean   float64
	StdDev float64
	Min    float64
	Max    float64
	Count  int
}

type Detector struct {
	pool pgxDB
}

func New(pool *pgxpool.Pool) *Detector {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newDetector(db)
}

func newDetector(db pgxDB) *Detector {
	return &Detector{pool: db}
}

const computeWindowSQL = `SELECT
    date_trunc('hour', created_at) AS hour,
    SUM(cost_usd)                  AS hourly_cost
FROM token_events
WHERE workspace_id = $1
  AND ($2 = '' OR team = $2)
  AND ($3 = '' OR feature = $3)
  AND ($4 = '' OR provider = $4)
  AND created_at > NOW() - (INTERVAL '1 day' * $5::int)
  AND created_at < NOW() - INTERVAL '1 hour'
GROUP BY hour
ORDER BY hour`

const currentHourSQL = `SELECT COALESCE(SUM(cost_usd), 0) AS current_cost
FROM token_events
WHERE workspace_id = $1
  AND ($2 = '' OR team = $2)
  AND ($3 = '' OR feature = $3)
  AND ($4 = '' OR provider = $4)
  AND created_at > date_trunc('hour', NOW())`

const distinctDimsSQL = `SELECT DISTINCT workspace_id, team, feature, provider
FROM token_events
WHERE created_at > NOW() - INTERVAL '7 days'
  AND cost_usd > 0
  AND ($1 = '' OR workspace_id = $1)`

// ComputeWindowStats aggregates hourly cost samples over a window of
// windowDays days and returns their first/second moments. The current
// in-progress hour is excluded so a half-full hour doesn't pull the
// baseline up. Returns (nil, nil) when no rows came back so callers
// can short-circuit on "no data" cleanly.
func (d *Detector) ComputeWindowStats(ctx context.Context, workspaceID, team, feature, provider string, windowDays int) (*WindowStats, error) {
	if d.pool == nil {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, computeWindowSQL, workspaceID, team, feature, provider, windowDays)
	if err != nil {
		return nil, fmt.Errorf("anomaly: window query: %w", err)
	}
	defer rows.Close()

	var values []float64
	for rows.Next() {
		var hour time.Time
		var cost float64
		if err := rows.Scan(&hour, &cost); err != nil {
			return nil, fmt.Errorf("anomaly: scan: %w", err)
		}
		values = append(values, cost)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, nil
	}
	return computeStats(values), nil
}

// computeStats is split out so Detect can reuse it on the trend slice
// without re-querying. Mean/StdDev are population moments (divided by N,
// not N-1) because the buckets are the entire universe of observations
// over the window, not a sample of it.
func computeStats(values []float64) *WindowStats {
	n := len(values)
	if n == 0 {
		return nil
	}
	var sum, min, max float64
	min, max = values[0], values[0]
	for _, v := range values {
		sum += v
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	mean := sum / float64(n)
	var sqDiff float64
	for _, v := range values {
		d := v - mean
		sqDiff += d * d
	}
	return &WindowStats{
		Mean:   mean,
		StdDev: math.Sqrt(sqDiff / float64(n)),
		Min:    min,
		Max:    max,
		Count:  n,
	}
}

// CheckCurrentHour returns the cost accumulated since the top of the
// current hour. Even an in-progress (partial) hour shows up here — it's
// the raw "what's happening right now" signal Detect compares against
// the trailing baseline.
func (d *Detector) CheckCurrentHour(ctx context.Context, workspaceID, team, feature, provider string) (float64, error) {
	if d.pool == nil {
		return 0, nil
	}
	var cost float64
	if err := d.pool.QueryRow(ctx, currentHourSQL, workspaceID, team, feature, provider).Scan(&cost); err != nil {
		return 0, fmt.Errorf("anomaly: current hour query: %w", err)
	}
	return cost, nil
}

// Detect runs the spike/unusual/trend checks for one dimension tuple.
// Returns nil (not an empty slice) when there isn't enough trailing
// data to make any judgement — callers can distinguish "no anomaly"
// from "insufficient data" by checking len vs nil.
func (d *Detector) Detect(ctx context.Context, workspaceID, team, feature, provider string) ([]Anomaly, error) {
	weekStats, err := d.ComputeWindowStats(ctx, workspaceID, team, feature, provider, 7)
	if err != nil {
		return nil, err
	}
	if weekStats == nil || weekStats.Count < minBucketsForDetection {
		return nil, nil
	}

	currentHour, err := d.CheckCurrentHour(ctx, workspaceID, team, feature, provider)
	if err != nil {
		return nil, err
	}

	var anomalies []Anomaly
	now := time.Now().UTC()
	mk := func(t AnomalyType, msg string, sigma float64) Anomaly {
		return Anomaly{
			ID:             uuid.NewString(),
			WorkspaceID:    workspaceID,
			Team:           team,
			Feature:        feature,
			Provider:       provider,
			Type:           t,
			CurrentValue:   currentHour,
			BaselineValue:  weekStats.Mean,
			DeviationSigma: sigma,
			Message:        msg,
			DetectedAt:     now,
		}
	}

	// Z-score branch. StdDev = 0 means the series is constant — any
	// z-score arithmetic divides by zero, so we skip the spike/unusual
	// detectors entirely (the trend detector below still runs).
	if weekStats.StdDev > 0 {
		z := (currentHour - weekStats.Mean) / weekStats.StdDev
		switch {
		case z > thresholdSpike:
			anomalies = append(anomalies, mk(AnomalySpike,
				fmt.Sprintf("Cost spike detected: $%.2f vs baseline $%.2f/hr (%.1fσ above normal)", currentHour, weekStats.Mean, z),
				z))
		case z > thresholdUnusual:
			anomalies = append(anomalies, mk(AnomalyUnusual,
				fmt.Sprintf("Unusual cost activity: $%.2f vs $%.2f/hr normal (%.1fσ)", currentHour, weekStats.Mean, z),
				z))
		}
	}

	// Trend branch: a 24h average that sits well above the 7-day
	// baseline is suspicious even when no single hour is a spike. A
	// flat 1.5x over a day adds up fast.
	dayStats, err := d.ComputeWindowStats(ctx, workspaceID, team, feature, provider, 1)
	if err != nil {
		return anomalies, err
	}
	if dayStats != nil && dayStats.Count > 0 && weekStats.Mean > 0 && dayStats.Mean > weekStats.Mean*trendMultiplier {
		ratio := dayStats.Mean / weekStats.Mean
		anomalies = append(anomalies, mk(AnomalyTrend,
			fmt.Sprintf("Cost trending up: $%.2f/hr avg over 24h vs $%.2f/hr baseline (%.1fx)", dayStats.Mean, weekStats.Mean, ratio),
			0,
		))
	}

	return anomalies, nil
}

// ScanAll enumerates every (workspace_id, team, feature, provider)
// tuple that has produced cost in the last 7 days and runs Detect on
// each. The DISTINCT query keeps the dimension count bounded — typical
// deployments see < 1000 tuples, well under the 30s scan budget.
func (d *Detector) ScanAll(ctx context.Context) ([]Anomaly, error) { return d.ScanWorkspace(ctx, "") }

// ScanWorkspace is ScanAll confined to one workspace's dimensions ("" = every workspace).
func (d *Detector) ScanWorkspace(ctx context.Context, workspaceID string) ([]Anomaly, error) {
	if d.pool == nil {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, distinctDimsSQL, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("anomaly: distinct dims query: %w", err)
	}
	type dim struct{ ws, team, feature, provider string }
	var dims []dim
	for rows.Next() {
		var x dim
		if err := rows.Scan(&x.ws, &x.team, &x.feature, &x.provider); err != nil {
			rows.Close()
			return nil, fmt.Errorf("anomaly: scan dim: %w", err)
		}
		dims = append(dims, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []Anomaly
	for _, x := range dims {
		anoms, err := d.Detect(ctx, x.ws, x.team, x.feature, x.provider)
		if err != nil {
			// Individual dimension failures don't kill the scan — log
			// and move on. Cost-only metadata; no prompt content.
			slog.Warn("anomaly: dimension scan failed",
				slog.String("workspace_id", x.ws),
				slog.String("team", x.team),
				slog.String("feature", x.feature),
				slog.String("provider", x.provider),
				slog.String("err", err.Error()),
			)
			continue
		}
		out = append(out, anoms...)
	}
	return out, nil
}

// StartMonitor wakes up every interval, runs ScanAll, publishes each
// anomaly to lens.anomalies.<type> on NATS, and logs the event. Cost
// metadata only — no prompt content, no response bodies.
func (d *Detector) StartMonitor(ctx context.Context, nc *nats.Conn, interval time.Duration) {
	if interval <= 0 {
		interval = defaultScanInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			anoms, err := d.ScanAll(ctx)
			if err != nil {
				slog.Error("anomaly: scan failed", slog.String("err", err.Error()))
				continue
			}
			for _, a := range anoms {
				slog.Warn("anomaly detected",
					slog.String("type", string(a.Type)),
					slog.String("workspace_id", a.WorkspaceID),
					slog.String("team", a.Team),
					slog.String("feature", a.Feature),
					slog.String("provider", a.Provider),
					slog.Float64("current_value", a.CurrentValue),
					slog.Float64("baseline_value", a.BaselineValue),
					slog.Float64("deviation_sigma", a.DeviationSigma),
					slog.String("message", a.Message),
				)
				if nc != nil {
					if data, err := json.Marshal(a); err == nil {
						_ = nc.Publish("lens.anomalies."+string(a.Type), data)
					}
				}
			}
		}
	}
}
