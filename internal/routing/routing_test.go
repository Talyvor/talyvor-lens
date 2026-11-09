package routing

import (
	"context"
	"go/build"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
)

type mockSource struct {
	stats       []mining.CohortStat
	tieredStats []mining.CohortStat
	calls       atomic.Int64 // atomic: the stress test refreshes concurrently
}

func (m *mockSource) AggregateCohorts(_ context.Context) ([]mining.CohortStat, error) {
	m.calls.Add(1)
	return m.stats, nil
}

func (m *mockSource) AggregateCohortsTiered(_ context.Context) ([]mining.CohortStat, error) {
	m.calls.Add(1)
	return m.tieredStats, nil
}

// costStub prices three OpenAI models; unknown → 0 (unpriced).
func costStub(model string) float64 {
	switch model {
	case "gpt-4o":
		return 0.10
	case "gpt-4o-mini":
		return 0.02
	case "gpt-4.1":
		return 0.06
	}
	return 0
}

// chatCohort: three openai candidates for chat/medium, all above the floor.
func chatCohort() []mining.CohortStat {
	return []mining.CohortStat{
		{FeatureCategory: "chat", InputTokenRange: "medium", ModelUsed: "gpt-4o", ProviderUsed: "openai", AvgQuality: 0.95, SampleCount: 50, DistinctWorkspaces: 6},
		{FeatureCategory: "chat", InputTokenRange: "medium", ModelUsed: "gpt-4o-mini", ProviderUsed: "openai", AvgQuality: 0.85, SampleCount: 50, DistinctWorkspaces: 6},
		{FeatureCategory: "chat", InputTokenRange: "medium", ModelUsed: "gpt-4.1", ProviderUsed: "openai", AvgQuality: 0.90, SampleCount: 50, DistinctWorkspaces: 6},
	}
}

func newAdvisor(t *testing.T, stats []mining.CohortStat, cfg Config) (*Advisor, *mockSource) {
	t.Helper()
	if cfg.MinSamples == 0 {
		cfg.MinSamples = 20
	}
	if cfg.MinWorkspaces == 0 {
		cfg.MinWorkspaces = 3
	}
	m := &mockSource{stats: stats}
	a := New(m, costStub, cfg)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return a, m
}

func TestRecommend_PicksBestQualityPerDollar(t *testing.T) {
	a, _ := newAdvisor(t, chatCohort(), Config{Enabled: true})
	// gpt-4o-mini: 0.85/0.02 = 42.5 (best); gpt-4.1: 15; gpt-4o: 9.5.
	rec := a.Recommend(context.Background(), "ws1", "chat", 1000, "", "openai", nil, nil)
	if rec.Basis != BasisQualityPerDollar {
		t.Fatalf("basis: got %q want quality_per_dollar", rec.Basis)
	}
	if rec.Model != "gpt-4o-mini" {
		t.Fatalf("should pick best quality-per-dollar model: got %q want gpt-4o-mini", rec.Model)
	}
}

func TestRecommend_BelowSampleFloorFallsBack(t *testing.T) {
	ctx := context.Background()

	// Too few patterns.
	thin := []mining.CohortStat{{FeatureCategory: "chat", InputTokenRange: "medium", ModelUsed: "gpt-4o-mini", ProviderUsed: "openai", AvgQuality: 0.9, SampleCount: 5, DistinctWorkspaces: 6}}
	a, _ := newAdvisor(t, thin, Config{Enabled: true})
	if rec := a.Recommend(ctx, "ws1", "chat", 1000, "", "openai", nil, nil); rec.Basis != BasisNone {
		t.Fatalf("below min samples must be basis=none, got %q (%s)", rec.Basis, rec.Model)
	}

	// Enough patterns but a single workspace — must NOT override.
	single := []mining.CohortStat{{FeatureCategory: "chat", InputTokenRange: "medium", ModelUsed: "gpt-4o-mini", ProviderUsed: "openai", AvgQuality: 0.9, SampleCount: 500, DistinctWorkspaces: 1}}
	a2, _ := newAdvisor(t, single, Config{Enabled: true})
	if rec := a2.Recommend(ctx, "ws1", "chat", 1000, "", "openai", nil, nil); rec.Basis != BasisNone {
		t.Fatalf("single-workspace signal must be basis=none, got %q", rec.Basis)
	}
}

func TestRecommend_DisabledReturnsNone(t *testing.T) {
	a, _ := newAdvisor(t, chatCohort(), Config{Enabled: false})
	if rec := a.Recommend(context.Background(), "ws1", "chat", 1000, "", "openai", nil, nil); rec.Basis != BasisNone {
		t.Fatalf("disabled advisor must never recommend: got %q", rec.Basis)
	}
}

// B28.105: Chat's cheaper-model hint reads /routing/recommendation, which must answer even while
// LENS_ROUTING_INTELLIGENCE_ENABLED is off. StartRefresh loads the cohorts regardless; the hot path
// (Recommend) still never recommends.
func TestStartRefresh_DisabledStillAnswersTheRead(t *testing.T) {
	m := &mockSource{stats: chatCohort()}
	a := New(m, costStub, Config{Enabled: false})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartRefresh(ctx)
	if rec := a.RecommendByRange(ctx, "ws1", "chat", "medium", "openai", nil, nil); rec.Model != "gpt-4o-mini" || rec.Basis != BasisQualityPerDollar {
		t.Fatalf("read with routing off: got %q (%s), want gpt-4o-mini on quality_per_dollar", rec.Model, rec.Basis)
	}
	if rec := a.Recommend(ctx, "ws1", "chat", 1000, "", "openai", nil, nil); rec.Basis != BasisNone {
		t.Fatalf("hot path with routing off must never recommend: got %q", rec.Basis)
	}
}

// B28.105: /v1/routing/intelligence/status serves Overview to any workspace, so a cohort below the
// privacy floor (here one workspace's own feature tag) must not be listed — only candidates that clear it.
func TestOverview_HidesCohortsBelowTheFloor(t *testing.T) {
	stats := append(chatCohort(),
		mining.CohortStat{FeatureCategory: "acme-secret-feature", InputTokenRange: "small", ModelUsed: "gpt-4o", ProviderUsed: "openai", AvgQuality: 0.9, SampleCount: 400, DistinctWorkspaces: 1},
		mining.CohortStat{FeatureCategory: "chat", InputTokenRange: "small", ModelUsed: "gpt-4o-mini", ProviderUsed: "openai", AvgQuality: 0.9, SampleCount: 5, DistinctWorkspaces: 1},
		mining.CohortStat{FeatureCategory: "chat", InputTokenRange: "small", ModelUsed: "gpt-4.1", ProviderUsed: "openai", AvgQuality: 0.8, SampleCount: 30, DistinctWorkspaces: 4},
	)
	a, _ := newAdvisor(t, stats, Config{})
	got := a.Overview()
	if len(got) != 2 {
		t.Fatalf("overview: got %d cohorts %+v, want chat/medium and chat/small only", len(got), got)
	}
	for _, d := range got {
		if d.Feature != "chat" || !d.Qualifies {
			t.Fatalf("overview lists a cohort below the floor: %+v", d)
		}
	}
	if got[1].InputRange != "small" || got[1].Model != "gpt-4.1" {
		t.Fatalf("chat/small: got %+v, want its best candidate that clears the floor (gpt-4.1)", got[1])
	}
}

// (b) Flag OFF (TierCohorts unset) must IGNORE the complexity arg — different complexities yield
// the identical non-tiered recommendation (byte-for-byte the current behavior).
func TestRecommend_FlagOff_IgnoresComplexity(t *testing.T) {
	a, _ := newAdvisor(t, chatCohort(), Config{Enabled: true}) // TierCohorts: false
	ctx := context.Background()
	r1 := a.Recommend(ctx, "ws1", "chat", 1000, "simple", "openai", nil, nil)
	r2 := a.Recommend(ctx, "ws1", "chat", 1000, "complex", "openai", nil, nil)
	r3 := a.Recommend(ctx, "ws1", "chat", 1000, "", "openai", nil, nil)
	if r1.Model != r2.Model || r1.Model != r3.Model || r1.Basis != r2.Basis {
		t.Fatalf("flag OFF must ignore complexity: simple=%q complex=%q empty=%q", r1.Model, r2.Model, r3.Model)
	}
	if r1.Model != "gpt-4o-mini" { // the same best-qpd pick as the non-tiered tests
		t.Fatalf("flag OFF must match the current non-tiered pick (gpt-4o-mini), got %q", r1.Model)
	}
}

func TestRecommend_NeverRecommendsDisallowedModelOrProvider(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdvisor(t, chatCohort(), Config{Enabled: true})

	// Allow only gpt-4o → the best-qpd gpt-4o-mini must NOT be recommended.
	rec := a.Recommend(ctx, "ws1", "chat", 1000, "", "openai", []string{"gpt-4o"}, nil)
	if rec.Model != "gpt-4o" {
		t.Fatalf("must only recommend an allowed model: got %q want gpt-4o", rec.Model)
	}

	// Provider not in the workspace allow-list → none.
	rec = a.Recommend(ctx, "ws1", "chat", 1000, "", "openai", nil, []string{"anthropic"})
	if rec.Basis != BasisNone {
		t.Fatalf("must not recommend a disallowed provider: got %q (%s)", rec.Basis, rec.Model)
	}
}

func TestRecommend_HotPathDoesNotQueryStore(t *testing.T) {
	a, m := newAdvisor(t, chatCohort(), Config{Enabled: true})
	if m.calls.Load() != 1 { // only the initial Refresh
		t.Fatalf("setup: expected 1 store call, got %d", m.calls.Load())
	}
	for i := 0; i < 500; i++ {
		a.Recommend(context.Background(), "ws1", "chat", 1000, "", "openai", nil, nil)
	}
	if m.calls.Load() != 1 {
		t.Fatalf("the request path must NOT query the store: got %d calls (want 1)", m.calls.Load())
	}
	// Only Refresh touches the store.
	_ = a.Refresh(context.Background())
	if m.calls.Load() != 2 {
		t.Fatalf("refresh should query the store: got %d", m.calls.Load())
	}
}

func TestConcurrentRecommendDuringRefresh(t *testing.T) {
	a, _ := newAdvisor(t, chatCohort(), Config{Enabled: true})
	ctx := context.Background()
	var wg sync.WaitGroup

	// Refreshers swapping the map.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = a.Refresh(ctx)
			}
		}()
	}
	// Readers on the hot path.
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				a.Recommend(ctx, "ws1", "chat", 1000, "", "openai", nil, nil)
				_ = a.Status()
				_ = a.Overview()
			}
		}()
	}
	wg.Wait()
}

func TestMetrics_BoundedByBasis(t *testing.T) {
	a, _ := newAdvisor(t, chatCohort(), Config{Enabled: true})
	ctx := context.Background()
	// Many distinct features → must NOT create a series per feature.
	for i := 0; i < 50; i++ {
		a.Recommend(ctx, "ws1", "feature"+string(rune('A'+i)), 1000, "", "openai", nil, nil)
	}
	a.Recommend(ctx, "ws1", "chat", 1000, "", "openai", nil, nil) // a quality_per_dollar hit
	if n := testutil.CollectAndCount(metrics.RoutingRecommendationsTotal); n > 3 {
		t.Fatalf("routing_recommendations_total has %d series — only {basis} (≤3) allowed", n)
	}
}

func TestUnpricedModel_UsesQualityBasis(t *testing.T) {
	// A cohort whose only model is unpriced (cost 0) → quality basis.
	stats := []mining.CohortStat{{FeatureCategory: "vibe", InputTokenRange: "medium", ModelUsed: "mystery-model", ProviderUsed: "openai", AvgQuality: 0.9, SampleCount: 50, DistinctWorkspaces: 6}}
	a, _ := newAdvisor(t, stats, Config{Enabled: true})
	rec := a.Recommend(context.Background(), "ws1", "vibe", 1000, "", "openai", nil, nil)
	if rec.Basis != BasisQuality || rec.Model != "mystery-model" {
		t.Fatalf("unpriced model should fall to quality basis: got basis=%q model=%q", rec.Basis, rec.Model)
	}
}

func TestRoutingHasNoHotPathDependency(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	forbidden := map[string]bool{
		"github.com/talyvor/lens/internal/proxy":       true,
		"github.com/talyvor/lens/internal/api":         true,
		"net/http":                                     true,
		"github.com/talyvor/lens/internal/economy":     true,
		"github.com/talyvor/lens/internal/poolroyalty": true,
		"github.com/talyvor/lens/internal/povi":        true,
	}
	for _, imp := range pkg.Imports {
		if forbidden[imp] {
			t.Errorf("routing must not import %q — the advisor must not depend on the request/HTTP layer OR any ledger/economy/mint package (it only changes which model serves, never mints)", imp)
		}
	}
}
