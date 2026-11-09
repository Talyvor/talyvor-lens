// Package routing turns the (opt-in) pattern-mining corpus into model
// recommendations — closing the loop so Lens can pick the best
// quality-per-dollar model for a request shape. More opted-in participants
// → better routing for everyone.
//
// This is the one analytics consumer that sits ON the request hot path, so
// the discipline differs from forecast/anomaly/roi:
//
//   - The per-request call (Recommend) is an IN-MEMORY map lookup only —
//     never a DB query. The aggregated corpus is loaded into memory on a
//     timer (Refresh / StartRefresh) and read under an RWMutex.
//   - It is ADVISORY and SAFE: OFF by default; engages only when a request
//     explicitly cedes the model choice (the proxy decides "pinned vs
//     auto"); recommends only WITHIN the workspace's allowed models; and
//     falls back silently to the default on any miss/below-floor/error.
//   - It reads only the privacy-bucketed, opted-in aggregate (CohortStat) —
//     never raw request content.
//   - A minimum sample size ACROSS MULTIPLE WORKSPACES is required before it
//     will recommend anything, so a thin or single-workspace signal can
//     never override the default.
package routing

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
)

// Basis names what a recommendation was ranked on.
type Basis string

const (
	BasisQualityPerDollar Basis = "quality_per_dollar"
	BasisQuality          Basis = "quality" // fallback when the model is unpriced
	BasisNone             Basis = "none"    // no qualifying recommendation
)

const (
	defaultMinSamples      = 20
	defaultMinWorkspaces   = 3
	defaultRefreshInterval = 5 * time.Minute
)

// Config tunes the advisor. Zero fields fall back to defaults.
type Config struct {
	Enabled         bool
	TierCohorts     bool          // condition cohorts on the complexity tier (LENS_ROUTING_TIER_COHORTS_ENABLED, default off)
	MinSamples      int           // ≥N patterns in a cohort (default 20)
	MinWorkspaces   int           // ≥M distinct contributing workspaces (default 3)
	RefreshInterval time.Duration // cache refresh cadence (default 5m)
}

// CostFunc returns the blended USD cost per 1k tokens for a model. Injected
// (production passes alerts.CostUSD) so the advisor needs no pricing
// dependency of its own. Return ≤0 for an unpriced/unknown model.
type CostFunc func(model string) float64

// cohortSource is the read surface — *mining.PatternMiner in production.
type cohortSource interface {
	AggregateCohorts(ctx context.Context) ([]mining.CohortStat, error)
	AggregateCohortsTiered(ctx context.Context) ([]mining.CohortStat, error)
}

// Candidate is one model option within a (feature, input-range) cohort,
// enriched with cost so it can be ranked by quality-per-dollar.
type Candidate struct {
	Model              string  `json:"model"`
	Provider           string  `json:"provider"`
	AvgQuality         float64 `json:"avg_quality"`
	CostPer1k          float64 `json:"cost_per_1k"`
	QualityPerDollar   float64 `json:"quality_per_dollar"`
	SampleCount        int     `json:"sample_count"`
	DistinctWorkspaces int     `json:"distinct_workspaces"`
}

// Recommendation is what the advisor would pick (or none).
type Recommendation struct {
	Model              string  `json:"model"`
	Provider           string  `json:"provider"`
	Basis              Basis   `json:"basis"`
	SampleSize         int     `json:"sample_size"`
	DistinctWorkspaces int     `json:"distinct_workspaces"`
	Confidence         string  `json:"confidence"`
	ExpectedQuality    float64 `json:"expected_quality"`
	ExpectedCostPer1k  float64 `json:"expected_cost_per_1k"`
	Reason             string  `json:"reason"`
}

// Status is the introspection payload.
type Status struct {
	Enabled       bool      `json:"enabled"`
	Cohorts       int       `json:"cohorts"`
	Candidates    int       `json:"candidates"`
	LastRefresh   time.Time `json:"last_refresh"`
	MinSamples    int       `json:"min_samples"`
	MinWorkspaces int       `json:"min_workspaces"`
}

// Advisor holds the in-memory cohort cache and the read/refresh paths.
type Advisor struct {
	src  cohortSource
	cost CostFunc
	cfg  Config
	now  func() time.Time

	mu            sync.RWMutex
	cohorts       map[string][]Candidate // feature|input_range → candidates (best first)
	tieredCohorts map[string][]Candidate // feature|input_range|complexity → candidates (only when TierCohorts on)
	lastRefresh   time.Time
}

// New builds an Advisor. src is the pattern aggregation, cost the pricing
// function (nil ⇒ everything unpriced ⇒ quality-basis only).
func New(src cohortSource, cost CostFunc, cfg Config) *Advisor {
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = defaultMinSamples
	}
	if cfg.MinWorkspaces <= 0 {
		cfg.MinWorkspaces = defaultMinWorkspaces
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if cost == nil {
		cost = func(string) float64 { return 0 }
	}
	return &Advisor{src: src, cost: cost, cfg: cfg, now: time.Now, cohorts: map[string][]Candidate{}, tieredCohorts: map[string][]Candidate{}}
}

// Enabled reports whether intelligence is on. The proxy gates the whole
// advisory branch on this so that, when off, routing is byte-for-byte today.
func (a *Advisor) Enabled() bool { return a != nil && a.cfg.Enabled }

func cohortKey(feature, inputRange string) string { return feature + "|" + inputRange }

// cohortKeyTiered adds the complexity dimension — the tier-conditioned key (TierCohorts on).
func cohortKeyTiered(feature, inputRange, complexity string) string {
	return feature + "|" + inputRange + "|" + complexity
}

// Refresh reloads the cohort cache from the aggregated corpus and ranks each
// cohort's candidates (priced quality-per-dollar first, then quality). The
// only DB touch in the whole package.
func (a *Advisor) Refresh(ctx context.Context) error {
	if a == nil || a.src == nil {
		return nil
	}
	stats, err := a.src.AggregateCohorts(ctx)
	if err != nil {
		return err
	}
	next := a.rankCohorts(stats, false)
	// When tier-conditioning is on, ALSO load the tiered overlay. The non-tiered map above is
	// kept as the fallback (Condition 2) and remains the byte-identical flag-OFF path.
	var nextTiered map[string][]Candidate
	if a.cfg.TierCohorts {
		tstats, terr := a.src.AggregateCohortsTiered(ctx)
		if terr != nil {
			return terr
		}
		nextTiered = a.rankCohorts(tstats, true)
	}
	a.mu.Lock()
	a.cohorts = next
	a.tieredCohorts = nextTiered
	a.lastRefresh = a.now()
	a.mu.Unlock()
	return nil
}

// rankCohorts builds + ranks the in-memory cohort map from aggregated stats. When tiered, the key
// carries the complexity dimension. Behavior-preserving extraction of the prior Refresh loop.
func (a *Advisor) rankCohorts(stats []mining.CohortStat, tiered bool) map[string][]Candidate {
	next := make(map[string][]Candidate)
	for _, s := range stats {
		cpk := a.cost(s.ModelUsed)
		qpd := 0.0
		if cpk > 0 {
			qpd = s.AvgQuality / cpk
		}
		k := cohortKey(s.FeatureCategory, s.InputTokenRange)
		if tiered {
			k = cohortKeyTiered(s.FeatureCategory, s.InputTokenRange, s.ComplexityBucket)
		}
		next[k] = append(next[k], Candidate{
			Model: s.ModelUsed, Provider: s.ProviderUsed, AvgQuality: s.AvgQuality,
			CostPer1k: cpk, QualityPerDollar: qpd,
			SampleCount: s.SampleCount, DistinctWorkspaces: s.DistinctWorkspaces,
		})
	}
	for k := range next {
		sortCandidates(next[k])
	}
	return next
}

// sortCandidates orders priced candidates by quality-per-dollar desc, then
// unpriced by quality desc, priced ahead of unpriced.
func sortCandidates(cs []Candidate) {
	sort.Slice(cs, func(i, j int) bool {
		ci, cj := cs[i], cs[j]
		pi, pj := ci.CostPer1k > 0, cj.CostPer1k > 0
		if pi != pj {
			return pi // priced first
		}
		if pi {
			return ci.QualityPerDollar > cj.QualityPerDollar
		}
		return ci.AvgQuality > cj.AvgQuality
	})
}

// StartRefresh does an immediate refresh, then refreshes on the configured
// interval until ctx is cancelled. Reuses the codebase's goroutine-ticker
// pattern. Runs even when disabled (B28.105): the read-only
// RecommendByRange behind Chat's cheaper-model hint needs the cohorts, while
// the hot path (Recommend) still returns none until enabled. The QualityGate
// already runs the same aggregate on the same timer, unflagged.
func (a *Advisor) StartRefresh(ctx context.Context) {
	if a == nil || a.src == nil {
		return
	}
	if err := a.Refresh(ctx); err != nil {
		slog.Warn("routing: initial refresh failed", slog.String("err", err.Error()))
	}
	go func() {
		t := time.NewTicker(a.cfg.RefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := a.Refresh(ctx); err != nil {
					slog.Warn("routing: refresh failed", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// Recommend is the HOT-PATH entry: in-memory lookup only. Returns a
// none-basis recommendation when disabled, below floor, or no allowed
// candidate — the proxy then keeps the default. Records the basis metric.
func (a *Advisor) Recommend(_ context.Context, _ string, feature string, inputTokens int, complexity, provider string, allowedModels, allowedProviders []string) Recommendation {
	if !a.Enabled() {
		return Recommendation{Basis: BasisNone, Reason: "routing intelligence disabled"}
	}
	var rec Recommendation
	if a.cfg.TierCohorts {
		// Tier-conditioned: the complexity tier picks a finer cohort; below-floor / missing tiers
		// fall back to the non-tiered cohort, then the proxy's default.
		rec = a.evaluateTiered(feature, mining.InputBucketFor(inputTokens), complexity, provider, allowedModels, allowedProviders)
	} else {
		// Flag OFF: the exact current path — complexity ignored, byte-identical to today.
		rec = a.evaluate(feature, mining.InputBucketFor(inputTokens), provider, allowedModels, allowedProviders)
	}
	metrics.RoutingRecommendation(string(rec.Basis))
	return rec
}

// RecommendByRange is the read-only introspection entry (the API) — takes an
// explicit input range and does NOT record metrics.
func (a *Advisor) RecommendByRange(_ context.Context, _ string, feature, inputRange, provider string, allowedModels, allowedProviders []string) Recommendation {
	if a == nil {
		return Recommendation{Basis: BasisNone}
	}
	return a.evaluate(feature, inputRange, provider, allowedModels, allowedProviders)
}

// evaluate is the pure selection: filter the cohort to the request's
// provider + the workspace's allowed models, enforce the sample floor, and
// return the best (the cohort is pre-sorted). Never invents a target.
func (a *Advisor) evaluate(feature, inputRange, provider string, allowedModels, allowedProviders []string) Recommendation {
	if rec, ok := providerGuard(provider, allowedProviders); !ok {
		return rec
	}
	a.mu.RLock()
	cands := a.cohorts[cohortKey(feature, inputRange)]
	a.mu.RUnlock()
	return a.pickFromCohort(cands, feature, inputRange, provider, allowedModels)
}

// evaluateTiered is the tier-conditioned selection (TierCohorts on): try the complexity-tier
// cohort first; on a miss OR a below-floor tier (Condition 1: per-tier COUNT(DISTINCT workspace_id)
// < MinWorkspaces ⇒ pickFromCohort returns BasisNone), fall back to the NON-tiered cohort
// (Condition 2), and if that also misses, BasisNone ⇒ the proxy keeps the default. A finer slice
// that fails the per-tier floor never surfaces.
func (a *Advisor) evaluateTiered(feature, inputRange, complexity, provider string, allowedModels, allowedProviders []string) Recommendation {
	if rec, ok := providerGuard(provider, allowedProviders); !ok {
		return rec
	}
	a.mu.RLock()
	tcands := a.tieredCohorts[cohortKeyTiered(feature, inputRange, complexity)]
	ncands := a.cohorts[cohortKey(feature, inputRange)]
	a.mu.RUnlock()
	if rec := a.pickFromCohort(tcands, feature, inputRange, provider, allowedModels); rec.Basis != BasisNone {
		return rec
	}
	return a.pickFromCohort(ncands, feature, inputRange, provider, allowedModels)
}

// providerGuard returns (rec, false) when the request's provider is missing or not in the
// workspace allow-list; (zero, true) to proceed.
func providerGuard(provider string, allowedProviders []string) (Recommendation, bool) {
	if provider == "" {
		return Recommendation{Basis: BasisNone, Reason: "no provider"}, false
	}
	if len(allowedProviders) > 0 && !contains(allowedProviders, provider) {
		return Recommendation{Provider: provider, Basis: BasisNone, Reason: "provider not in workspace allow-list"}, false
	}
	return Recommendation{}, true
}

// pickFromCohort filters a cohort's candidates to the request's provider + the workspace's allowed
// models, enforces the per-cohort sample/workspace floor (the privacy floor — for a TIERED cohort
// these are the per-tier counts), and returns the best (pre-sorted). BasisNone if none qualifies.
func (a *Advisor) pickFromCohort(cands []Candidate, feature, inputRange, provider string, allowedModels []string) Recommendation {
	for _, c := range cands {
		if c.Provider != provider { // v1 stays within the request's provider
			continue
		}
		if len(allowedModels) > 0 && !contains(allowedModels, c.Model) {
			continue // never recommend a model the workspace can't use
		}
		if c.SampleCount < a.cfg.MinSamples || c.DistinctWorkspaces < a.cfg.MinWorkspaces {
			continue // thin / single-workspace signal never overrides the default
		}
		basis := BasisQualityPerDollar
		if c.CostPer1k <= 0 {
			basis = BasisQuality
		}
		return Recommendation{
			Model: c.Model, Provider: c.Provider, Basis: basis,
			SampleSize: c.SampleCount, DistinctWorkspaces: c.DistinctWorkspaces,
			Confidence:        confidence(c.SampleCount, c.DistinctWorkspaces),
			ExpectedQuality:   c.AvgQuality,
			ExpectedCostPer1k: c.CostPer1k,
			Reason: fmt.Sprintf("best %s for %s/%s among %s models: %s (quality %.2f, $%.4f/1k, %d samples across %d workspaces)",
				basis, feature, inputRange, provider, c.Model, c.AvgQuality, c.CostPer1k, c.SampleCount, c.DistinctWorkspaces),
		}
	}
	return Recommendation{
		Provider: provider, Basis: BasisNone,
		Reason: fmt.Sprintf("no qualifying %s candidate for %s/%s (need ≥%d samples across ≥%d workspaces)",
			provider, feature, inputRange, a.cfg.MinSamples, a.cfg.MinWorkspaces),
	}
}

// Status returns the introspection snapshot.
func (a *Advisor) Status() Status {
	if a == nil {
		return Status{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	cands := 0
	for _, cs := range a.cohorts {
		cands += len(cs)
	}
	return Status{
		Enabled: a.cfg.Enabled, Cohorts: len(a.cohorts), Candidates: cands,
		LastRefresh: a.lastRefresh, MinSamples: a.cfg.MinSamples, MinWorkspaces: a.cfg.MinWorkspaces,
	}
}

// CohortDigest is the dashboard view of one cohort's leading candidate.
type CohortDigest struct {
	Feature            string  `json:"feature"`
	InputRange         string  `json:"input_range"`
	Model              string  `json:"model"`
	Provider           string  `json:"provider"`
	AvgQuality         float64 `json:"avg_quality"`
	CostPer1k          float64 `json:"cost_per_1k"`
	QualityPerDollar   float64 `json:"quality_per_dollar"`
	SampleCount        int     `json:"sample_count"`
	DistinctWorkspaces int     `json:"distinct_workspaces"`
	Qualifies          bool    `json:"qualifies"` // meets the sample/workspace floor
}

// Overview returns the leading candidate per cohort for the dashboard. Pure
// in-memory read; "Qualifies" reflects whether it clears the floor (and so
// would actually be recommended).
func (a *Advisor) Overview() []CohortDigest {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]CohortDigest, 0, len(a.cohorts))
	for k, cs := range a.cohorts {
		if len(cs) == 0 {
			continue
		}
		feature, inputRange := splitCohortKey(k)
		top := cs[0]
		out = append(out, CohortDigest{
			Feature: feature, InputRange: inputRange, Model: top.Model, Provider: top.Provider,
			AvgQuality: top.AvgQuality, CostPer1k: top.CostPer1k, QualityPerDollar: top.QualityPerDollar,
			SampleCount: top.SampleCount, DistinctWorkspaces: top.DistinctWorkspaces,
			Qualifies: top.SampleCount >= a.cfg.MinSamples && top.DistinctWorkspaces >= a.cfg.MinWorkspaces,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Feature != out[j].Feature {
			return out[i].Feature < out[j].Feature
		}
		return out[i].InputRange < out[j].InputRange
	})
	return out
}

func splitCohortKey(k string) (feature, inputRange string) {
	// key = feature + "|" + inputRange; split on the last separator so a
	// feature containing "|" (unlikely) still yields the right input range.
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

func confidence(samples, workspaces int) string {
	switch {
	case samples >= 100 && workspaces >= 10:
		return "high"
	case samples >= 40 && workspaces >= 5:
		return "medium"
	default:
		return "low"
	}
}

func contains(hs []string, needle string) bool {
	for _, h := range hs {
		if h == needle {
			return true
		}
	}
	return false
}
