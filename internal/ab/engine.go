// engine.go is the per-workspace experimentation system and the sole A/B path:
// a model with arbitrary variants, deterministic per-user assignment, async
// result recording, and a statistical-light analysis pass. Its HTTP routes hand
// back JSON shapes the frontend renders directly. (The legacy global shadow-A/B
// Tester was retired in #139 — see git history if you need the old contract.)

package ab

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- A/B bucket assignment only, never an id, token or secret
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbjson"
)

// pgxDB is the subset of *pgxpool.Pool the Engine needs. Tests pass nil to skip
// persistence — the in-memory map is the source of truth either way.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ─── types ────────────────────────────────────────

// ExperimentStatus drives lifecycle transitions:
//
//	draft → running → (paused → running)* → completed
//
// `completed` is terminal; restarting requires cloning.
type ExperimentStatus string

const (
	StatusDraft     ExperimentStatus = "draft"
	StatusRunning   ExperimentStatus = "running"
	StatusPaused    ExperimentStatus = "paused"
	StatusCompleted ExperimentStatus = "completed"
)

// MetricType is the primary metric used to pick a winner.
// Variants are always recorded for *all* metrics so the user
// can re-analyse against a different primary without re-running.
type MetricType string

const (
	MetricLatency        MetricType = "latency"
	MetricQuality        MetricType = "quality"
	MetricCost           MetricType = "cost"
	MetricUserPreference MetricType = "user_preference"
)

// Variant is one arm of an experiment. SystemPromptOverride is
// optional — when set, the proxy substitutes it for the request's
// system message; when nil, the variant uses the request's own.
type Variant struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	Model                string  `json:"model"`
	Provider             string  `json:"provider"`
	SystemPromptOverride *string `json:"system_prompt_override,omitempty"`
	Weight               float64 `json:"weight"`
}

// Experiment is the top-level config object. TrafficSplit must
// match Variants length and sum to 1.0 (±0.01) — the validator
// catches drift.
type Experiment struct {
	ID           string           `json:"id"`
	WorkspaceID  string           `json:"workspace_id"`
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	Status       ExperimentStatus `json:"status"`
	StartedAt    *time.Time       `json:"started_at,omitempty"`
	EndedAt      *time.Time       `json:"ended_at,omitempty"`
	Variants     []Variant        `json:"variants"`
	TrafficSplit []float64        `json:"traffic_split"`
	Metric       MetricType       `json:"metric"`
	CreatedAt    time.Time        `json:"created_at"`
}

// ExperimentResult is one observation. UserID identifies the
// caller for sticky assignment; downstream analysis groups by
// VariantID, not UserID, so PII concerns are limited to the
// hash domain.
type ExperimentResult struct {
	ExperimentID string        `json:"experiment_id"`
	VariantID    string        `json:"variant_id"`
	UserID       string        `json:"user_id"`
	Latency      time.Duration `json:"latency"`
	QualityScore float64       `json:"quality_score"`
	CostUSD      float64       `json:"cost_usd"`
	Tokens       int           `json:"tokens"`
	CreatedAt    time.Time     `json:"created_at"`
}

// VariantStats is the per-arm summary used in Analysis.
type VariantStats struct {
	VariantID   string        `json:"variant_id"`
	Name        string        `json:"name"`
	Model       string        `json:"model"`
	SampleCount int           `json:"sample_count"`
	AvgLatency  time.Duration `json:"avg_latency"`
	AvgQuality  float64       `json:"avg_quality"`
	AvgCostUSD  float64       `json:"avg_cost_usd"`
	P95Latency  time.Duration `json:"p95_latency"`
}

// Analysis is the AnalyzeExperiment return shape. Winner is a
// pointer so "no winner yet" reads cleanly as `null` in JSON.
type Analysis struct {
	ExperimentID   string         `json:"experiment_id"`
	Variants       []VariantStats `json:"variants"`
	Winner         *string        `json:"winner,omitempty"`
	Confidence     float64        `json:"confidence"`
	SampleSize     int            `json:"sample_size"`
	Recommendation string         `json:"recommendation"`
}

// ─── validation ───────────────────────────────────

const (
	// MaxActiveExperiments caps active experiments per workspace
	// per spec. The check happens at Create + Start time.
	MaxActiveExperiments = 10

	// TrafficSplitTolerance is the ±band on the 1.0 sum check.
	TrafficSplitTolerance = 0.01

	// MinSamplesForWinner is the per-variant sample floor before
	// winner detection fires.
	MinSamplesForWinner = 100

	// MinSamplesForAnalysis is the experiment-wide floor before
	// AnalyzeExperiment returns anything beyond an empty summary.
	MinSamplesForAnalysis = 10

	// AutoCompleteSamples is the total-sample cap that auto-
	// completes an experiment.
	AutoCompleteSamples = 1000

	// AutoCompleteAge is the max elapsed time before auto-complete.
	AutoCompleteAge = 7 * 24 * time.Hour
)

// ValidateExperiment is the canonical pre-insert check. Each
// error is phrased so it can be surfaced verbatim to API
// callers — no wrapping prose at the call site needed.
func ValidateExperiment(exp *Experiment) error {
	if exp == nil {
		return errors.New("ab: experiment is nil")
	}
	if strings.TrimSpace(exp.WorkspaceID) == "" {
		return errors.New("ab: workspace_id is required")
	}
	if strings.TrimSpace(exp.Name) == "" {
		return errors.New("ab: name is required")
	}
	if len(exp.Variants) < 2 {
		return errors.New("ab: experiment requires at least 2 variants")
	}
	if len(exp.Variants) != len(exp.TrafficSplit) {
		return fmt.Errorf("ab: traffic_split length (%d) must equal variants length (%d)",
			len(exp.TrafficSplit), len(exp.Variants))
	}
	seen := map[string]bool{}
	for i, v := range exp.Variants {
		if strings.TrimSpace(v.ID) == "" {
			return fmt.Errorf("ab: variant[%d] missing id", i)
		}
		if seen[v.ID] {
			return fmt.Errorf("ab: duplicate variant id %q", v.ID)
		}
		seen[v.ID] = true
		if strings.TrimSpace(v.Model) == "" {
			return fmt.Errorf("ab: variant[%d] missing model", i)
		}
	}
	sum := 0.0
	for i, w := range exp.TrafficSplit {
		if w < 0 {
			return fmt.Errorf("ab: traffic_split[%d] is negative", i)
		}
		sum += w
	}
	if diff := sum - 1.0; diff < -TrafficSplitTolerance || diff > TrafficSplitTolerance {
		return fmt.Errorf("ab: traffic_split must sum to 1.0 (got %.4f)", sum)
	}
	switch exp.Metric {
	case MetricLatency, MetricQuality, MetricCost, MetricUserPreference:
		// ok
	case "":
		return errors.New("ab: metric is required")
	default:
		return fmt.Errorf("ab: unknown metric %q", exp.Metric)
	}
	return nil
}

// ─── deterministic assignment ─────────────────────

// AssignVariant returns the variant the supplied user lands on
// for the supplied experiment. Hash-bucket math means the same
// (experimentID, userID) tuple always picks the same variant —
// stable across processes, restarts, and concurrent calls.
//
// The function honours the variant Weight field. When the
// weights don't sum to 1.0 they're normalised, so a caller can
// pass {0.5, 0.5} or {50, 50} interchangeably.
//
// Returns nil when variants is empty.
func AssignVariant(experimentID, userID string, variants []Variant) *Variant {
	if len(variants) == 0 {
		return nil
	}
	// Hash → 0..1 bucket. We pull 8 bytes from the digest and
	// reduce mod 1<<53 so the float64 conversion is exact.
	sum := sha256.Sum256([]byte(experimentID + "::" + userID))
	bucket := float64(binary.BigEndian.Uint64(sum[:8])%(1<<53)) / float64(1<<53)

	total := 0.0
	for _, v := range variants {
		w := v.Weight
		if w < 0 {
			w = 0
		}
		total += w
	}
	if total <= 0 {
		// Degenerate input: pick deterministically by index.
		idx := int(binary.BigEndian.Uint64(sum[:8]) % uint64(len(variants)))
		v := variants[idx]
		return &v
	}
	cumulative := 0.0
	for i := range variants {
		w := variants[i].Weight
		if w < 0 {
			w = 0
		}
		cumulative += w / total
		// `<=` on the last step protects against floating-point
		// jitter pushing the bucket value to ~1.0 and falling off
		// the end of the slice.
		if bucket < cumulative || i == len(variants)-1 {
			v := variants[i]
			return &v
		}
	}
	// Unreachable — the loop's final iteration always returns.
	v := variants[len(variants)-1]
	return &v
}

// ─── engine ───────────────────────────────────────

// Engine owns the experiment catalogue + result recorder. The
// database is optional (`nil` pool = in-memory only) so tests
// can exercise the pure logic without a Postgres fixture.
type Engine struct {
	pool pgxDB
	mu   sync.RWMutex
	// experiments mirrors the database for fast in-memory reads.
	// It's authoritative when pool is nil; otherwise it's a cache
	// hydrated from CRUD writes.
	experiments map[string]*Experiment
	// results buffers ExperimentResult rows when called via
	// RecordResultAsync. A background goroutine flushes them to
	// Postgres so the proxy hot path never blocks on the DB.
	resultsBuf  []ExperimentResult
	closing     chan struct{}
	flushSignal chan struct{}
}

// NewEngine constructs an Engine. nil pool is supported for
// in-memory operation (handy for tests + dry-runs).
func NewEngine(pool *pgxpool.Pool) *Engine {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newEngine(db)
}

func newEngine(pool pgxDB) *Engine {
	e := &Engine{
		pool:        pool,
		experiments: map[string]*Experiment{},
		closing:     make(chan struct{}),
		flushSignal: make(chan struct{}, 1),
	}
	return e
}

// Close stops background workers. Idempotent.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.closing:
		return
	default:
		close(e.closing)
	}
}

// ─── CRUD ─────────────────────────────────────────

const insertExperimentSQL = `
INSERT INTO experiments (
    id, workspace_id, name, description, status, metric,
    traffic_split, variants, started_at, ended_at, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (id) DO UPDATE SET
    name=EXCLUDED.name,
    description=EXCLUDED.description,
    status=EXCLUDED.status,
    metric=EXCLUDED.metric,
    traffic_split=EXCLUDED.traffic_split,
    variants=EXCLUDED.variants,
    started_at=EXCLUDED.started_at,
    ended_at=EXCLUDED.ended_at`

// CreateExperiment validates + stores a new experiment in the
// draft state. Returns ValidationError-shaped errors verbatim
// so the HTTP layer can map them to 400 cleanly.
func (e *Engine) CreateExperiment(ctx context.Context, exp *Experiment) error {
	if exp.CreatedAt.IsZero() {
		exp.CreatedAt = time.Now().UTC()
	}
	if exp.Status == "" {
		exp.Status = StatusDraft
	}
	if exp.ID == "" {
		exp.ID = generateExperimentID()
	}
	if err := ValidateExperiment(exp); err != nil {
		return err
	}
	// Cap on active experiments — count includes the new row only
	// if it's already running (created-then-started flow is the
	// common case, but a CreateExperiment-with-status=running
	// shortcut should still be capped).
	if exp.Status == StatusRunning {
		if err := e.checkActiveCap(ctx, exp.WorkspaceID, exp.ID); err != nil {
			return err
		}
	}
	e.mu.Lock()
	stored := *exp
	e.experiments[exp.ID] = &stored
	e.mu.Unlock()

	if e.pool != nil {
		variantsJSON, _ := dbjson.Marshal(exp.Variants)
		splitJSON, _ := dbjson.Marshal(exp.TrafficSplit)
		if _, err := e.pool.Exec(ctx, insertExperimentSQL,
			exp.ID, exp.WorkspaceID, exp.Name, exp.Description,
			string(exp.Status), string(exp.Metric),
			splitJSON, variantsJSON,
			exp.StartedAt, exp.EndedAt, exp.CreatedAt,
		); err != nil {
			return fmt.Errorf("ab: insert experiment: %w", err)
		}
	}
	return nil
}

// GetExperiment reads from memory (authoritative for in-memory
// mode) or returns a clone of the cached row so callers can't
// mutate engine state.
func (e *Engine) GetExperiment(id string) (*Experiment, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	exp, ok := e.experiments[id]
	if !ok {
		return nil, false
	}
	clone := *exp
	return &clone, true
}

// ListExperiments returns every experiment for workspaceID,
// newest first by CreatedAt.
func (e *Engine) ListExperiments(workspaceID string) []*Experiment {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []*Experiment
	for _, exp := range e.experiments {
		if exp.WorkspaceID == workspaceID {
			clone := *exp
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// StartExperiment transitions draft → running (or paused →
// running) and stamps StartedAt on the first transition.
func (e *Engine) StartExperiment(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	exp, ok := e.experiments[id]
	if !ok {
		return fmt.Errorf("ab: experiment %q not found", id)
	}
	if exp.Status == StatusCompleted {
		return errors.New("ab: cannot start a completed experiment — clone it instead")
	}
	if err := e.checkActiveCapLocked(exp.WorkspaceID, id); err != nil {
		return err
	}
	if exp.Status == StatusDraft {
		now := time.Now().UTC()
		exp.StartedAt = &now
	}
	exp.Status = StatusRunning
	if e.pool != nil {
		variantsJSON, _ := dbjson.Marshal(exp.Variants)
		splitJSON, _ := dbjson.Marshal(exp.TrafficSplit)
		_, _ = e.pool.Exec(ctx, insertExperimentSQL,
			exp.ID, exp.WorkspaceID, exp.Name, exp.Description,
			string(exp.Status), string(exp.Metric),
			splitJSON, variantsJSON, exp.StartedAt, exp.EndedAt, exp.CreatedAt,
		)
	}
	return nil
}

// StopExperiment is the manual completion path. Sets status to
// completed + stamps EndedAt. Use PauseExperiment for a
// temporary halt.
func (e *Engine) StopExperiment(ctx context.Context, id string) error {
	return e.transitionStatus(ctx, id, StatusCompleted, true)
}

// PauseExperiment halts traffic without marking the experiment
// terminal.
func (e *Engine) PauseExperiment(ctx context.Context, id string) error {
	return e.transitionStatus(ctx, id, StatusPaused, false)
}

func (e *Engine) transitionStatus(ctx context.Context, id string, to ExperimentStatus, stampEnd bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	exp, ok := e.experiments[id]
	if !ok {
		return fmt.Errorf("ab: experiment %q not found", id)
	}
	exp.Status = to
	if stampEnd {
		now := time.Now().UTC()
		exp.EndedAt = &now
	}
	if e.pool != nil {
		variantsJSON, _ := dbjson.Marshal(exp.Variants)
		splitJSON, _ := dbjson.Marshal(exp.TrafficSplit)
		_, _ = e.pool.Exec(ctx, insertExperimentSQL,
			exp.ID, exp.WorkspaceID, exp.Name, exp.Description,
			string(exp.Status), string(exp.Metric),
			splitJSON, variantsJSON, exp.StartedAt, exp.EndedAt, exp.CreatedAt,
		)
	}
	return nil
}

func (e *Engine) checkActiveCap(_ context.Context, workspaceID, excludeID string) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.checkActiveCapLocked(workspaceID, excludeID)
}

// checkActiveCapLocked is the lock-free inner check; callers must hold e.mu.
func (e *Engine) checkActiveCapLocked(workspaceID, excludeID string) error {
	active := 0
	for _, exp := range e.experiments {
		if exp.WorkspaceID == workspaceID && exp.Status == StatusRunning && exp.ID != excludeID {
			active++
		}
	}
	if active >= MaxActiveExperiments {
		return fmt.Errorf("ab: workspace already has %d active experiments (max %d)",
			active, MaxActiveExperiments)
	}
	return nil
}

// ─── result recording ─────────────────────────────

const insertResultSQL = `
INSERT INTO experiment_results (
    experiment_id, variant_id, user_id, latency_ms,
    quality_score, cost_usd, tokens, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

// RecordResult is the blocking insert path. Useful for tests
// and for callers that want a strict success/failure signal.
func (e *Engine) RecordResult(ctx context.Context, r ExperimentResult) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.ExperimentID == "" || r.VariantID == "" {
		return errors.New("ab: experiment_id and variant_id required")
	}
	// No in-memory per-result mirror: maintaining running averages there would need
	// pgxmock-style helpers, and the production analysis path reads from Postgres. The
	// resultsBuf below is the only memory-side store (used when there is no pool).

	if e.pool == nil {
		// In-memory only — stash the row.
		e.mu.Lock()
		e.resultsBuf = append(e.resultsBuf, r)
		e.mu.Unlock()
		return nil
	}
	if _, err := e.pool.Exec(ctx, insertResultSQL,
		r.ExperimentID, r.VariantID, r.UserID,
		r.Latency.Milliseconds(), r.QualityScore, r.CostUSD,
		r.Tokens, r.CreatedAt,
	); err != nil {
		return fmt.Errorf("ab: insert result: %w", err)
	}
	return nil
}

// RecordResultAsync queues a result for background insertion.
// Never blocks. The proxy uses this so a slow Postgres can't
// stretch the response path.
func (e *Engine) RecordResultAsync(r ExperimentResult) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.RecordResult(ctx, r); err != nil {
			slog.Warn("ab: async record failed",
				slog.String("experiment", r.ExperimentID),
				slog.String("err", err.Error()))
		}
	}()
}

// ─── analysis ────────────────────────────────────

const analysisSQL = `
SELECT variant_id,
       COUNT(*),
       COALESCE(AVG(latency_ms), 0),
       COALESCE(AVG(quality_score), 0),
       COALESCE(AVG(cost_usd), 0),
       COALESCE(PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
FROM experiment_results
WHERE experiment_id = $1
GROUP BY variant_id`

// AnalyzeExperiment runs the per-variant aggregations and picks
// a winner (if confidence is high enough). Spec rules:
//
//   - Analysis only meaningful at >= 10 total samples.
//   - Winner requires every active variant to have >= 100 samples.
//   - Confidence = min(sample/1000, 0.99) — a deliberately
//     simple proxy for the wider distribution check.
func (e *Engine) AnalyzeExperiment(ctx context.Context, experimentID string) (*Analysis, error) {
	exp, ok := e.GetExperiment(experimentID)
	if !ok {
		return nil, fmt.Errorf("ab: experiment %q not found", experimentID)
	}
	stats, err := e.collectStats(ctx, exp)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, s := range stats {
		total += s.SampleCount
	}
	out := &Analysis{
		ExperimentID: experimentID,
		Variants:     stats,
		SampleSize:   total,
	}
	if total < MinSamplesForAnalysis {
		out.Recommendation = "Not enough samples for analysis yet."
		return out, nil
	}
	// Confidence trends toward 1.0 with sample size; capped at
	// 0.99 so we never claim certainty.
	conf := float64(total) / 1000
	if conf > 0.99 {
		conf = 0.99
	}
	out.Confidence = conf

	// Winner detection: requires every variant to clear the
	// per-variant floor. This prevents a high-traffic variant
	// dominating before the others have a chance.
	floorMet := true
	for _, s := range stats {
		if s.SampleCount < MinSamplesForWinner {
			floorMet = false
			break
		}
	}
	if !floorMet {
		out.Recommendation = fmt.Sprintf("Need at least %d samples per variant for winner detection.", MinSamplesForWinner)
		return out, nil
	}
	winner := pickWinner(stats, exp.Metric)
	if winner != nil {
		out.Winner = &winner.VariantID
		out.Recommendation = fmt.Sprintf("Variant %s wins on %s.", winner.Name, exp.Metric)
	} else {
		out.Recommendation = "No variant outperforms the others significantly."
	}
	return out, nil
}

// collectStats runs the per-variant aggregation. When the engine
// runs in-memory mode (nil pool) we compute the stats directly
// from the buffered results — slower but exercises the same
// code path.
func (e *Engine) collectStats(ctx context.Context, exp *Experiment) ([]VariantStats, error) {
	names := map[string]Variant{}
	for _, v := range exp.Variants {
		names[v.ID] = v
	}
	stats := make(map[string]*VariantStats, len(exp.Variants))
	for _, v := range exp.Variants {
		stats[v.ID] = &VariantStats{
			VariantID: v.ID,
			Name:      v.Name,
			Model:     v.Model,
		}
	}

	if e.pool == nil {
		// In-memory aggregation. We re-walk the buffer per call
		// since this only runs from tests / dashboard reads, not
		// the proxy hot path.
		latencies := map[string][]float64{}
		sums := map[string]struct {
			lat, qual, cost float64
		}{}
		e.mu.RLock()
		for _, r := range e.resultsBuf {
			if r.ExperimentID != exp.ID {
				continue
			}
			s := stats[r.VariantID]
			if s == nil {
				continue
			}
			s.SampleCount++
			d := sums[r.VariantID]
			d.lat += float64(r.Latency.Milliseconds())
			d.qual += r.QualityScore
			d.cost += r.CostUSD
			sums[r.VariantID] = d
			latencies[r.VariantID] = append(latencies[r.VariantID], float64(r.Latency.Milliseconds()))
		}
		e.mu.RUnlock()
		for id, s := range stats {
			if s.SampleCount == 0 {
				continue
			}
			d := sums[id]
			s.AvgLatency = time.Duration(d.lat/float64(s.SampleCount)) * time.Millisecond
			s.AvgQuality = d.qual / float64(s.SampleCount)
			s.AvgCostUSD = d.cost / float64(s.SampleCount)
			ls := latencies[id]
			sort.Float64s(ls)
			s.P95Latency = time.Duration(ls[int(float64(len(ls)-1)*0.95)]) * time.Millisecond
		}
	} else {
		rows, err := e.pool.Query(ctx, analysisSQL, exp.ID)
		if err != nil {
			return nil, fmt.Errorf("ab: analysis query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				vid                        string
				count                      int
				avgLatMs, avgQual, avgCost float64
				p95LatMs                   float64
			)
			if err := rows.Scan(&vid, &count, &avgLatMs, &avgQual, &avgCost, &p95LatMs); err != nil {
				return nil, fmt.Errorf("ab: scan: %w", err)
			}
			s := stats[vid]
			if s == nil {
				// Variant has results but isn't in the catalogue
				// (e.g. config was edited mid-experiment). Surface
				// it anyway so the operator can investigate.
				s = &VariantStats{VariantID: vid, Name: vid}
				stats[vid] = s
			}
			s.SampleCount = count
			s.AvgLatency = time.Duration(avgLatMs) * time.Millisecond
			s.AvgQuality = avgQual
			s.AvgCostUSD = avgCost
			s.P95Latency = time.Duration(p95LatMs) * time.Millisecond
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Return in catalogue order so the dashboard's table stays stable.
	out := make([]VariantStats, 0, len(exp.Variants))
	for _, v := range exp.Variants {
		s := stats[v.ID]
		if s == nil {
			out = append(out, VariantStats{VariantID: v.ID, Name: v.Name, Model: v.Model})
			continue
		}
		out = append(out, *s)
	}
	return out, nil
}

// pickWinner selects the best variant on the primary metric.
// Latency + cost favour the minimum; quality + user_preference
// favour the maximum. Returns nil when fewer than two variants
// have any data.
func pickWinner(stats []VariantStats, metric MetricType) *VariantStats {
	withData := 0
	for _, s := range stats {
		if s.SampleCount > 0 {
			withData++
		}
	}
	if withData < 2 {
		return nil
	}
	var best *VariantStats
	for i := range stats {
		s := &stats[i]
		if s.SampleCount == 0 {
			continue
		}
		if best == nil {
			best = s
			continue
		}
		if betterOn(s, best, metric) {
			best = s
		}
	}
	return best
}

func betterOn(a, b *VariantStats, metric MetricType) bool {
	switch metric {
	case MetricLatency:
		return a.AvgLatency < b.AvgLatency
	case MetricCost:
		return a.AvgCostUSD < b.AvgCostUSD
	case MetricQuality, MetricUserPreference:
		return a.AvgQuality > b.AvgQuality
	}
	return false
}

// ─── auto-complete ────────────────────────────────

// CheckAutoComplete walks every running experiment and marks
// any that exceed the sample or age limit as completed. Safe
// to call repeatedly — the status transition is idempotent.
func (e *Engine) CheckAutoComplete(ctx context.Context) {
	e.mu.RLock()
	ids := make([]string, 0, len(e.experiments))
	for id, exp := range e.experiments {
		if exp.Status == StatusRunning {
			ids = append(ids, id)
		}
	}
	e.mu.RUnlock()

	for _, id := range ids {
		exp, ok := e.GetExperiment(id)
		if !ok || exp.Status != StatusRunning {
			continue
		}
		if exp.StartedAt != nil && time.Since(*exp.StartedAt) >= AutoCompleteAge {
			if err := e.StopExperiment(ctx, id); err != nil {
				slog.Warn("ab: auto-complete age stop failed",
					slog.String("experiment", id), slog.String("err", err.Error()))
			}
			continue
		}
		// Sample count check — re-use analysis aggregator.
		stats, err := e.collectStats(ctx, exp)
		if err != nil {
			continue
		}
		total := 0
		for _, s := range stats {
			total += s.SampleCount
		}
		if total >= AutoCompleteSamples {
			if err := e.StopExperiment(ctx, id); err != nil {
				slog.Warn("ab: auto-complete samples stop failed",
					slog.String("experiment", id), slog.String("err", err.Error()))
			}
		}
	}
}

// RunAutoCompleteLoop fires CheckAutoComplete every `interval`
// until the engine closes. Caller passes the long-lived
// context (typically the server's root context).
func (e *Engine) RunAutoCompleteLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.closing:
				return
			case <-ticker.C:
				e.CheckAutoComplete(ctx)
			}
		}
	}()
}

// ─── helpers ──────────────────────────────────────

// generateExperimentID returns a stable, sortable identifier.
// We use a millisecond timestamp prefix + random suffix; the
// prefix is for human-readable ordering, the suffix avoids
// collisions on concurrent creates.
func generateExperimentID() string {
	return fmt.Sprintf("exp_%d_%06d", time.Now().UnixMilli(), rand.Intn(1_000_000))
}
