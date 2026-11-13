// Package metrics owns every Prometheus metric Lens exposes. One registry
// (the client_golang default), one place to define names + labels, so
// cardinality stays under control. All metric names use the "lens_" namespace
// prefix.
//
// Cardinality discipline: labels are bounded sets only — route PATTERNS (not
// raw paths), provider names, cache layers, normalized status codes, breaker
// providers, and the (bounded) workspace tenant set. Never request IDs or any
// unbounded value.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// latencyBuckets covers sub-millisecond to multi-second HTTP/upstream latency.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// ledgerBuckets is tuned for the token-ledger write hot path (sub-ms to ~1s) so
// the 100ms p99 alert has resolution around its threshold.
var ledgerBuckets = []float64{0.0001, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

var (
	// ─── pre-existing (kept; do not rename) ───
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_requests_total", Help: "Total proxied requests, by provider and outcome."},
		[]string{"provider", "outcome"},
	)
	CacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_cache_hits_total", Help: "Cache hits, by cache layer."},
		[]string{"layer"},
	)
	TokensSavedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_tokens_saved_total", Help: "Tokens saved, by provider and strategy."},
		[]string{"provider", "strategy"},
	)

	// ─── HTTP layer (Upgrade 11) ───
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_http_requests_total", Help: "HTTP requests, by route pattern, method, and status code."},
		[]string{"route", "method", "status"},
	)
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "lens_http_request_duration_seconds", Help: "HTTP request latency, by route and method.", Buckets: latencyBuckets},
		[]string{"route", "method"},
	)
	InflightRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_inflight_requests", Help: "In-flight HTTP requests currently being served."},
	)

	// ─── upstream providers ───
	UpstreamRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_upstream_provider_requests_total", Help: "Upstream provider requests, by provider and status."},
		[]string{"provider", "status"},
	)
	UpstreamDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "lens_upstream_provider_duration_seconds", Help: "Upstream provider call latency, by provider.", Buckets: latencyBuckets},
		[]string{"provider"},
	)

	// ─── cache ───
	CacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_cache_misses_total", Help: "Cache misses, by cache layer."},
		[]string{"layer"},
	)

	// ─── circuit breaker ───
	CircuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_circuit_breaker_state", Help: "Circuit breaker state by provider: 0=closed, 1=open, 2=half-open."},
		[]string{"provider"},
	)

	// ─── rate limiting ───
	// NOTE: workspace is a bounded tenant set; if a deployment has unbounded
	// workspaces, normalize this label (e.g. bucket unknown → "other").
	RateLimitRejectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_rate_limit_rejections_total", Help: "Rate-limit rejections (HTTP 429), by workspace."},
		[]string{"workspace"},
	)
	RateLimitRedisErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_rate_limit_redis_errors_total", Help: "Rate-limiter Redis calls that failed; that request was counted in this instance's memory instead."},
	)

	// ─── token ledger hot path ───
	TokenLedgerWriteDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "lens_token_ledger_write_duration_seconds", Help: "Token-ledger write latency (the minting hot path).", Buckets: ledgerBuckets},
	)

	// ─── economy ───
	TokensMintedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_tokens_minted_total", Help: "Total LENS tokens minted."},
	)
	LXCConvertedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_lxc_converted_total", Help: "Total LXC minted via LENS→LXC conversion."},
	)

	// ─── high availability ───
	HAInstanceCount = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_ha_instance_count", Help: "Active Lens instances seen in the HA registry (0 when HA disabled)."},
	)

	// ReplicaLagSeconds is the read-replica replay lag in seconds (U8/U9). It
	// stays 0 when no replica is configured (LENS_DB_REPLICA_URL unset) or the
	// queried node is not in recovery; only the lag monitor populates it, and
	// only when a replica is wired.
	ReplicaLagSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_db_replica_lag_seconds", Help: "Read-replica replay lag in seconds (0 when no replica configured)."},
	)

	// ─── budget governance (Upgrade 19) ───
	// scope is bounded (workspace/team/sprint). scope_id is potentially
	// unbounded (team / sprint ids), so the budgets service gates these two
	// gauges behind a cardinality guard before calling the setters below —
	// never emit a scope_id label without that guard. The two counters use
	// only the bounded {scope} label and are safe to emit unguarded.
	BudgetSpentUSD = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_budget_spent_usd", Help: "Current spend (USD) per budget, by scope and scope_id."},
		[]string{"scope", "scope_id"},
	)
	BudgetUtilizationRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_budget_utilization_ratio", Help: "Spend / limit ratio per budget, by scope and scope_id."},
		[]string{"scope", "scope_id"},
	)
	BudgetThresholdCrossedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_budget_threshold_crossed_total", Help: "Alert thresholds crossed, by scope."},
		[]string{"scope"},
	)
	BudgetBlocksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_budget_blocks_total", Help: "Requests blocked by a hard_block budget, by scope."},
		[]string{"scope"},
	)
	// AgentSettleFailuresTotal counts an agent's served answer whose settle still failed after its retries
	// (B35.2): its hold is left for the stranded sweeper to refund, so the answer is not billed. Bounded {path}:
	// reservation, pooled or debit.
	AgentSettleFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_agent_settle_failures_total", Help: "Agent answers served whose settle failed after its retries, by path — each is an answer not billed as it should be."},
		[]string{"path"},
	)
	// AgentHoldCutsTotal counts an agent's answer whose delivered cost was above its hold, so its settle charged
	// the hold and wrote the rest off (B35.3). Each is logged at ERROR with the reservation and the amounts.
	AgentHoldCutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_agent_hold_cuts_total", Help: "Agent answers whose delivered cost was above the hold, so the settle charged the hold and wrote the rest off, by model."},
		[]string{"model"},
	)

	// ─── cost forecasting (Upgrade 20) ───
	// Both carry only the bounded {scope} label (workspace/team/sprint).
	// scope_id is deliberately OMITTED to keep cardinality bounded by
	// construction — same discipline as budgets, no unbounded id labels.
	ForecastProjectedUSD = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_forecast_projected_usd", Help: "Most recent projected period-total spend (USD), by scope. A projection, not actual spend."},
		[]string{"scope"},
	)
	ForecastWillExceedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_forecast_will_exceed_budget_total", Help: "Times a forecast projected a budget would be exceeded, by scope."},
		[]string{"scope"},
	)

	// ─── cost anomaly detection (Upgrade 21) ───
	// Bounded labels only: scope (issue/team/sprint) and severity
	// (low/warn/high). unit_id / issue_id are deliberately OMITTED — they're
	// unbounded, so they never become label values.
	AnomaliesDetectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_anomalies_detected_total", Help: "Cost anomalies detected (deduped per unit+period), by scope and severity."},
		[]string{"scope", "severity"},
	)
	AnomalyMaxFactor = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_anomaly_max_factor", Help: "Largest cost factor over baseline median observed in the most recent scan, by scope."},
		[]string{"scope"},
	)

	// RoyaltyDetectorFlagged is the count of flagged findings from the most recent
	// detector sweep, by economy (cache|distill) and detector (volume|bilateral|similarity).
	// Alert on > 0. Observe-only — the sweep never resolves/adjudicates.
	RoyaltyDetectorFlagged = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_royalty_detector_flagged", Help: "Flagged findings from the most recent royalty detector sweep, by economy and detector."},
		[]string{"economy", "detector"},
	)

	// DetectorLastRunAgeSeconds is the seconds since a detector/settlement-clearer last
	// ran (Phase-4a Item 4). Under settlement fail-closed a stalled detector strands
	// mints, so a rising age must be alertable (e.g. alert if > 15m). 0 on a fresh run.
	DetectorLastRunAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_detector_last_run_age_seconds", Help: "Seconds since the named detector/clearer last ran — a stall signal for the fail-closed settlement layer."},
		[]string{"detector"},
	)

	// AnnotationReputationEvents counts reputation_events appended, by kind
	// (agreement_outcome|decay|admin_reset) — so reputation movement is observable
	// (collusion-visibility). Reputation is money-decoupled; this is observe-only.
	AnnotationReputationEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_annotation_reputation_events_total", Help: "Annotation reputation events appended, by kind."},
		[]string{"kind"},
	)

	// ─── executive ROI reporting (Upgrade 24) ───
	// Bounded label: period (monthly/weekly/total). No workspace/unit labels.
	ROIReportsGeneratedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_roi_report_generated_total", Help: "Executive ROI reports generated, by period."},
		[]string{"period"},
	)
	ROIReportDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "lens_roi_report_generation_duration_seconds", Help: "ROI report generation latency (an expensive aggregation).", Buckets: latencyBuckets},
	)

	// ─── routing intelligence (Upgrade 22) ───
	// Bounded label: basis (quality_per_dollar | quality | none). No
	// feature/model/workspace labels.
	RoutingRecommendationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_routing_recommendations_total", Help: "Routing recommendations computed on the request path, by basis."},
		[]string{"basis"},
	)
	RoutingIntelligenceAppliedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_routing_intelligence_applied_total", Help: "Requests whose model was changed from the default by routing intelligence."},
	)
	// RoutingBrainAppliedTotal / RoutingBrainAdvisoryTotal count the H8.1 Routing
	// Brain's serve-path outcomes: APPLIED = an autonomous workspace's route became
	// the brain's pick (within the hard floor); ADVISORY = a recommendation was
	// surfaced but the router's model stood (advisory posture, or an autonomous floor
	// fallback). Mint-free — these are routing observations, never a mint signal.
	RoutingBrainAppliedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_routing_brain_applied_total", Help: "Auto-route requests whose model became the Routing Brain's autonomous pick (within the hard floor)."},
	)
	RoutingBrainAdvisoryTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_routing_brain_advisory_total", Help: "Auto-route requests where a Routing Brain recommendation was surfaced but the router's model stood."},
	)
	RoutingFallbackTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_routing_fallback_total", Help: "Requests that fell back to the default route (no recommendation / below sample floor)."},
	)
	// RoutingTierGatedTotal counts auto-route recommendations SUPPRESSED by the
	// Shape-1 work-tier gate (#198), by reason. Subtractive only — a gated
	// request takes the base route, so it is also counted in RoutingFallbackTotal.
	// UnpricedModelRequests counts requests priced on a DERIVED FALLBACK rate because the model is not
	// in the catalog. ⚠ ALERT ON THIS: any non-zero value means a provider is serving a model Lens does
	// not know, so that traffic is being billed on a guess (and, before the fix that added this counter,
	// was being served FREE). The remedy is to add the model via LENS_MODEL_CATALOG_OVERRIDES.
	UnpricedModelRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_unpriced_model_requests_total", Help: "Requests priced on a derived fallback rate because the model is absent from the catalog, by model and purpose (hold/charge). Non-zero means under-billing until the catalog is updated."},
		[]string{"model", "purpose"},
	)

	// ModelWatchUnpricedModels is how many models the providers currently serve that the catalog cannot
	// price EXACTLY. ⚠ ALERT ON > 0: that traffic is billed on a derived floor, i.e. under-recovered,
	// until a person adds the published rate. A gauge, not a counter — it is a current condition that
	// goes back to 0 when the catalog is fixed, so it can be alerted on without rate() windows.
	ModelWatchUnpricedModels = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_modelwatch_unpriced_models", Help: "Models a provider serves that the catalog cannot price exactly. >0 means under-billing until the catalog is updated."},
	)

	// ModelWatchSinkConfigured is 1 when catalog-drift alerts have a delivery sink and 0 when they do
	// not. ⚠ ALERT ON == 0. It exists because an unconfigured webhook is indistinguishable, from inside
	// the code and from the changelog, from a working alert — the failure the Track spend emitter is in
	// right now (nil when unset, unset everywhere, so no spend alert has ever been delivered and
	// nothing says so). This metric makes "nobody is listening" a visible state rather than a silence.
	ModelWatchSinkConfigured = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_modelwatch_sink_configured", Help: "1 if catalog-drift alerts have a delivery sink, 0 if they can only reach a log. 0 means alerts do not reach a person."},
	)

	RoutingTierGatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_routing_tier_gated_total", Help: "Auto-route recommendations suppressed by the work-tier gate, by reason (sensitivity_optout / downgrade_veto)."},
		[]string{"reason"},
	)

	// ─── vision / multimodal routing (Upgrade 15) ───
	// modality is a bounded, low-cardinality label (text / image / audio /
	// document / comma-joined sets). No per-model or per-workspace labels.
	RequestsByModalityTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_requests_by_modality_total", Help: "Requests by detected modality."},
		[]string{"modality"},
	)
	VisionRouteRedirectsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_vision_route_redirects_total", Help: "Requests redirected to a modality-capable model (auto-route)."},
	)
	ModalityUnsupportedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_modality_unsupported_total", Help: "Requests failed fast because no configured model supports the requested modality."},
	)
	// source is a bounded set: provider_usage (billed on the provider's
	// reported token counts) vs estimated (len/4 fallback when no usage was
	// surfaced). Lets us watch how often metering falls back to estimation.
	SpendRecordsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_spend_records_total", Help: "Spend records written, by token-count source (provider_usage|estimated)."},
		[]string{"source"},
	)

	// ─── DISTILL (document conversion, stage 2) ───
	// result is a bounded set: hit | miss. Never document content/hash as a
	// label.
	DistillCacheTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_distill_cache_total", Help: "DISTILL cache lookups, by result (hit|miss) and kind (conversion|ocr)."},
		[]string{"result", "kind"},
	)
	// Total input tokens saved by distillation (raw doc estimate minus the
	// converted Markdown estimate, same len/4 basis the gateway bills on),
	// summed across every distillation RESULT RETURNED — cache hits included,
	// because the savings is realized each time distilled Markdown is used in
	// place of the raw document (the per-request value, which stage 3 attaches
	// to token_events). NOT a count of unique conversions.
	DistillTokensSavedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_distill_tokens_saved_total", Help: "Input tokens saved by document distillation (len/4 basis), summed across results returned (cache hits included — realized per use)."},
	)
	// ─── DISTILL vision fallback (stage 5) ───
	// The EXPENSIVE path: a text-less document (scanned/encrypted PDF) routed to
	// a vision model for OCR. result is a bounded set: ok | empty | error.
	// Vision OCR is SPEND, never a saving — its token cost lives in the separate
	// cost counter below and must never be added to tokens_saved.
	DistillVisionFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_distill_vision_fallback_total", Help: "DISTILL vision-OCR fallback attempts for text-less documents, by result (ok|empty|error)."},
		[]string{"result"},
	)
	DistillVisionTokensCostTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_distill_vision_tokens_cost_total", Help: "Tokens SPENT by the vision-OCR fallback (the expensive path — a cost, never a saving)."},
	)

	// ─── guardrails (Upgrade 13) ───
	// type (pii/injection/topic/word_filter/custom/output_validation) and
	// action (block/redact/warn/allow) are bounded sets. Never workspace or
	// content as a label.
	GuardrailTriggeredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_guardrail_triggered_total", Help: "Guardrail rules triggered, by type and action."},
		[]string{"type", "action"},
	)
	GuardrailBlocksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_guardrail_blocks_total", Help: "Requests/responses blocked by a guardrail, by type."},
		[]string{"type"},
	)
	GuardrailRedactionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_guardrail_redactions_total", Help: "Guardrail redactions applied, by type."},
		[]string{"type"},
	)

	// ─── evaluation pipeline (Upgrade 17) ───
	// result is the bounded set {pass, fail, unknown}. Never per-dataset or
	// per-workspace as a label — those are unbounded.
	EvalRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_eval_runs_total", Help: "Eval dataset runs completed, by aggregate result."},
		[]string{"result"},
	)
	EvalRegressionsDetectedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_eval_regressions_detected_total", Help: "Per-case quality regressions detected across eval runs."},
	)
	ABSignificantResultsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_ab_significant_results_total", Help: "A/B experiments whose quality difference reached statistical significance."},
	)

	// ─── PoVI receipts (Token Economy Phase 1, Part 1) ───
	// verified is a bounded {true,false} set. Never node_id/request_id labels.
	POVIReceiptsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_povi_receipts_total", Help: "PoVI receipts processed, by signature-verified."},
		[]string{"verified"},
	)
	POVIReceiptVerifyFailuresTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_povi_receipt_verify_failures_total", Help: "PoVI receipts that failed signature verification (forged/tampered)."},
	)
	// Only ever increments if the provisional/unsafe minting flag is on — so a
	// flipped LENS_POVI_MINTING_ENABLED is visible in metrics.
	POVIProvisionalMintsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_povi_provisional_mints_total", Help: "PROVISIONAL receipt-based LENS mints (UNSAFE — only if LENS_POVI_MINTING_ENABLED is on; default off)."},
	)

	// ─── PoVI staking (Token Economy Phase 1, Part 2) ───
	// Unlabeled gauges/counters — trivially cardinality-safe (no node/workspace
	// labels; these are protocol-wide totals).
	POVIStakeLockedTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_povi_stake_locked_total", Help: "Total LENS currently locked as node-registration collateral."},
	)
	POVINodesStakedGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_povi_nodes_staked", Help: "Number of minting-eligible staked nodes (active stake ≥ min)."},
	)
	POVISlashesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_povi_slashes_total", Help: "Stake slash events."},
	)
	POVISlashAmountTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_povi_slash_amount_total", Help: "Total LENS burned via stake slashing."},
	)

	// ─── PoVI challenge-and-slash (Token Economy Phase 1, Part 3) ───
	// result is the bounded set {pass, fail, timeout}.
	POVIChallengesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_povi_challenges_total", Help: "PoVI challenges issued, by result."},
		[]string{"result"},
	)
	POVIChallengeSlashesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_povi_challenge_slashes_total", Help: "Slashes triggered by a failed/timed-out challenge."},
	)
	POVIChallengeTimeoutsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_povi_challenge_timeouts_total", Help: "Challenges where the node did not answer (treated as a failure)."},
	)
)

func init() {
	prometheus.MustRegister(
		RequestsTotal, CacheHitsTotal, TokensSavedTotal,
		HTTPRequestsTotal, HTTPRequestDuration, InflightRequests,
		UpstreamRequestsTotal, UpstreamDuration,
		CacheMissesTotal,
		CircuitBreakerState,
		RateLimitRejectionsTotal, RateLimitRedisErrorsTotal,
		TokenLedgerWriteDuration,
		TokensMintedTotal, LXCConvertedTotal,
		HAInstanceCount,
		ReplicaLagSeconds,
		BudgetSpentUSD, BudgetUtilizationRatio,
		BudgetThresholdCrossedTotal, BudgetBlocksTotal, AgentSettleFailuresTotal, AgentHoldCutsTotal,
		ForecastProjectedUSD, ForecastWillExceedTotal,
		AnomaliesDetectedTotal, AnomalyMaxFactor, RoyaltyDetectorFlagged, DetectorLastRunAgeSeconds, AnnotationReputationEvents,
		ROIReportsGeneratedTotal, ROIReportDuration,
		RoutingRecommendationsTotal, RoutingIntelligenceAppliedTotal, RoutingFallbackTotal, RoutingTierGatedTotal,
		UnpricedModelRequests, ModelWatchUnpricedModels, ModelWatchSinkConfigured,
		RoutingBrainAppliedTotal, RoutingBrainAdvisoryTotal,
		RequestsByModalityTotal, VisionRouteRedirectsTotal, ModalityUnsupportedTotal,
		SpendRecordsTotal,
		DistillCacheTotal, DistillTokensSavedTotal,
		DistillVisionFallbackTotal, DistillVisionTokensCostTotal,
		GuardrailTriggeredTotal, GuardrailBlocksTotal, GuardrailRedactionsTotal,
		EvalRunsTotal, EvalRegressionsDetectedTotal, ABSignificantResultsTotal,
		POVIReceiptsTotal, POVIReceiptVerifyFailuresTotal, POVIProvisionalMintsTotal,
		POVIStakeLockedTotal, POVINodesStakedGauge, POVISlashesTotal, POVISlashAmountTotal,
		POVIChallengesTotal, POVIChallengeSlashesTotal, POVIChallengeTimeoutsTotal,
	)
}

func Handler() http.Handler { return promhttp.Handler() }

// ─── thin helpers (keep call sites one-liners) ───

// RecordUpstream records an upstream provider call's outcome + latency.
func RecordUpstream(provider, status string, dur time.Duration) {
	UpstreamRequestsTotal.WithLabelValues(provider, status).Inc()
	UpstreamDuration.WithLabelValues(provider).Observe(dur.Seconds())
}

// RecordCacheHit / RecordCacheMiss track cache effectiveness by layer.
func RecordCacheHit(layer string)  { CacheHitsTotal.WithLabelValues(layer).Inc() }
func RecordCacheMiss(layer string) { CacheMissesTotal.WithLabelValues(layer).Inc() }

// SetBreakerState records a breaker transition (0 closed, 1 open, 2 half-open).
func SetBreakerState(provider string, state float64) {
	CircuitBreakerState.WithLabelValues(provider).Set(state)
}

// RateLimitRejected increments the per-workspace rejection counter.
func RateLimitRejected(workspace string) {
	if workspace == "" {
		workspace = "unknown"
	}
	RateLimitRejectionsTotal.WithLabelValues(workspace).Inc()
}

// RateLimitRedisError counts one rate-limiter Redis call that failed.
func RateLimitRedisError() { RateLimitRedisErrorsTotal.Inc() }

// ObserveLedgerWrite records a token-ledger write duration.
func ObserveLedgerWrite(dur time.Duration) { TokenLedgerWriteDuration.Observe(dur.Seconds()) }

// MintedTokens / ConvertedLXC bump the economy counters.
func MintedTokens(n float64) {
	if n > 0 {
		TokensMintedTotal.Add(n)
	}
}
func ConvertedLXC(n float64) {
	if n > 0 {
		LXCConvertedTotal.Add(n)
	}
}

// SetHAInstanceCount publishes the current active-instance count.
func SetHAInstanceCount(n int) { HAInstanceCount.Set(float64(n)) }

// SetReplicaLagSeconds publishes the current read-replica replay lag (U8/U9).
func SetReplicaLagSeconds(v float64) { ReplicaLagSeconds.Set(v) }

// ─── budget governance helpers ───
// SetBudgetSpent / SetBudgetUtilization carry the unbounded scope_id label;
// callers MUST apply the budgets cardinality guard before invoking them.

func SetBudgetSpent(scope, scopeID string, v float64) {
	BudgetSpentUSD.WithLabelValues(scope, scopeID).Set(v)
}

func SetBudgetUtilization(scope, scopeID string, v float64) {
	BudgetUtilizationRatio.WithLabelValues(scope, scopeID).Set(v)
}

// BudgetThresholdCrossed / BudgetBlocked use only the bounded {scope} label.
func BudgetThresholdCrossed(scope string) { BudgetThresholdCrossedTotal.WithLabelValues(scope).Inc() }
func BudgetBlocked(scope string)          { BudgetBlocksTotal.WithLabelValues(scope).Inc() }

// AgentSettleFailed counts a served agent answer whose settle failed on path (reservation, pooled or debit).
func AgentSettleFailed(path string) { AgentSettleFailuresTotal.WithLabelValues(path).Inc() }

// AgentHoldCut counts an agent's answer whose settle was cut to its hold (B35.3). The caller names a model the
// catalog knows, or "other": a requested model is the client's string, and a label must be a bounded set.
func AgentHoldCut(model string) { AgentHoldCutsTotal.WithLabelValues(model).Inc() }

// ─── cost forecasting helpers ───
// Bounded {scope} label only — no scope_id, so no cardinality guard needed.

func SetForecastProjected(scope string, v float64) {
	ForecastProjectedUSD.WithLabelValues(scope).Set(v)
}
func ForecastWillExceed(scope string) { ForecastWillExceedTotal.WithLabelValues(scope).Inc() }

// ─── cost anomaly helpers ───
// Bounded {scope, severity} labels only — no unit_id, so no guard needed.

func AnomalyDetected(scope, severity string) {
	AnomaliesDetectedTotal.WithLabelValues(scope, severity).Inc()
}
func SetAnomalyMaxFactor(scope string, v float64) {
	AnomalyMaxFactor.WithLabelValues(scope).Set(v)
}

// SetRoyaltyDetectorFlagged sets the flagged-finding count for one (economy, detector)
// after a sweep — including 0, so the gauge reflects the current picture.
func SetRoyaltyDetectorFlagged(economy, detector string, n float64) {
	RoyaltyDetectorFlagged.WithLabelValues(economy, detector).Set(n)
}

// SetDetectorLastRunAgeSeconds publishes the staleness of a named detector/clearer.
func SetDetectorLastRunAgeSeconds(detector string, ageSeconds float64) {
	DetectorLastRunAgeSeconds.WithLabelValues(detector).Set(ageSeconds)
}

// IncAnnotationReputationEvent counts one appended reputation event of the given kind.
func IncAnnotationReputationEvent(kind string) {
	AnnotationReputationEvents.WithLabelValues(kind).Inc()
}

// ─── ROI reporting helpers ───
// Bounded {period} label only.

func ROIReportGenerated(period string) { ROIReportsGeneratedTotal.WithLabelValues(period).Inc() }
func ObserveROIReportDuration(d time.Duration) {
	ROIReportDuration.Observe(d.Seconds())
}

// ─── routing intelligence helpers ───
// Bounded {basis} label on recommendations; applied/fallback are unlabeled.

func RoutingRecommendation(basis string) { RoutingRecommendationsTotal.WithLabelValues(basis).Inc() }
func RoutingIntelligenceApplied()        { RoutingIntelligenceAppliedTotal.Inc() }
func RoutingBrainApplied()               { RoutingBrainAppliedTotal.Inc() }
func RoutingBrainAdvisory()              { RoutingBrainAdvisoryTotal.Inc() }
func RoutingFallback()                   { RoutingFallbackTotal.Inc() }
func RoutingTierGated(reason string)     { RoutingTierGatedTotal.WithLabelValues(reason).Inc() }

// ─── vision / multimodal helpers ───
// Bounded {modality} label on the counter; redirects/unsupported unlabeled.

func RequestByModality(modality string) { RequestsByModalityTotal.WithLabelValues(modality).Inc() }
func VisionRouteRedirect()              { VisionRouteRedirectsTotal.Inc() }
func ModalityUnsupported()              { ModalityUnsupportedTotal.Inc() }

// SpendRecord counts a spend write by its token-count source. Pass
// "provider_usage" when billed on the provider's reported counts, or
// "estimated" on the len/4 fallback.
func SpendRecord(source string) { SpendRecordsTotal.WithLabelValues(source).Inc() }

// ─── DISTILL helpers ───

// DistillCache counts a conversion-cache lookup by result ("hit"|"miss").
// DistillCache records one distill cache lookup. kind is "conversion" (the
// text-extraction cache) or "ocr" (the vision-OCR result cache) — distinct
// keyspaces, distinctly observable.
func DistillCache(result, kind string) { DistillCacheTotal.WithLabelValues(result, kind).Inc() }

// DistillTokensSaved adds to the running total of input tokens saved by
// distillation. Negative inputs (a conversion that grew the token count) are
// ignored — a counter only goes up; the per-call signed value lives in the
// Savings struct.
func DistillTokensSaved(n int) {
	if n > 0 {
		DistillTokensSavedTotal.Add(float64(n))
	}
}

// DistillVisionFallback counts a vision-OCR fallback attempt by result, folding
// unexpected values to "unknown" so the label stays bounded.
func DistillVisionFallback(result string) {
	switch result {
	case "ok", "empty", "error":
	default:
		result = "unknown"
	}
	DistillVisionFallbackTotal.WithLabelValues(result).Inc()
}

// DistillVisionTokensCost adds to the running total of tokens SPENT by the
// vision-OCR fallback (the expensive path). This is a cost, deliberately kept
// separate from DistillTokensSaved so OCR spend can never look like a saving.
func DistillVisionTokensCost(n int) {
	if n > 0 {
		DistillVisionTokensCostTotal.Add(float64(n))
	}
}

// ─── guardrails helpers ───
// Bounded {type, action} labels only — never workspace or content.

func GuardrailTriggered(gtype, action string) {
	GuardrailTriggeredTotal.WithLabelValues(gtype, action).Inc()
}
func GuardrailBlock(gtype string)     { GuardrailBlocksTotal.WithLabelValues(gtype).Inc() }
func GuardrailRedaction(gtype string) { GuardrailRedactionsTotal.WithLabelValues(gtype).Inc() }

// ─── evaluation pipeline helpers (Upgrade 17) ───

// EvalRunRecorded counts a completed eval run. result is folded to the bounded
// set {pass, fail, unknown} so the label can never explode cardinality.
func EvalRunRecorded(result string) {
	switch result {
	case "pass", "fail":
	default:
		result = "unknown"
	}
	EvalRunsTotal.WithLabelValues(result).Inc()
}

// EvalRegressionsDetected adds n detected regressions (no-op for n<=0).
func EvalRegressionsDetected(n int) {
	if n > 0 {
		EvalRegressionsDetectedTotal.Add(float64(n))
	}
}

// ABSignificantResult counts one A/B experiment that reached significance.
func ABSignificantResult() { ABSignificantResultsTotal.Inc() }

// ─── PoVI receipt helpers (Token Economy Phase 1, Part 1) ───

// POVIReceipt counts a processed receipt by signature-verified, and bumps the
// dedicated failures counter when verification failed.
func POVIReceipt(verified bool) {
	if verified {
		POVIReceiptsTotal.WithLabelValues("true").Inc()
		return
	}
	POVIReceiptsTotal.WithLabelValues("false").Inc()
	POVIReceiptVerifyFailuresTotal.Inc()
}

// POVIProvisionalMint counts a PROVISIONAL receipt-based mint. Only invoked
// when the unsafe minting flag is on, so flipping it is observable.
func POVIProvisionalMint() { POVIProvisionalMintsTotal.Inc() }

// ─── PoVI staking helpers (Part 2) ───

// SetPOVIStakeLocked publishes the total LENS locked as node collateral.
func SetPOVIStakeLocked(v float64) { POVIStakeLockedTotal.Set(v) }

// SetPOVINodesStaked publishes the count of minting-eligible staked nodes.
func SetPOVINodesStaked(n float64) { POVINodesStakedGauge.Set(n) }

// POVISlash records one slash event of `amount` LENS burned.
func POVISlash(amount float64) {
	POVISlashesTotal.Inc()
	if amount > 0 {
		POVISlashAmountTotal.Add(amount)
	}
}

// ─── PoVI challenge helpers (Part 3) ───

// POVIChallenge counts a challenge by result, folding unexpected values to
// "unknown" so the label stays bounded.
func POVIChallenge(result string) {
	switch result {
	case "pass", "fail", "timeout":
	default:
		result = "unknown"
	}
	POVIChallengesTotal.WithLabelValues(result).Inc()
}

// POVIChallengeSlash records a slash triggered by a failed challenge.
func POVIChallengeSlash(amount float64) {
	POVIChallengeSlashesTotal.Inc()
	if amount > 0 {
		POVISlashAmountTotal.Add(amount)
	}
}

// POVIChallengeTimeout records a challenge the node did not answer.
func POVIChallengeTimeout() { POVIChallengeTimeoutsTotal.Inc() }
