package localrouter

// multi.go — multi-endpoint local-model Router. Coexists with
// the legacy `LocalRouter` (single Ollama instance) in router.go.
// New code paths should use Router; the proxy still wires the
// legacy LocalRouter for the simple single-Ollama deployment.
//
// Key responsibilities:
//   - Hold an in-memory registry of LocalEndpoints across
//     providers (Ollama / vLLM / llamacpp).
//   - Run async health checks (every 30s) so the request hot
//     path only reads cached health, never blocks.
//   - Select an endpoint per request using one of four strategies
//     (round-robin / least-loaded / lowest-latency / priority).
//   - Track per-endpoint EMA latency + error rate so the
//     least-loaded / lowest-latency strategies have real data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2" // nosemgrep: math-random-used — load-distribution RNG only, NOT security-critical (crypto/rand draw is in benchprobe)
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/safehttp"
)

// ─── constants ───────────────────────────────────

const (
	// DefaultHealthCheckInterval is the cadence for StartHealthChecks.
	// 30s mirrors the legacy LocalRouter — frequent enough that an
	// endpoint going down is reflected in routing within one window,
	// rare enough that we don't hammer hobby-grade local servers.
	DefaultHealthCheckInterval = 30 * time.Second

	// healthCheckTimeout caps a single endpoint probe. Local
	// servers should answer /api/tags or /v1/models in <100ms.
	healthCheckTimeout = 3 * time.Second

	// P1 #10 PR-B quality-bias tuning (all flag-gated; off ⇒ unused).
	// qualityEpsilon is the ε-greedy exploration floor: ε of selections pick uniformly among eligible
	// candidates so every node keeps ≥ ε/n share regardless of score (a new/unscored node still gets
	// traffic; a one-bad-probe node stays in rotation).
	qualityEpsilon = 0.15
	// qualityPriorK is the Bayesian shrinkage strength: q_eff = (n·score + k·0.5)/(n + k). n=0 ⇒ 0.5
	// (neutral, no penalty for new); n≫k ⇒ the running avg. k=5 ⇒ ~5 probes to half-trust the score.
	qualityPriorK = 5.0
	// qualityLatencyScale normalizes AvgLatencyMs into the LowestLatency base affinity.
	qualityLatencyScale = 100.0

	// F4-capstone step B — StrategyPriceAware tuning.
	// defaultGatewayPriceCeiling clamps the node-declared price_per_token (min(declared, ceiling)) so a node
	// can't set the agent's charge. 0.50 = 10× the 0.050 node default (compute_mining.ComputeMineBaseRate),
	// so legitimate nodes — even premium ones a few× the base — are NEVER clamped; only inflated declarations
	// (>0.50) are capped. It is a routing bound, not a mint gate (NOT in the economy force-off block).
	defaultGatewayPriceCeiling = 0.50
	// priceAwareQualityThreshold / priceAwareQualityWarmup are L4's EXACT held-probe C-gate: a candidate is
	// eligible only with benchmark score ≥ threshold AND sample_count ≥ warmup. A cheap-but-unqualified node
	// is ineligible.
	priceAwareQualityThreshold = 0.7
	priceAwareQualityWarmup    = 5

	// latencyEMAWeight controls how fast AvgLatencyMs reacts to
	// new samples. 0.2 = "the latest call counts for 20% of the
	// new average". Spec-mandated.
	latencyEMAWeight = 0.2

	// errorRateEMAWeight is intentionally smaller (0.1 — slow
	// decay) so one transient blip doesn't yank the error rate.
	errorRateEMAWeight = 0.1
)

// ─── types ───────────────────────────────────────

// RoutingStrategy is the selection policy SelectEndpoint applies
// when more than one healthy endpoint can serve a model.
type RoutingStrategy string

const (
	StrategyRoundRobin    RoutingStrategy = "round_robin"
	StrategyLeastLoaded   RoutingStrategy = "least_loaded"
	StrategyLowestLatency RoutingStrategy = "lowest_latency"
	StrategyPriority      RoutingStrategy = "priority"
	// StrategyPriceAware (F4-capstone step B) picks the best price/latency deal among QUALITY-PASSING nodes,
	// with the node-declared price CLAMPED to the gateway ceiling. Opt-in only — never the default; existing
	// routes are unchanged unless a caller explicitly selects it.
	StrategyPriceAware RoutingStrategy = "price_aware"
)

// LocalEndpoint is one configured local-model server. Mutable
// fields (Healthy / LastCheckAt / EMA stats / active count) are
// guarded by Router.mu — never touched outside the router.
type LocalEndpoint struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspace_id,omitempty"` // mining ownership (Batch 2 Item 2)
	URL           string    `json:"url"`
	Provider      string    `json:"provider"`
	Models        []string  `json:"models"`
	Priority      int       `json:"priority"`
	MaxConcurrent int       `json:"max_concurrent"`
	Active        bool      `json:"active"`
	Healthy       bool      `json:"healthy"`
	LastCheckAt   time.Time `json:"last_check_at"`
	AvgLatencyMs  int64     `json:"avg_latency_ms"`
	ErrorRate     float64   `json:"error_rate"`

	// activeCount tracks in-flight requests. Atomic so
	// SelectEndpoint can read it under the router's RLock
	// without taking the count.
	activeCount int64 `json:"-"`

	// quality / qualitySamples are the per-model proof-of-benchmark signal (P1 #10 PR-B): the
	// running-avg score + sample_count from benchmark_node_scores, attached by the periodic
	// quality-sync loop (NOT a per-request DB read). mu-guarded (read under Router.mu like the
	// other fields). Preserved across Register re-registration. Absent model ⇒ neutral prior.
	quality        map[string]float64 `json:"-"`
	qualitySamples map[string]int     `json:"-"`

	// pricePerToken is the node-declared inference_nodes.price_per_token, attached by the periodic
	// price-sync loop (NOT a per-request DB read). mu-guarded. CLAMPED to the gateway ceiling at selection
	// — never used raw (a node cannot set the agent's charge). Preserved across Register re-registration.
	// ≤0 (unsynced) ⇒ treated AS the ceiling at selection, never "free".
	pricePerToken float64 `json:"-"`
}

// Router is the multi-endpoint registry + selector.
type Router struct {
	endpoints  []*LocalEndpoint
	mu         sync.RWMutex
	httpClient *http.Client
	pool       *pgxpool.Pool

	// rrCursor is the round-robin index. Atomic so we don't
	// take a write lock just to advance the counter.
	rrCursor uint64

	// qualityEnabled gates the P1 #10 proof-of-benchmark quality bias (LENS_PROOF_OF_BENCHMARK_ENABLED).
	// nil/false ⇒ SelectEndpoint runs the original deterministic switch VERBATIM (byte-identical).
	qualityEnabled func() bool
	// priceCeilingFn returns the gateway price ceiling (LENS_GATEWAY_PRICE_CEILING) used ONLY by
	// StrategyPriceAware. nil ⇒ defaultGatewayPriceCeiling. Read under mu like qualityEnabled.
	priceCeilingFn func() float64
	// randFloat / randIntN are the selection RNG (math/rand/v2, concurrent-safe by default).
	// Injectable so a test can seed a deterministic source. Distribution-only — NOT security-critical
	// (the crypto/rand draw lives in benchprobe).
	randFloat func() float64
	randIntN  func(n int) int

	// onRequestServed is the compute-mining hook the proxy
	// invokes after a successful served request. Stays nil
	// until SetOnRequestServed wires it (Batch 2 Item 2).
	// requestID (U6) is the SERVER-DERIVED work-product key the live
	// wire-up must supply so the compute mint is idempotent.
	onRequestServed func(nodeID, requestingWorkspace, requestID string, tokens int, latencyMs int64)
}

// SetOnRequestServed installs the mining hook. main.go wires it
// to ComputeMiner.RecordServedRequest. The hook fires from
// NotifyServed below — keeping the router free of a hard
// dependency on the mining package.
func (r *Router) SetOnRequestServed(fn func(nodeID, requestingWorkspace, requestID string, tokens int, latencyMs int64)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRequestServed = fn
}

// SetHTTPClient replaces the underlying HTTP client used for health
// checks and inference calls. main.go calls this to inject a client
// whose TLS config trusts node certificates (e.g. self-signed certs
// on LAN deployments, controlled via LENS_NODE_TLS_SKIP_VERIFY).
func (r *Router) SetHTTPClient(client *http.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.httpClient = client
}

// NotifyServed is the proxy-side entry point: "this endpoint
// just served `tokens` tokens to `requestingWorkspace` in
// `latencyMs`". When a mining hook is wired it's invoked
// synchronously so the proxy can `defer` it and not lose the
// accounting on early returns.
func (r *Router) NotifyServed(nodeID, requestingWorkspace, requestID string, tokens int, latencyMs int64) {
	r.mu.RLock()
	fn := r.onRequestServed
	r.mu.RUnlock()
	if fn != nil {
		fn(nodeID, requestingWorkspace, requestID, tokens, latencyMs)
	}
}

// ─── errors ──────────────────────────────────────

var (
	ErrNoHealthyEndpoint = errors.New("localrouter: no healthy endpoint for model")
	ErrUnknownStrategy   = errors.New("localrouter: unknown routing strategy")
)

// ─── constructors ────────────────────────────────

// NewRouter builds an empty Router. Endpoints get added via
// Register() or via ParseEndpointsConfig + NewRouterFromConfig.
func NewRouter(pool *pgxpool.Pool) *Router {
	return &Router{
		httpClient: &http.Client{Timeout: healthCheckTimeout},
		pool:       pool,
		randFloat:  rand.Float64, // math/rand/v2 top-level — concurrent-safe
		randIntN:   rand.IntN,
	}
}

// NewRouterFromConfig is the env-driven constructor. It calls
// ParseEndpointsConfig and registers each result. Malformed
// entries are logged via the caller (we surface them via
// (results, err) only when *all* entries fail).
func NewRouterFromConfig(pool *pgxpool.Pool, configStr string) *Router {
	r := NewRouter(pool)
	endpoints, _ := ParseEndpointsConfig(configStr)
	for _, e := range endpoints {
		r.Register(e)
	}
	return r
}

// ─── registry ────────────────────────────────────

// Register adds an endpoint. New endpoints start with
// Active=true, Healthy=false — the first health check will flip
// them. Idempotent on ID: re-registering replaces the slot.
func (r *Router) Register(e *LocalEndpoint) {
	if e == nil {
		return
	}
	if e.ID == "" {
		e.ID = fmt.Sprintf("%s-%d", e.Provider, time.Now().UnixNano())
	}
	if e.MaxConcurrent <= 0 {
		e.MaxConcurrent = 16
	}
	e.URL = strings.TrimRight(e.URL, "/")
	e.Active = true
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ex := range r.endpoints {
		if ex.ID == e.ID {
			// Preserve RUNTIME health/stats across a config re-register. The control-plane
			// NodeSyncer re-Registers every reconcile (~30s); without this the endpoint's Healthy
			// flag (set by the periodic sweep) is reset to false on every tick, and SelectEndpoint
			// — the auto-route picker — would only ever see it healthy in a brief race window.
			e.Healthy = ex.Healthy
			e.LastCheckAt = ex.LastCheckAt
			e.AvgLatencyMs = ex.AvgLatencyMs
			e.ErrorRate = ex.ErrorRate
			atomic.StoreInt64(&e.activeCount, atomic.LoadInt64(&ex.activeCount))
			// P1 #10 PR-B: preserve the benchmark quality the sync loop attached, across the
			// NodeSyncer's ~30s re-register (same reason as the health/stats preservation above).
			e.quality = ex.quality
			e.qualitySamples = ex.qualitySamples
			e.pricePerToken = ex.pricePerToken // F4 step B: preserve synced price across re-register
			r.endpoints[i] = e
			return
		}
	}
	r.endpoints = append(r.endpoints, e)
}

// Remove deletes an endpoint by ID. Returns false when no such
// ID exists — callers use that to surface a 404.
func (r *Router) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.endpoints {
		if e.ID == id {
			r.endpoints = append(r.endpoints[:i], r.endpoints[i+1:]...)
			return true
		}
	}
	return false
}

// Get returns a deep-copy of one endpoint for read-only callers
// (admin API). Returns nil if the ID is unknown.
func (r *Router) Get(id string) *LocalEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.endpoints {
		if e.ID == id {
			cp := *e
			cp.Models = append([]string{}, e.Models...)
			return &cp
		}
	}
	return nil
}

// List returns a snapshot of all endpoints. Safe to expose
// directly via the admin API.
func (r *Router) List() []*LocalEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*LocalEndpoint, 0, len(r.endpoints))
	for _, e := range r.endpoints {
		cp := *e
		cp.Models = append([]string{}, e.Models...)
		out = append(out, &cp)
	}
	return out
}

// ─── health checks ───────────────────────────────

// CheckHealth probes a single endpoint and flips its Healthy
// field. Returns the final state for the caller (so the admin
// "check now" endpoint can return it inline).
func (r *Router) CheckHealth(ctx context.Context, endpoint *LocalEndpoint) bool {
	if endpoint == nil {
		return false
	}
	healthy := r.probe(ctx, endpoint)
	r.mu.Lock()
	endpoint.Healthy = healthy
	endpoint.LastCheckAt = time.Now()
	r.mu.Unlock()
	return healthy
}

// probe runs the provider-specific health request. Tests
// override the httpClient with one wired to httptest.Server.
func (r *Router) probe(ctx context.Context, endpoint *LocalEndpoint) bool {
	path := "/health"
	switch endpoint.Provider {
	case "ollama":
		path = "/api/tags"
	case "vllm":
		path = "/v1/models"
	case "llamacpp":
		path = "/health"
	}
	return r.probeURL(ctx, endpoint.URL, path)
}

// probeURL GETs baseURL+path and reports a 200. baseURL is caller-supplied, so it is checked first.
func (r *Router) probeURL(ctx context.Context, baseURL, path string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	if err := safehttp.CheckURL(baseURL); err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return false
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	// Best-effort consume — we don't care about the body for
	// llamacpp/vllm. For ollama we *could* parse /api/tags here,
	// but the spec says "model available" is satisfied by the
	// configured Models list, so we just confirm the endpoint
	// itself is up.
	_, _ = io.Copy(io.Discard, resp.Body)
	return true
}

// CheckHealthByID looks up the live endpoint by ID (not a copy)
// and probes it. Returns ErrNoHealthyEndpoint when the ID is
// unknown — admin "check now" uses this so the registry state
// reflects the fresh probe.
func (r *Router) CheckHealthByID(ctx context.Context, id string) (*LocalEndpoint, error) {
	r.mu.RLock()
	var target *LocalEndpoint
	for _, e := range r.endpoints {
		if e.ID == id {
			target = e
			break
		}
	}
	r.mu.RUnlock()
	if target == nil {
		return nil, ErrNoHealthyEndpoint
	}
	r.CheckHealth(ctx, target)
	return r.Get(id), nil
}

// StartHealthChecks runs CheckHealth on every endpoint at the
// given interval. Returns immediately — caller is responsible
// for cancelling ctx to stop the loop. Health checks are async
// so they never block requests.
func (r *Router) StartHealthChecks(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHealthCheckInterval
	}
	go func() {
		// Initial sweep so the first request after startup sees
		// a populated health state.
		r.sweepHealth(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sweepHealth(ctx)
			}
		}
	}()
}

func (r *Router) sweepHealth(ctx context.Context) {
	for _, e := range r.List() { // List() returns copies — safe
		// We can't pass the copy to CheckHealth because we want
		// to mutate the original. Look up the real one by ID.
		r.mu.RLock()
		var target *LocalEndpoint
		for _, real := range r.endpoints {
			if real.ID == e.ID {
				target = real
				break
			}
		}
		r.mu.RUnlock()
		if target != nil {
			r.CheckHealth(ctx, target)
		}
	}
}

// ─── selection ───────────────────────────────────

// SelectEndpoint picks one healthy, active endpoint that serves
// `model`. Returns ErrNoHealthyEndpoint when nothing matches.
// Never returns silently — if zero healthy endpoints, the
// caller gets an explicit error and can fall back to cloud.
func (r *Router) SelectEndpoint(model string, strategy RoutingStrategy) (*LocalEndpoint, error) {
	qOn := r.qualityEnabled != nil && r.qualityEnabled()
	paOn := strategy == StrategyPriceAware // F4 step B — opt-in only

	r.mu.RLock()
	candidates := make([]*LocalEndpoint, 0, len(r.endpoints))
	var qeff []float64           // captured UNDER the lock (the quality maps are mu-guarded) — only when qOn
	var paSig []priceAwareSignal // price/quality/latency captured under the lock — only when paOn
	for _, e := range r.endpoints {
		if !e.Active || !e.Healthy {
			continue
		}
		if !servesModel(e, model) {
			continue
		}
		candidates = append(candidates, e)
		if qOn {
			qeff = append(qeff, qEffLocked(e, model))
		}
		if paOn {
			paSig = append(paSig, priceAwareSignal{price: e.pricePerToken, score: e.quality[model], samples: e.qualitySamples[model], latencyMs: e.AvgLatencyMs})
		}
	}
	ceiling := defaultGatewayPriceCeiling
	if paOn && r.priceCeilingFn != nil {
		ceiling = r.priceCeilingFn()
	}
	r.mu.RUnlock()

	if len(candidates) == 0 {
		return nil, ErrNoHealthyEndpoint
	}

	// F4 step B: explicit price-aware selection — deterministic, C-gated, clamped. Opt-in, so it takes
	// precedence over the ε-greedy quality path (it already folds quality in via the hard C-gate).
	if paOn {
		return r.selectPriceAware(candidates, paSig, ceiling)
	}

	// P1 #10 PR-B: quality-weighted ε-greedy selection (composes WITH the strategy). Flag-off skips
	// this entirely and runs the original deterministic switch below VERBATIM (byte-identical).
	if qOn {
		return r.selectQualityWeighted(candidates, qeff, strategy)
	}

	switch strategy {
	case StrategyRoundRobin, "":
		idx := atomic.AddUint64(&r.rrCursor, 1)
		return candidates[(int(idx)-1)%len(candidates)], nil
	case StrategyLeastLoaded:
		best := candidates[0]
		bestN := atomic.LoadInt64(&best.activeCount)
		for _, c := range candidates[1:] {
			n := atomic.LoadInt64(&c.activeCount)
			if n < bestN {
				best, bestN = c, n
			}
		}
		return best, nil
	case StrategyLowestLatency:
		best := candidates[0]
		for _, c := range candidates[1:] {
			// Endpoints that have never been measured (Avg=0)
			// lose to ones that have data; otherwise pick the
			// lowest measurement.
			if best.AvgLatencyMs == 0 && c.AvgLatencyMs > 0 {
				best = c
				continue
			}
			if c.AvgLatencyMs > 0 && c.AvgLatencyMs < best.AvgLatencyMs {
				best = c
			}
		}
		return best, nil
	case StrategyPriority:
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.Priority < best.Priority {
				best = c
			}
		}
		return best, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, strategy)
	}
}

// SetQualityEnabled wires the P1 #10 quality-bias gate (LENS_PROOF_OF_BENCHMARK_ENABLED). nil/false ⇒
// SelectEndpoint is byte-identical to today. A setter so the constructor signature stays put.
func (r *Router) SetQualityEnabled(fn func() bool) {
	r.mu.Lock()
	r.qualityEnabled = fn
	r.mu.Unlock()
}

// SetRand injects the selection RNG (test-only; production uses math/rand/v2 top-level). Lets a test
// seed a deterministic source to assert the bounded shares.
func (r *Router) SetRand(float64Fn func() float64, intNFn func(int) int) {
	r.mu.Lock()
	r.randFloat, r.randIntN = float64Fn, intNFn
	r.mu.Unlock()
}

// SetPriceCeiling wires the gateway price ceiling used by StrategyPriceAware (LENS_GATEWAY_PRICE_CEILING).
// nil ⇒ defaultGatewayPriceCeiling. A setter so the constructor signature stays put. Only StrategyPriceAware
// reads it — other strategies are unaffected.
func (r *Router) SetPriceCeiling(fn func() float64) {
	r.mu.Lock()
	r.priceCeilingFn = fn
	r.mu.Unlock()
}

// priceAwareSignal is the per-candidate price/quality/latency snapshot captured under Router.mu (the price +
// quality maps are mu-guarded), so selectPriceAware never touches shared state post-lock.
type priceAwareSignal struct {
	price     float64
	score     float64
	samples   int
	latencyMs int64
}

// effectivePrice clamps a node-declared price to the gateway ceiling: min(declared, ceiling). An unknown or
// non-positive declared price (an unsynced node) is treated AS the ceiling — a node can't declare ≤0 to look
// free. THE LOAD-BEARING GUARD: the node-declared price can never move the charge above the ceiling, and
// can't be gamed below it via an out-of-band value.
func effectivePrice(declared, ceiling float64) float64 {
	if declared <= 0 || declared > ceiling {
		return ceiling
	}
	return declared
}

// selectPriceAware picks the best price/latency deal among the QUALITY-PASSING candidates. C-gate FIRST
// (reuse L4's held-probe bar: score ≥ threshold AND samples ≥ warmup) — a cheap but unqualified node is
// ineligible however cheap. Then rank by cost = effective_price × (1 + latency/scale), lowest wins: price is
// ONE factor, latency the other (never a pure race-to-the-bottom). Deterministic. ErrNoHealthyEndpoint if
// none qualify.
func (r *Router) selectPriceAware(candidates []*LocalEndpoint, sig []priceAwareSignal, ceiling float64) (*LocalEndpoint, error) {
	var best *LocalEndpoint
	var bestCost float64
	for i, c := range candidates {
		s := sig[i]
		if s.samples < priceAwareQualityWarmup || s.score < priceAwareQualityThreshold {
			continue // held-probe C-gate: unqualified ⇒ ineligible
		}
		eff := effectivePrice(s.price, ceiling)
		lat := float64(s.latencyMs)
		if lat <= 0 {
			lat = qualityLatencyScale // unmeasured latency = neutral
		}
		cost := eff * (1 + lat/qualityLatencyScale)
		if best == nil || cost < bestCost {
			best, bestCost = c, cost
		}
	}
	if best == nil {
		return nil, ErrNoHealthyEndpoint // all candidates failed the C-gate
	}
	return best, nil
}

// SetNodePrice attaches the node-declared price_per_token to a registered endpoint (called by the periodic
// price-sync loop, NOT per request). No-op if the node isn't registered.
func (r *Router) SetNodePrice(nodeID string, price float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.endpoints {
		if e.ID == nodeID {
			e.pricePerToken = price
			return
		}
	}
}

// StartPriceSync periodically loads inference_nodes.price_per_token into the routers' in-memory price (F4
// step B). BLOCKING (runs the ticker in the caller's goroutine until ctx is done) so it composes with leader
// election. Reads from the router's pool — SelectEndpoint never touches the DB.
func (r *Router) StartPriceSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHealthCheckInterval
	}
	r.syncPrice(ctx) // initial load
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.syncPrice(ctx)
		}
	}
}

func (r *Router) syncPrice(ctx context.Context) {
	if r.pool == nil {
		return
	}
	rows, err := r.pool.Query(ctx, `SELECT id, price_per_token FROM inference_nodes WHERE active = true`)
	if err != nil {
		return // best-effort; routing keeps the last-synced price
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID string
		var price float64
		if err := rows.Scan(&nodeID, &price); err != nil {
			return
		}
		r.SetNodePrice(nodeID, price)
	}
}

// qEffLocked returns the shrinkage-adjusted effective quality for (endpoint, model). Caller holds
// Router.mu (the quality maps are mu-guarded). No row / no samples ⇒ exactly the neutral prior 0.5.
func qEffLocked(e *LocalEndpoint, model string) float64 {
	n := float64(e.qualitySamples[model])
	score := e.quality[model]
	return (n*score + qualityPriorK*0.5) / (n + qualityPriorK)
}

// selectQualityWeighted composes the per-model quality (qeff, captured under the lock) with the
// strategy's OWN signal (base affinity) and picks ε-greedily: ε of the time uniformly among
// candidates (the exploration floor), else weighted-random by base_i·qeff_i. Quality can never zero a
// node out and never make one 100%. Returns ErrUnknownStrategy for an unknown strategy (parity).
func (r *Router) selectQualityWeighted(candidates []*LocalEndpoint, qeff []float64, strategy RoutingStrategy) (*LocalEndpoint, error) {
	base := make([]float64, len(candidates))
	switch strategy {
	case StrategyRoundRobin, "":
		for i := range base {
			base[i] = 1
		}
	case StrategyLeastLoaded:
		for i, c := range candidates {
			base[i] = 1.0 / (1.0 + float64(atomic.LoadInt64(&c.activeCount)))
		}
	case StrategyLowestLatency:
		for i, c := range candidates {
			lat := float64(c.AvgLatencyMs)
			if lat <= 0 {
				lat = qualityLatencyScale // unmeasured = neutral base
			}
			base[i] = 1.0 / (1.0 + lat/qualityLatencyScale)
		}
	case StrategyPriority:
		minP := candidates[0].Priority
		for _, c := range candidates[1:] {
			if c.Priority < minP {
				minP = c.Priority
			}
		}
		for i, c := range candidates {
			base[i] = math.Pow(2, -float64(c.Priority-minP))
		}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, strategy)
	}

	// ε-greedy exploration floor: every eligible node keeps ≥ ε/n share regardless of score.
	if r.randFloat() < qualityEpsilon {
		return candidates[r.randIntN(len(candidates))], nil
	}

	// Exploitation: weighted-random by base_i · qeff_i.
	weights := make([]float64, len(candidates))
	total := 0.0
	for i := range candidates {
		weights[i] = base[i] * qeff[i]
		total += weights[i]
	}
	if total <= 0 { // degenerate (shouldn't happen: qeff ≥ floor > 0) — fall back to uniform
		return candidates[r.randIntN(len(candidates))], nil
	}
	target := r.randFloat() * total
	for i := range candidates {
		target -= weights[i]
		if target <= 0 {
			return candidates[i], nil
		}
	}
	return candidates[len(candidates)-1], nil
}

// UpdateQuality attaches a per-model benchmark score to a registered endpoint (called by the periodic
// quality-sync loop, NOT per request). No-op if the node isn't registered.
func (r *Router) UpdateQuality(nodeID, model string, score float64, samples int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.endpoints {
		if e.ID != nodeID {
			continue
		}
		if e.quality == nil {
			e.quality = make(map[string]float64)
			e.qualitySamples = make(map[string]int)
		}
		e.quality[model] = score
		e.qualitySamples[model] = samples
		return
	}
}

// StartQualitySync periodically loads benchmark_node_scores into the routers' in-memory quality
// (P1 #10 PR-B). BLOCKING (runs the ticker loop in the caller's goroutine until ctx is done) so it
// composes with leader election. Reads from the router's pool — SelectEndpoint never touches the DB.
func (r *Router) StartQualitySync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHealthCheckInterval
	}
	r.syncQuality(ctx) // initial load
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.syncQuality(ctx)
		}
	}
}

func (r *Router) syncQuality(ctx context.Context) {
	if r.pool == nil {
		return
	}
	rows, err := r.pool.Query(ctx, `SELECT node_id, model, score, sample_count FROM benchmark_node_scores`)
	if err != nil {
		return // best-effort; routing keeps the last-synced quality
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, model string
		var score float64
		var samples int
		if err := rows.Scan(&nodeID, &model, &score, &samples); err != nil {
			return
		}
		r.UpdateQuality(nodeID, model, score, samples)
	}
}

func servesModel(e *LocalEndpoint, model string) bool {
	if len(e.Models) == 0 {
		// Empty Models list = "serves anything". Useful for
		// generic local proxies; risky for shared deployments,
		// so callers should set Models explicitly.
		return true
	}
	for _, m := range e.Models {
		if m == model || strings.HasPrefix(model, m) {
			return true
		}
	}
	return false
}

// ─── policy decision ─────────────────────────────

// ShouldRouteLocally is the "decide before dialing" check the
// proxy calls. requireLocal forces a true answer (the caller
// has already decided); otherwise we require a healthy endpoint
// *and* an average latency under maxLatencyMs (0 = no cap).
func (r *Router) ShouldRouteLocally(model string, maxLatencyMs int, requireLocal bool) bool {
	if requireLocal {
		return true
	}
	ep, err := r.SelectEndpoint(model, StrategyLowestLatency)
	if err != nil {
		return false
	}
	if maxLatencyMs <= 0 {
		return true
	}
	// An endpoint that's never been measured has AvgLatencyMs=0
	// — treat that as "unknown but allowed" so the first call
	// still routes locally and seeds the EMA.
	if ep.AvgLatencyMs == 0 {
		return true
	}
	return ep.AvgLatencyMs < int64(maxLatencyMs)
}

// ─── request tracking ────────────────────────────

// RecordRequest folds one observation into the endpoint's EMA
// stats. latencyMs may be 0 for a connection-refused error.
func (r *Router) RecordRequest(endpointID string, latencyMs int64, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.endpoints {
		if e.ID != endpointID {
			continue
		}
		if e.AvgLatencyMs == 0 {
			e.AvgLatencyMs = latencyMs
		} else {
			e.AvgLatencyMs = int64(float64(e.AvgLatencyMs)*(1-latencyEMAWeight) +
				float64(latencyMs)*latencyEMAWeight)
		}
		var sampleErr float64
		if !success {
			sampleErr = 1.0
		}
		e.ErrorRate = e.ErrorRate*(1-errorRateEMAWeight) + sampleErr*errorRateEMAWeight
		return
	}
}

// IncActive / DecActive are called by the proxy around an
// outgoing call so SelectEndpoint(StrategyLeastLoaded) has data.
func (r *Router) IncActive(endpointID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.endpoints {
		if e.ID == endpointID {
			atomic.AddInt64(&e.activeCount, 1)
			return
		}
	}
}

func (r *Router) DecActive(endpointID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.endpoints {
		if e.ID == endpointID {
			atomic.AddInt64(&e.activeCount, -1)
			return
		}
	}
}

// ─── config parsing ──────────────────────────────

// ParseEndpointsConfig parses LENS_LOCAL_ENDPOINTS-style strings:
//
//	provider:url:model1,model2;provider:url:model3
//
// Lenient by design — malformed segments are skipped and counted
// in the returned error (callers can log it; an empty config
// returns nil, nil). A returned slice + non-nil error means
// "we got some endpoints, but some entries were bad".
func ParseEndpointsConfig(config string) ([]*LocalEndpoint, error) {
	if strings.TrimSpace(config) == "" {
		return nil, nil
	}
	var out []*LocalEndpoint
	var skipped []string
	for _, entry := range strings.Split(config, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Split on ":" but URLs contain colons too — so we treat
		// the LAST piece as models, the FIRST as provider, and
		// reassemble everything in the middle as the URL.
		parts := strings.Split(entry, ":")
		if len(parts) < 3 {
			skipped = append(skipped, entry)
			continue
		}
		provider := parts[0]
		models := parts[len(parts)-1]
		url := strings.Join(parts[1:len(parts)-1], ":")
		if provider == "" || url == "" || models == "" {
			skipped = append(skipped, entry)
			continue
		}
		switch provider {
		case "ollama", "vllm", "llamacpp":
			// supported
		default:
			skipped = append(skipped, entry)
			continue
		}
		modelList := []string{}
		for _, m := range strings.Split(models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				modelList = append(modelList, m)
			}
		}
		if len(modelList) == 0 {
			skipped = append(skipped, entry)
			continue
		}
		out = append(out, &LocalEndpoint{
			Provider:      provider,
			URL:           strings.TrimRight(url, "/"),
			Models:        modelList,
			Active:        true,
			MaxConcurrent: 16,
		})
	}
	if len(skipped) > 0 {
		return out, fmt.Errorf("localrouter: skipped %d malformed entries: %s",
			len(skipped), strings.Join(skipped, " | "))
	}
	return out, nil
}

// ─── small helpers used by api server ────────────

// EndpointsJSON is the admin-API shape: same as LocalEndpoint
// but with the unexported activeCount surfaced as ActiveCount.
type EndpointsJSON struct {
	*LocalEndpoint
	ActiveCount int64 `json:"active_count"`
}

// MarshalJSON folds activeCount into the JSON output without
// changing the struct definition.
func (e *LocalEndpoint) MarshalJSON() ([]byte, error) {
	type alias LocalEndpoint
	return json.Marshal(&struct {
		*alias
		ActiveCount int64 `json:"active_count"`
	}{
		alias:       (*alias)(e),
		ActiveCount: atomic.LoadInt64(&e.activeCount),
	})
}
