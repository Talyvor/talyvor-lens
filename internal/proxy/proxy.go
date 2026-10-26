package proxy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/budget"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/inference"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/reqtrack"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/routingbrain"
	"github.com/talyvor/lens/internal/safehttp"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/tare"
	"github.com/talyvor/lens/internal/templates"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/internal/worktier"
)

const (
	maxBodyBytes                = 4 << 20 // 4 MiB
	openAIChatURL               = "https://api.openai.com/v1/chat/completions"
	anthropicMessageURL         = "https://api.anthropic.com/v1/messages"
	googleGenerativeLanguageURL = "https://generativelanguage.googleapis.com"
	upstreamTimeout             = 120 * time.Second
	defaultWorkspaceID          = "default"
)

// CacheBypassHeader, set to "bypass", makes a request skip every cache read (B15.2). The response
// echoes it as "bypassed".
const CacheBypassHeader = "X-Talyvor-Cache"

// CacheStoreHeader, set to "off", keeps a request's answer out of every cache layer, own or pooled, buffered or
// streamed (B17.151). A temporary chat sends it with X-Talyvor-Cache: bypass, so it neither reads nor leaves an answer.
const CacheStoreHeader = "X-Talyvor-Cache-Store"

// PoolHeader, set to "off", keeps a request's answer out of the shared pool (B17.152): no pooled entry and no
// shadow-pool observation, buffered or streamed, so it earns no royalty. Its own workspace still caches it, and the
// request may itself be served from the pool. Chat sends it with every question in a chat kept out of the pool.
const PoolHeader = "X-Talyvor-Pool"

type cacheStoreOffKey struct{}
type poolOffKey struct{}

// withCacheStoreHeader marks ctx when r asks that its answer be kept nowhere (CacheStoreHeader) or out of the
// shared pool (PoolHeader); storeAnswer, sharesAnswers and shadowPoolObservation read the marks.
func withCacheStoreHeader(ctx context.Context, r *http.Request) context.Context {
	if strings.EqualFold(r.Header.Get(CacheStoreHeader), "off") {
		ctx = context.WithValue(ctx, cacheStoreOffKey{}, true)
	}
	if strings.EqualFold(r.Header.Get(PoolHeader), "off") {
		ctx = context.WithValue(ctx, poolOffKey{}, true)
	}
	return ctx
}

func cacheStoreOff(ctx context.Context) bool {
	off, _ := ctx.Value(cacheStoreOffKey{}).(bool)
	return off
}

func poolOff(ctx context.Context) bool {
	off, _ := ctx.Value(poolOffKey{}).(bool)
	return off
}

// alertSink is the subset of *alerts.AlertManager that proxy.serve()
// touches. Defined locally so tests can drop in a counter mock without
// pulling in the full pgxpool / NATS stack the real manager needs.
type alertSink interface {
	IsCircuitOpen(workspaceID, team, feature string) bool
	GetDowngradeModel(provider, model string) string
	RecordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool) error
	// RecordSpendWithDistill is RecordSpend plus the token_events.distill_method
	// attribution ("convert" / "vision_ocr"). Used only by the DISTILL request
	// path; non-distilled traffic keeps using RecordSpend.
	RecordSpendWithDistill(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string) error
	// RecordSpendWithTare is RecordSpendWithDistill plus the Tare metering record (token_events
	// tare_* columns, B6.4). Used only when Tare reduced the request.
	RecordSpendWithTare(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string, tare alerts.TareMeter) error
	// RecordCacheServe writes the token_events row for a CACHE-SERVED response
	// (serve_source = the cache-hit layer, cost_usd = 0 — Talyvor's provider
	// cost; the requester's pre-serve LXC debit is a different ledger). Used
	// only by the cache serve points; upstream traffic keeps using RecordSpend.
	RecordCacheServe(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, sessionID, requestID, modality, serveSource string) error
	// RecordNodeServe writes the token_events row for a request served by a
	// REGISTERED INFERENCE NODE (serve_source='node', cost_usd = what the question
	// was charged — B27.2; any LENS owed the node is lens_token_ledger's number). It
	// makes a node serve countable as a MISS in the cache hit rate and in budgets. Used
	// only by the node-serve point (tryNodeRouting); upstream/cache traffic keeps its own recorders.
	RecordNodeServe(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, sessionID, requestID, modality string, costUSD float64) error
}

// learnerRecorder is the minimal surface the proxy uses to feed the routing
// learner (*learner.Learner satisfies it). Kept as an interface so a test can
// inject a double and ASSERT ON WHAT THE LEARNER RECEIVED: the learner persists
// the raw prompt+response to a 30-day NATS stream, so the logging-policy gate
// must be provable by observing that a none/metadata workspace's content never
// reaches it — not merely by a status code.
type learnerRecorder interface {
	Record(ctx context.Context, event learner.TokenEvent) error
}

// budgetGate is the subset of *budgets.Service the proxy hot path touches.
// Defined locally so the proxy can be exercised without a DB-backed
// service. CheckBudget is the spend gate (most-restrictive across the
// workspace/team/sprint budgets); RecordSpend feeds the in-memory running
// totals after a request bills.
type budgetGate interface {
	CheckBudget(ctx context.Context, workspace, team, sprint string, estCost float64) budgets.Decision
	RecordSpend(ctx context.Context, workspace, team, sprint string, cost float64)
}

type Proxy struct {
	exact            *cache.ExactCache
	semantic         *cache.SemanticCache
	embedder         cache.Embedder
	compressor       *compressor.Compressor
	router           *router.Router
	piiDetector      *pii.Detector
	alertManager     alertSink
	templateDetector *templates.TemplateDetector
	scorer           *quality.Scorer
	tracker          *attribution.Tracker
	attrStore        *attribution.Store
	budgetService    budgetGate
	routingAdvisor   *routing.Advisor
	qualityGate      *routing.QualityGate // B27.23: a cost downgrade stands only on measured quality
	workspaceManager *workspace.Manager
	// tareModel is Tare phase 2a (internal/tare/kompress), run where phase 1 refuses for a workspace
	// that opted in (GetTareModel). nil = this lens has no model; SetTareModel installs it.
	tareModel tare.Reduction
	// tareTraces keeps prose for Tare phase 2b's training set (B27.36). nil = this lens collects nothing.
	tareTraces  tareTraceSink
	localRouter *localrouter.LocalRouter

	// Node auto-route (blocker 6) — optional, flag-gated, nil-safe. When nodeAutoRouteEnabled AND
	// nodeRouter has a healthy endpoint for the model, the gateway forwards to a registered node's
	// /inference with a short-lived token signed by lensChallengePriv (the EXISTING challenge key);
	// the node auto-signs + submits its own receipt. nil/false → fully inert (byte-identical serve).
	nodeAutoRouteEnabled bool
	nodeRouter           *localrouter.Router
	lensChallengePriv    ed25519.PrivateKey
	nodeHTTPClient       *http.Client
	// servedMeasure records Lens's OWN measurement of each node-served request
	// (request_id → node_id + measured output tokens), the basis the PoVI receipt
	// mint prices on (migration 0099). nil ⇒ no measurement recorded, so any
	// enabled receipt mint fails closed. *povi.MeasurementStore satisfies it.
	servedMeasure servedMeasurementRecorder

	injectionDetector *injection.Detector
	budgetEnforcer    *budget.Enforcer
	sessionTracker    *session.SessionTracker
	promptManager     *prompts.Manager
	fallbackRouter    *fallback.FallbackRouter
	keyPool           *keypool.Pool
	auditExporter     *audit.Exporter
	guardrails        *guardrails.Engine
	distiller         *distillIntegration
	poolGate          *cache_pooling.PoolabilityGate
	royaltyMinter     royaltySink
	poolDiscount      float64 // r — consumer discount on CROSS-TENANT pooled hits; 0 = charge list

	// synthetic reports a B17.1 synthetic workspace (workspace.Manager.GetSynthetic); nil = none are.
	synthetic func(wsID string) bool

	// codeRunner runs the code a model asks to run when the request has Run code on (B28.119); nil = never.
	codeRunner codeRunner

	// limits enforces the spending cap and rate limits a workspace set through PUT .../config
	// (B18.3). nil = none enforced.
	limits *workspaceLimits

	// Shadow LXC spend (Stage 2.4/2.5) — optional, nil-safe. lxcShadowEnabled
	// is read per-call so the flag stays live. See shadow_lxc.go.
	lxcSink          lxcSpendSink
	lxcShadowEnabled func() bool

	// LXC gating (Stage 2.4/2.5) — optional, nil-safe pre-serve block.
	// Inert unless lxcGatingEnabled() AND lxcShadowEnabled() (see lxc_gate.go).
	lxcGate          lxcBalanceReader
	lxcGatingEnabled func() bool
	// B1.6 subscription allowance — drawn before prepaid LXC; nil = no allowance wired.
	allowance subscriptionAllowance
	// B27.26 BYOK — a workspace's own provider keys while it is on the BYOK plan; nil = BYOK not wired.
	ownKeySrc ownKeySource

	// F4-capstone step C.1 — the agent allocator (see agent_allocator.go). agentSpender debits the pre-serve
	// LXC estimate against the per-scoped-key sub-budget; agentAllocEnabled gates it; agentDebitSalt
	// (process-start crypto/rand) seeds the server-derived debit key. All nil/empty ⇒ inert (non-agent
	// behavior unchanged).
	agentSpender      agentSpender
	agentAllocEnabled func() bool
	agentDebitSalt    []byte

	// Reservation path (billing redesign) — when reservationEnabled, the pre-serve seam HOLDS and the
	// post-serve seam SETTLES the delivered cost. reservationMaxOut bounds the conservative hold's output
	// allowance when a request omits max_tokens. Both nil-safe (reservationActive() checks).
	reservationEnabled func() bool
	reservationMaxOut  func() int
	// B9.8 — the per-session running total and its ceiling for browser-chat requests (chat_billing.go).
	sessionSpend      sessionSpend
	sessionSpendBound int64

	// Routing-pattern capture (Phase-3) — optional, nil-safe post-serve
	// producer for the routing Advisor. See pattern_capture.go.
	patternSink           patternCaptureSink
	patternCaptureEnabled func() bool

	// Cross-tenant pooling SHADOW LOG (W4.9) — optional, nil-safe post-serve
	// observation of what would have pooled. Shares the obsLimiter budget.
	// Persist-only sink; no cache client, no ledger. See shadow_pool.go.
	poolShadowSink    poolShadowSink
	poolShadowEnabled func() bool

	// WorkTier descriptive classifier (optional, nil-safe post-serve). Shares the
	// obsLimiter budget with pattern capture. See worktier_capture.go.
	workTierSink    workTierSink
	workTierEnabled func() bool

	// routeDecision descriptive capture (optional, nil-safe post-serve). MINT-FREE go/no-go substrate: records
	// the routing Advisor's baseline vs served model + estimated cost. Shares the obsLimiter. See
	// routedecision_capture.go.
	routeDecisionSink    routeDecisionSink
	routeDecisionEnabled func() bool

	// routingBrain (H8.1) is the OFFLINE-computed, MINT-FREE routing recommender read
	// on the serve path as a cheap in-memory lookup. Nil / disabled → routing is
	// byte-for-byte unchanged. Advisory surfaces; autonomous applies within the hard
	// floor. See routing_brain_apply.go.
	routingBrain *routingbrain.Brain

	// Routing-prediction LIVE emit (Proof-of-Improvement piece 3) — optional, nil-safe post-serve. Records a
	// contributor's routing PREDICTION when the live cohort override fires (the FARM GATE). Mint-free itself;
	// the offline scorer+minter turn a proven prediction into a held mint. Default-off. See
	// routing_predict_emit.go.
	routingPredictEmitter     routingPredictEmitter
	routingPredictEmitEnabled func() bool

	// nodeLatency descriptive capture (optional, nil-safe post-serve on the node-route path). Shares the
	// obsLimiter budget; default-off; mint-free (the sink has no ledger handle). See nodelatency_capture.go.
	nodeLatencySink    nodeLatencySink
	nodeLatencyEnabled func() bool

	// compressionSink is the durable record of what the prompt rewriter did to
	// the bytes a provider received. No enable flag rides beside it on purpose —
	// see SetCompressionSink for why the measurement is bound to the 0117 gate
	// rather than to a switch of its own.
	compressionSink compressionSink

	// K4 intrinsic output verifier (optional, nil-safe post-serve). Shares the obsLimiter budget;
	// default-off; mint-free (import-guarded). See output_verdict_capture.go.
	outputVerdictSink    outputVerdictSink
	outputVerdictEnabled func() bool

	// obsLimiter bounds post-serve observational writes (pattern capture).
	// nil = no bound. main wires the same limiter into attribution's
	// RecordAsync so the total observational claim on the DB pool stays
	// bounded (#122). See pattern_capture.go / internal/backpressure.
	obsLimiter *backpressure.Limiter

	// Distill attribution (S1) — optional, nil-safe post-serve, MINT-FREE
	// record of a consented cross-tenant pooled-distill serve. See
	// distill_attribution.go. nil ⇒ attribution off (inert).
	distillAttribSink distillAttributionSink

	// Routing-pattern EARNING (S4) — optional, nil-safe. Separate sink from
	// capture (this one can mint via RecordPattern). See pattern_earn.go.
	patternEarnSink    patternEarnSink
	patternEarnEnabled func() bool
	retryConfig        retry.Config
	httpClient         *http.Client
	openAIKey          string
	anthropicKey       string
	googleKey          string
	learner            learnerRecorder

	// Upstream URLs are unexported and defaulted so tests can swap them
	// for an httptest server without leaking config to callers.
	openAIURL    string
	anthropicURL string
	googleURL    string

	// B40.10 — speech (speech.go). An empty key leaves /v1/audio/* answering "not configured".
	deepgramKey string
	deepgramURL string

	// Bedrock-specific state. bedrockURL is empty in production (URL is
	// computed from region); tests set it to an httptest base URL. The
	// config is set via SetBedrockConfig after construction so New's
	// already-long parameter list doesn't grow further.
	bedrockConfig BedrockConfig
	bedrockURL    string

	// Extra OpenAI-compatible providers (Mistral, Groq, vLLM). Keys
	// land here via SetExtraProviderConfig; URLs default to the real
	// endpoints, tests override. vLLM has no public default — the
	// operator runs the inference server somewhere.
	mistralKey string
	groqKey    string
	vllmKey    string
	mistralURL string
	groqURL    string
	vllmURL    string
}

// New constructs a Proxy. The learner is variadic so callers that don't
// need usage analytics still compile; production wires a *learner.Learner
// as the last argument.
func New(
	exactCache *cache.ExactCache,
	semanticCache *cache.SemanticCache,
	embedder cache.Embedder,
	compressorImpl *compressor.Compressor,
	routerImpl *router.Router,
	piiDetector *pii.Detector,
	alertManager *alerts.AlertManager,
	templateDetector *templates.TemplateDetector,
	scorer *quality.Scorer,
	tracker *attribution.Tracker,
	workspaceManager *workspace.Manager,
	localRouter *localrouter.LocalRouter,
	injectionDetector *injection.Detector,
	budgetEnforcer *budget.Enforcer,
	_ *batch.BatchRouter, // B18.2: no longer read — the proxy ignores X-Talyvor-Batch
	sessionTracker *session.SessionTracker,
	promptManager *prompts.Manager,
	fallbackRouter *fallback.FallbackRouter,
	keyPool *keypool.Pool,
	auditExporter *audit.Exporter,
	guardrailsEngine *guardrails.Engine,
	openAIKey string,
	anthropicKey string,
	googleKey string,
	learners ...*learner.Learner,
) *Proxy {
	p := &Proxy{
		exact:             exactCache,
		semantic:          semanticCache,
		embedder:          embedder,
		compressor:        compressorImpl,
		router:            routerImpl,
		piiDetector:       piiDetector,
		templateDetector:  templateDetector,
		scorer:            scorer,
		tracker:           tracker,
		workspaceManager:  workspaceManager,
		localRouter:       localRouter,
		injectionDetector: injectionDetector,
		budgetEnforcer:    budgetEnforcer,
		sessionTracker:    sessionTracker,
		promptManager:     promptManager,
		fallbackRouter:    fallbackRouter,
		keyPool:           keyPool,
		auditExporter:     auditExporter,
		guardrails:        guardrailsEngine,
		retryConfig:       retry.DefaultConfig(),
		httpClient:        &http.Client{Timeout: upstreamTimeout, Transport: reqtrack.Transport(nil)}, // B27.11: waits on the provider read "upstream"
		openAIKey:         openAIKey,
		anthropicKey:      anthropicKey,
		googleKey:         googleKey,
		openAIURL:         openAIChatURL,
		anthropicURL:      anthropicMessageURL,
		googleURL:         googleGenerativeLanguageURL,
		mistralURL:        "https://api.mistral.ai",
		groqURL:           "https://api.groq.com",
		// vllmURL stays empty by default — operator-supplied.
	}
	// Guard the typed-nil interface trap (as with alertManager below): assign
	// only a non-nil concrete pointer so the `p.learner == nil` fast-path in
	// recordTokenEvent keeps working when no learner is wired.
	if len(learners) > 0 && learners[0] != nil {
		p.learner = learners[0]
	}
	// Guard the typed-nil interface trap: assign the concrete pointer
	// only when it isn't nil so `p.alertManager != nil` keeps working.
	if alertManager != nil {
		p.alertManager = alertManager
	}
	return p
}

// SetAlertSink lets tests inject a counter mock implementing alertSink.
// Production never calls this — main.go wires a real *alerts.AlertManager
// through New(). Kept unexported in spirit (tests are in-package).
// SetAttributionStore wires the upgraded per-request
// attribution store (Upgrade Batch 1 / Item 3). Keeping this
// as a setter rather than a constructor arg avoids re-shuffling
// the already-long proxy.New signature; main.go calls it right
// after constructing the proxy.
func (p *Proxy) SetAttributionStore(s *attribution.Store) {
	p.attrStore = s
}

// SetRoutingAdvisor wires the pattern-network routing advisor (Upgrade 22).
// A setter so proxy.New's signature stays put. A nil advisor (or one whose
// Enabled() is false) leaves model selection byte-for-byte unchanged.
func (p *Proxy) SetRoutingAdvisor(a *routing.Advisor) {
	if a != nil {
		p.routingAdvisor = a
	}
}

// SetBudgetService wires the per-team / per-sprint budget governor
// (Upgrade 19). A setter, like SetAttributionStore, so proxy.New's
// signature stays put. A nil service disables the budget gate entirely —
// the workspace spend cap continues to enforce on its own.
func (p *Proxy) SetBudgetService(s *budgets.Service) {
	// Guard the typed-nil interface trap so `p.budgetService != nil` holds.
	if s != nil {
		p.budgetService = s
	}
}

func (p *Proxy) setAlertSink(sink alertSink) {
	p.alertManager = sink
}

// SetSyntheticLookup wires B17.1: a synthetic workspace's cache entries live in their own partition,
// and it earns no royalty.
func (p *Proxy) SetSyntheticLookup(isSynthetic func(wsID string) bool) { p.synthetic = isSynthetic }

func (p *Proxy) isSynthetic(wsID string) bool { return p.synthetic != nil && p.synthetic(wsID) }

// SetPoolGate enables the Phase-2 Stage 2.0 shared-cache governance gate. Wired
// as a setter so proxy.New's signature stays put. When unset (nil), the pooled
// path is fully inert: storeCaches still owner-tags private entries but writes no
// pooled copy, and the request path never attempts a cross-tenant read.
func (p *Proxy) SetPoolGate(gate *cache_pooling.PoolabilityGate) {
	p.poolGate = gate
}

// SetNodeRouter wires gateway auto-routing to registered inference nodes (blocker 6). A setter so
// proxy.New's signature stays put. enabled=false (the default) leaves the serve path byte-identical:
// tryNodeRouting is never entered. lensPriv is the gateway's EXISTING challenge key — it signs the
// short-lived, request-bound token the node verifies with its pinned challenge pubkey. No receipt or
// mint code is involved: the node produces + submits its own receipt as today.
func (p *Proxy) SetNodeRouter(r *localrouter.Router, lensPriv ed25519.PrivateKey, client *http.Client, enabled bool) {
	p.nodeRouter = r
	p.lensChallengePriv = lensPriv
	p.nodeHTTPClient = client
	p.nodeAutoRouteEnabled = enabled
}

// servedMeasurementRecorder persists Lens's OWN measurement of a node-served
// request (request_id → node_id + measured output tokens) — the basis the PoVI
// receipt mint prices on and binds to the serving node (migration 0099).
// *povi.MeasurementStore satisfies it.
type servedMeasurementRecorder interface {
	Record(ctx context.Context, requestID, nodeID, workspaceID string, outputTokens int) error
}

// SetServedMeasurementRecorder wires the mint-basis measurement writer. Setter so
// proxy.New's signature stays put; when unset (nil) node-served requests record no
// measurement, so any enabled receipt mint fails closed (mints nothing).
func (p *Proxy) SetServedMeasurementRecorder(r servedMeasurementRecorder) { p.servedMeasure = r }

// royaltySink is the Phase-2 Stage 2.1 Pool-B royalty surface: one call per
// SERVED cross-tenant pooled hit. *poolroyalty.Minter satisfies it; exactly-
// once (the request_id claim) and the inert-by-default flag live in the
// Minter, not here — the proxy only reports what it actually served.
type royaltySink interface {
	MintServedHit(ctx context.Context, h poolroyalty.ServedHit) (poolroyalty.Result, error)
}

// SetRoyaltyMinter enables the Stage 2.1 Pool-B royalty mint. Wired as a
// setter so proxy.New's signature stays put. When unset (nil), pooled hits
// serve exactly as Stage 2.0 left them and nothing mints.
func (p *Proxy) SetRoyaltyMinter(m royaltySink) {
	p.royaltyMinter = m
}

// SetPoolConsumerDiscount sets r, the fraction of a CROSS-TENANT POOLED hit's list price handed back
// to the consumer (config.PoolConsumerDiscount; see there for why 0.30). Same setter idiom as the
// royalty minter so proxy.New's signature stays put.
//
// UNWIRED MEANS 0, i.e. charge list — exactly today's behaviour. That is the safe direction for a
// money path: a forgotten wiring under-discounts, which is visible to a customer and costs them
// nothing they were promised, whereas a default that over-discounted would silently under-fund
// every contributor's royalty (the royalty is a share of the settled charge).
func (p *Proxy) SetPoolConsumerDiscount(r float64) {
	if r < 0 || r >= 1 || math.IsNaN(r) {
		return // config already refuses these at boot; belt-and-braces, never a garbage charge
	}
	p.poolDiscount = r
}

// pooledPrice is the priced outcome of one cross-tenant pooled hit, in µLXC — the ledger's unit, so
// the number quoted to the consumer and the number debited are the same kind of thing.
//
// ⚠ BOTH PRICES CEIL AND `Saved` IS THE DIFFERENCE OF THE ROUNDED INTEGERS, never a separately
// rounded USD figure. Rounding each of the three independently lets them disagree by a µLXC, and a
// bill whose parts do not add up is worse than one with no parts. Charged ≤ List because ceil is
// monotone and r < 1, so Saved is never negative.
type pooledPrice struct {
	ListULXC    int64   // what the live call would have cost (avoided COGS), priced as a charge
	ChargedULXC int64   // what the consumer is billed after the discount
	SavedULXC   int64   // ListULXC − ChargedULXC, exact by construction
	Rate        float64 // the rate actually applied to THIS request
	PriceBasis  string  // "fallback" when the model was absent from the catalog
	modelForRow string  // the model that served, stamped on the money row
	Pooled      bool    // false ⇒ own-cache hit or no pooled hit; nothing is charged or advertised
}

// chargeULXC prices a USD amount as a CHARGE: µLXC, rounded UP. The single conversion used for both
// the list price and the discounted price, so they round the same way.
func chargeULXC(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return economy.ChargeULXC(usd)
}

// pricePooledHit applies the consumer discount to a pooled hit's avoided cost. Pure — no I/O, no
// context — so it can be called BEFORE the response body goes out, which is the only moment the
// saving can still be put in a header.
func pricePooledHit(avoidedUSD, rate float64, priceBasis string) pooledPrice {
	list := chargeULXC(avoidedUSD)
	charged := chargeULXC(avoidedUSD * (1 - rate))
	if charged > list {
		charged = list // unreachable while rate >= 0; a charge above list is never emitted
	}
	return pooledPrice{ListULXC: list, ChargedULXC: charged, SavedULXC: list - charged,
		Rate: rate, PriceBasis: priceBasis, Pooled: true}
}

// setSavingHeaders puts the saving on the response. ⚠ MUST BE CALLED BEFORE THE BODY IS WRITTEN —
// the serve happens before the settle (a billing failure must never affect an already-served
// response), so a header written after the body silently never reaches the client.
//
// This is the half that decides whether the discount works at all: 644 instead of 920, with nothing
// saying so, is indistinguishable from a cheaper answer. The tester who reported this only noticed
// by comparing two numbers by hand.
func (pp pooledPrice) setSavingHeaders(w http.ResponseWriter) {
	if !pp.Pooled || pp.ListULXC <= 0 {
		return
	}
	w.Header().Set("X-Talyvor-Pool-List-ULXC", strconv.FormatInt(pp.ListULXC, 10))
	w.Header().Set("X-Talyvor-Pool-Charged-ULXC", strconv.FormatInt(pp.ChargedULXC, 10))
	w.Header().Set("X-Talyvor-Pool-Saved-ULXC", strconv.FormatInt(pp.SavedULXC, 10))
	w.Header().Set("X-Talyvor-Pool-Discount-Rate", strconv.FormatFloat(pp.Rate, 'f', -1, 64))
}

// clearSavingHeaders removes them again. Needed on exactly one path: an SSE replay that FAILS
// validation falls through to a live upstream call, and that response must not carry a pooled
// saving — it was not a pooled serve, the consumer pays full price for it, and a stale header would
// promise a discount the ledger does not show (and a stale replay mark, a free answer). Safe because replayAsSSE validates every frame
// before writing anything, so nothing has been flushed when it errors.
func clearSavingHeaders(w http.ResponseWriter) {
	for _, h := range []string{"X-Talyvor-Pool-List-ULXC", "X-Talyvor-Pool-Charged-ULXC",
		"X-Talyvor-Pool-Saved-ULXC", "X-Talyvor-Pool-Discount-Rate", "X-Talyvor-Cache-Replay"} {
		w.Header().Del(h)
	}
}

// extractResponseContent pulls the assistant text out of an OpenAI-shape
// response (forwardWithFallback normalizes every provider to this shape), so
// the output guardrails inspect the model's actual output, not the JSON
// envelope. Returns "" when the shape doesn't match.
func extractResponseContent(body []byte) string {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) == nil && len(r.Choices) > 0 {
		return r.Choices[0].Message.Content
	}
	return ""
}

// emptyAnswer reports a provider answer that says nothing — no text and no tool call — in the Anthropic
// shape (content blocks) or the OpenAI one (choices). A body in neither shape is not judged empty.
func emptyAnswer(body []byte) bool {
	var r struct {
		Content *[]struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Choices *[]struct {
			Message struct {
				Content      json.RawMessage `json:"content"`
				ToolCalls    json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage `json:"function_call"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false
	}
	switch {
	case r.Content != nil:
		for _, c := range *r.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					return false
				}
			case "thinking", "redacted_thinking":
			default:
				return false // a tool call, or a block this does not know
			}
		}
		return true
	case r.Choices != nil:
		blank := func(raw json.RawMessage) bool {
			t := strings.TrimSpace(string(raw))
			return t == "" || t == "null" || t == `""` || t == "[]"
		}
		for _, c := range *r.Choices {
			var text string
			if !blank(c.Message.ToolCalls) || !blank(c.Message.FunctionCall) ||
				!blank(c.Message.Content) && (json.Unmarshal(c.Message.Content, &text) != nil || strings.TrimSpace(text) != "") {
				return false
			}
		}
		return true
	}
	return false
}

// replaceResponseContent rewrites the first choice's message content (used to
// re-inject redacted output). Returns the body unchanged when the shape
// doesn't match, so a redaction can never corrupt the response envelope.
func replaceResponseContent(body []byte, newContent string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return body
	}
	c0, ok := choices[0].(map[string]any)
	if !ok {
		return body
	}
	msg, ok := c0["message"].(map[string]any)
	if !ok {
		return body
	}
	msg["content"] = newContent
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// isAutoRoute reports whether a request cedes its model choice to the
// routing advisor — either the "auto" pseudo-model (a convention developers
// recognise from model-router gateways) or an explicit X-Talyvor-Auto-Route
// header. Any concrete model name is treated as pinned and always honored.
func isAutoRoute(r *http.Request, model string) bool {
	if strings.EqualFold(strings.TrimSpace(model), "auto") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("X-Talyvor-Auto-Route"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// routingDelegated reports whether the customer has CEDED model choice — so the
// base router may downgrade the requested model. Two consented paths:
//   - per-request delegation: the "auto" pseudo-model or the X-Talyvor-Auto-Route
//     header (isAutoRoute) — the customer asked Lens to pick;
//   - per-workspace consent: the workspace opted in to cost-optimised routing on
//     concrete models (cost_optimize_routing, default OFF).
//
// An explicitly named model on a non-consenting workspace returns FALSE and is
// HONOURED exactly — never downgraded. This is the founder's rule in one predicate.
func (p *Proxy) routingDelegated(r *http.Request, wsID, model string) bool {
	if isAutoRoute(r, model) {
		return true
	}
	return p.workspaceManager != nil && p.workspaceManager.GetCostOptimizeRouting(wsID)
}

// providerConfig holds the per-provider knobs HandleOpenAI/HandleAnthropic/
// HandleGoogle differ on. Everything else is shared in serve(). The URL
// is a function of the model so Gemini's path-style routing fits cleanly.
// providerConfig is an alias for the type that moved to internal/inference (PR-3c). The alias keeps every
// in-proxy type-name reference (serve/forward/configForProvider/applyKey signatures) compiling unchanged;
// the fields stay unexported in inference, so CONSTRUCTION is exclusively via inference.ConfigFor /
// inference.ConfigForKey and FIELD ACCESS is via the exported Provider methods (ProviderName/UpstreamURL/…).
type providerConfig = inference.ProviderConfig

func (p *Proxy) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, p.configForProvider("openai"))
}

func (p *Proxy) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, p.configForProvider("anthropic"))
}

// HandleGoogle proxies an OpenAI-shaped request through to Gemini's
// generateContent endpoint, translating the body in and the response
// back out so downstream caching / scoring / cost-attribution code
// treats the request indistinguishably from OpenAI or Anthropic.
func (p *Proxy) HandleGoogle(w http.ResponseWriter, r *http.Request) {
	if p.googleKey == "" {
		writeError(w, http.StatusServiceUnavailable, "Google API key not configured")
		return
	}
	p.serve(w, r, p.configForProvider("google"))
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, cfg providerConfig) {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get(WebSearchHeader)), "on") {
		p.serveSearchingWeb(w, r, cfg) // B28.118: the search and the answer each come back here without the header
		return
	}
	if p.codeRunner != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get(RunCodeHeader)), "on") {
		p.serveRunningCode(w, r, cfg) // B28.119: each of its rounds comes back here without the header
		return
	}
	ctx := r.Context()
	// requestStart is captured before any work so the per-request
	// attribution row can report wall-clock latency for the IDE
	// dashboard. The legacy alerts pipeline doesn't use it; this
	// is a parallel signal recorded asynchronously after the
	// response finishes.
	requestStart := time.Now()

	body, err := readLimitedBody(r, maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4MB limit — upload a large document to POST /v1/documents (up to 25 MB) and reference it by id")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	model, prompt, err := extractPrompt(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// B38.8: a model its provider retired fails there — say so here and name the successor, never serve another.
	if m, ok := catalog.Get(model); ok && m.RetiredSuccessor != "" {
		writeError(w, http.StatusGone, fmt.Sprintf("model retired: %s is no longer offered by its provider — use %s", model, m.RetiredSuccessor))
		return
	}
	// B40.10: a speech model has no token rates, so here it would be held and charged nothing — it is served on /v1/audio/*.
	if m, ok := catalog.Get(model); ok && m.Tier == catalog.TierSpeech {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s is a speech model — send it to /v1/audio/transcriptions or /v1/audio/speech", model))
		return
	}

	// Modality detection (Upgrade 15). Cheap + structural — inspects content
	// block types only, never decodes the base64 image/audio bytes. Drives
	// capability-aware routing below and the modality dimension on the spend
	// record. Always recorded so the dashboard sees the request mix.
	modSet := modality.Detect(body)
	metrics.RequestByModality(modSet.Label())
	w.Header().Set("X-Talyvor-Modality", modSet.Label())

	// ⚠ B18.2: X-Talyvor-Batch IS IGNORED — the request is served and billed like any other. It used
	// to take every non-streaming claude-* request to Anthropic's batch API ahead of the workspace,
	// budget and billing work: 202, nothing billed, and no route to fetch the result (the batch
	// routes are unregistered). Talyvor paid the provider and charged no one. Batch is not on the
	// roadmap.

	// Workspace identification + logging policy come BEFORE the workspace
	// policy gate so even a 403 response carries the X-Talyvor-Logging
	// header. wsID resolution (ExtractWorkspaceID) is cheap and pure —
	// header read with a "default" fallback — so doing it ahead of the
	// CheckPolicy call has no downside.
	wsID := defaultWorkspaceID
	if p.workspaceManager != nil {
		wsID = p.workspaceManager.ExtractWorkspaceID(r)
	}

	// Request ID stamped here so every downstream artefact — token_events
	// row, structured log, span attribute — can be correlated back to
	// the same HTTP call. Generated fresh per request; clients can also
	// pass an X-Talyvor-Request-ID header to retain their own.
	requestID := r.Header.Get("X-Talyvor-Request-ID")
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Talyvor-Request-ID", requestID)
	// B32.11: a charge's platform fee row names the request; on r too, for the streamed seam's context.
	ctx = economy.WithChargeRequest(ctx, requestID)
	r = r.WithContext(ctx)

	// Per-workspace logging policy. Decided once per request and applied
	// at each observability write below. Default (metadata) preserves
	// costs/tokens but drops prompt text; `none` skips every DB write;
	// `full` keeps the historic behaviour. Security checks (PII,
	// injection) run regardless of policy. Header is set BEFORE every
	// early-return path (workspace 403, injection 400, budget 400, cache
	// hit, SSE replay) so downstream consumers always see the policy
	// that governed the response.
	loggingPolicy := workspace.LoggingMetadata
	if p.workspaceManager != nil {
		loggingPolicy = p.workspaceManager.GetLoggingPolicy(wsID)
	}
	w.Header().Set("X-Talyvor-Logging", string(loggingPolicy))

	// Workspace policy gate. The workspace decision happens before any
	// cache lookup so a blocked workspace can't even read someone else's
	// cached response — the request returns 403 immediately. Cache
	// isolation downstream is then achieved by prefixing the prompt with
	// the workspace ID before it reaches the cache layer.
	cachePrompt := prompt
	if p.workspaceManager != nil {
		policy := p.workspaceManager.CheckPolicy(ctx, wsID, cfg.ProviderName(), model, len(prompt)/4)
		if !policy.Allowed {
			writeError(w, http.StatusForbidden, policy.Violation)
			metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "workspace_blocked").Inc()
			return
		}
		cachePrompt = wsID + ":" + prompt
	}

	// DISTILL request-path integration (Upgrade 23). When the workspace + this
	// request opt in AND a document block is present, convert it to clean
	// Markdown via the KILLABLE subprocess before the model sees it. This runs
	// after the workspace gate (so a blocked workspace spends no conversion) and
	// before guardrails/cache/capability (so they all operate on the distilled
	// text). INERT-BY-DEFAULT: a non-opted-in request, a request with no
	// document, or a conversion failure leaves body/prompt/modSet byte-for-byte
	// unchanged. After a successful distill we re-derive the variables computed
	// from the body — prompt, modSet (now text-only, so the capability gate
	// won't redirect to a vision model), and the workspace-scoped cachePrompt.
	// distillMethod tags the spend row written far below ("" = not distilled);
	// visionOCR carries any OCR sub-call cost to book as its own spend row.
	// WHICH upstream endpoint this request is for. Classified once, from the inbound path, and
	// read by the cache guard below and by forward(). Anything unrecognised is endpointChat, so
	// every route that worked before is byte-identical.
	endpoint := classifyEndpoint(r)

	distillMethod := ""
	var visionOCR visionSpend
	var distillFacts []distillServeFact // S1: consented cross-tenant serves, recorded post-flush below
	if p.distiller != nil {
		// The live vision-OCR dispatcher for this request (same provider, scoped
		// to the workspace allow-list). On a text-less document the orchestrator
		// uses it to recover text via a vision model and books the cost honestly;
		// a nil-safe failure path leaves a NeedsVision document untouched.
		vd := p.newVisionDispatcher(r, cfg, wsID)
		if nb, np, nm, did, vs, dfacts, saved := p.distiller.MaybeDistillSaving(ctx, r, body, wsID, modSet, vd); did {
			body, prompt, modSet = nb, np, nm
			distillFacts = dfacts
			cachePrompt = prompt
			if p.workspaceManager != nil {
				cachePrompt = wsID + ":" + prompt
			}
			// Durable DISTILL attribution for the spend write below: the main
			// (lower-count) row is tagged 'convert' (the saving is implicit in the
			// reduced count — never a second write), and any OCR sub-call cost is
			// booked as its OWN 'vision_ocr' row, never blended.
			distillMethod = "convert"
			visionOCR = vs
			// modSet is now the post-distill (text-only) set, so the capability
			// gate + spend below bill the converted text. The X-Talyvor-Modality
			// header + RequestByModality metric above intentionally keep the
			// INCOMING modality (what the client sent) — that's the request-mix
			// signal, and the X-Talyvor-Distill header below tells the client we
			// converted it.
			w.Header().Set("X-Talyvor-Distill", "applied")
			// B18.13 — and what it saved. Set before the streaming split, so both seams send them.
			w.Header().Set("X-Talyvor-Distill-Tokens-Saved", strconv.Itoa(saved.tokens))
			w.Header().Set("X-Talyvor-Distill-Bytes-Saved", strconv.Itoa(saved.bytes))
		}
	}

	// TARE (B6.4/B6.5) — the context-reduction layer, OFF unless the workspace policy AND (always, or
	// this request's X-Talyvor-Tare: true) turn it on. Runs after distill and BEFORE the streaming
	// branch below, so the streaming and buffered paths send the same reduced body. Only the newest
	// message can change (tare.PrefixStable); prompt and the workspace-scoped cachePrompt are
	// re-derived from the reduced body exactly as distill does, so a reduced request is cached
	// under its own key and never answers an unreduced one. tareMeter rides the spend write on both
	// paths; its zero value means Tare changed nothing.
	var tareMeter alerts.TareMeter
	tareInput := body // B27.36: Tare's training set keeps the prose as it came, whatever the Tare policy
	if p.shouldTare(r, wsID) {
		if nb, kind, tin, tout, ok := tareReduce(ctx, body, p.tareModelFor(wsID)); ok {
			if _, np, perr := extractPrompt(nb); perr == nil {
				body, prompt = nb, np
				cachePrompt = prompt
				if p.workspaceManager != nil {
					cachePrompt = wsID + ":" + prompt
				}
				tareMeter = alerts.TareMeter{Kind: string(kind), TokensIn: tin, TokensOut: tout,
					WorkItemID: attribution.ExtractFromRequest(r).IssueID}
				w.Header().Set("X-Talyvor-Tare", "applied")
			}
		}
	}

	// Session pickup — header-driven, optional. Empty sessionID means
	// the caller isn't tracking sessions; the entire feature is skipped.
	sessionID := r.Header.Get("X-Talyvor-Session")
	agentName := r.Header.Get("X-Talyvor-Agent")
	if agentName == "" {
		agentName = "default"
	}
	var sess *session.Session
	if sessionID != "" && p.sessionTracker != nil {
		if sess = p.sessionTracker.GetOrCreate(ctx, sessionID, wsID, agentName); sess == nil {
			writeError(w, http.StatusNotFound, "session not found") // another workspace's session (B42.2)
			return
		}
	}

	// Extract any incoming W3C trace context BEFORE we start our own
	// span. otelhttp middleware already does this in production, but
	// extracting again here is idempotent and keeps tests + direct
	// handler invocations correct.
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))

	// streaming flag is needed both for the cache replay branch below
	// and for the OTel span attributes — compute it once here.
	streaming := streamRequested(body)

	// Start the proxy span. Attributes carry the safe metadata —
	// provider/model/workspace/stream — and never the prompt content.
	ctx, span := otel.Tracer("lens/proxy").Start(ctx, "proxy.serve",
		trace.WithAttributes(
			attribute.String("lens.provider", cfg.ProviderName()),
			attribute.String("lens.model", model),
			attribute.String("lens.workspace", wsID),
			attribute.Bool("lens.stream", streaming),
		),
	)
	defer span.End()

	// Set the response header right after the span is live so it persists
	// even if a cache hit short-circuits the request below.
	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		w.Header().Set("X-Talyvor-Trace-ID", sc.TraceID().String())
	}

	// Team / feature attribution for cost accounting and circuit breakers.
	// Both default to "" when callers don't supply the headers; the alert
	// manager treats empty values as a distinct attribution bucket.
	team := r.Header.Get("X-Talyvor-Team")
	feature := r.Header.Get("X-Talyvor-Feature")
	// Sprint follows the issue convention — a client-supplied header, not
	// derived from the API key. Empty when the caller isn't tracking sprints.
	sprint := r.Header.Get("X-Talyvor-Sprint")

	// Budget gate (Upgrade 19). Sits alongside the workspace spend cap but
	// governs workspace / team / sprint budgets with most-restrictive-wins.
	// Reads the in-memory budget snapshot only — no per-request DB hit. A
	// hard_block budget already over its limit rejects with 402; alert and
	// off budgets never reject here (they notify on the recording path). The
	// estimate uses input tokens only (output is unknown pre-call), so the
	// gate under- rather than over-blocks; the true cost is booked later.
	if p.budgetService != nil {
		estCost := budgetEstimateUSD(model, prompt)
		if p.budgetService.CheckBudget(ctx, wsID, team, sprint, estCost) == budgets.DecisionBlock {
			writeError(w, http.StatusPaymentRequired, "budget exceeded for workspace/team/sprint")
			metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "budget_blocked").Inc()
			return
		}
	}

	// B18.3 — the spending cap and rate limits this workspace set through PUT .../config, checked
	// before the provider is called on both seams (the streaming split is below).
	if status, msg := p.limits.admit(ctx, wsID, budgetEstimateUSD(model, prompt),
		len(prompt)/4+boundedMaxOut(extractMaxTokens(body), p.reservationMaxOut)); status != 0 {
		writeError(w, status, msg)
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "workspace_limit_blocked").Inc()
		return
	}

	// B27.26 — BYOK (byok.go): on the BYOK plan with its own key for this provider, the request goes upstream
	// on that key and Talyvor charges it no tokens, so the token gates, holds and charges below pass it by.
	own := p.ownKeys(ctx, wsID, cfg.ProviderName())
	byok := own != nil
	if byok {
		w.Header().Set("X-Talyvor-BYOK", "own-key")
	}

	// LXC gating (Stage 2.4/2.5) — pre-serve block when the workspace can't
	// afford the estimated LXC cost. Sits alongside the budget gate, BEFORE the
	// upstream call. Inert unless LXCGatingEnabled AND shadow are both on; the
	// estimate is input-only (under-blocks); a balance-read error fails open.
	if !byok && p.lxcGateBlocks(ctx, wsID, model, prompt) {
		writeError(w, http.StatusPaymentRequired, "insufficient LXC balance for estimated request cost")
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "lxc_blocked").Inc()
		return
	}
	// B1.6 — a subscriber whose plan allowance is used up continues on prepaid credit, and
	// is refused when there is none. Inert for non-subscribers and agent keys (allowance.go).
	if !byok && p.allowanceGateBlocks(ctx, wsID, model, prompt) {
		writeError(w, http.StatusPaymentRequired, "this period's plan allowance is used up and prepaid credit does not cover the request — top up to continue")
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "allowance_blocked").Inc()
		return
	}
	// B9.8 — a browser-chat request is charged, so it is admitted only when allowance + prepaid covers
	// its conservative cost and its session is under its bound. Inert for every other credential.
	if msg, blocked := p.chatAdmission(ctx, wsID, model, prompt, boundedMaxOut(extractMaxTokens(body), p.reservationMaxOut)); blocked && !byok {
		writeError(w, http.StatusPaymentRequired, msg)
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "chat_blocked").Inc()
		return
	}

	// F4-capstone step C.1 — the agent allocator (see agent_allocator.go). For an API-key-authed request with
	// LXCAgentAllocationEnabled, DEBIT the input-only LXC estimate against the per-scoped-key sub-budget HERE,
	// BEFORE node selection (tryLocalRouting) and BEFORE the upstream call — so a blocked agent does no
	// routing work and physically cannot exceed its ceiling. Non-agent traffic (agentKeyID == "") skips this
	// entirely and is unchanged. agentKeyID is reused below to pick the price-aware routing strategy.
	agentKeyID := agentKeyIDFromContext(ctx)
	// The output the hold allows: an agent's request is held for it, and a pooled answer is priced within it (B35.3).
	holdMaxOut := boundedMaxOut(extractMaxTokens(body), p.reservationMaxOut)
	// B27.26: a BYOK agent request is held or debited nothing, so its agent's rules are judged on their own.
	if agentKeyID != "" && byok {
		if err := p.byokAgentRules(withAgentCall(ctx, agentKeyID, model, cfg.ProviderName(), prompt, ""), agentKeyID); err != nil {
			metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), writeAgentRefusal(w, err)).Inc()
			return
		}
	}
	if agentKeyID != "" && !byok {
		// B19.2 — the agent's spending rules are judged in its hold or debit; they need the model,
		// provider and prompt, and a retry repeating its Idempotency-Key is charged once.
		ctx = withAgentCall(ctx, agentKeyID, model, cfg.ProviderName(), prompt, r.Header.Get("Idempotency-Key"))
		if p.reservationActive() {
			// Billing redesign: HOLD a conservative (output-aware, BOUNDED) reservation pre-serve — the
			// ceiling stays enforced against it — and SETTLE the delivered cost post-serve (or RELEASE a
			// cache hit, free). The hold rides ctx to the post-serve seam.
			rctx, err := p.agentReserve(ctx, agentKeyID, wsID, model, prompt, requestID, holdMaxOut)
			if err != nil {
				metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), writeAgentRefusal(w, err)).Inc()
				return
			}
			ctx = rctx
			// The reservation handle rides `ctx` to the buffered post-serve settle. The STREAMING seam runs
			// on a disconnect-immune context DERIVED FROM r (context.WithoutCancel(r.Context()) — so a
			// mid-flight client disconnect can't abort the bill), NOT this local ctx. Carry the handle onto r
			// too, or the streaming settle can't find it and the hold is stranded (swept + refunded → the
			// streaming request bills nothing). Values only; r's cancellation is unchanged.
			r = r.WithContext(ctx)
		} else {
			actx, err := p.agentAllocate(ctx, agentKeyID, wsID, model, prompt, requestID)
			if err != nil {
				metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), writeAgentRefusal(w, err)).Inc()
				return
			}
			// B23.2: the debit is this request's charge; the mark rides ctx and r (for the streamed seam, as
			// the reservation handle does above) so the post-serve shadow debit does not charge it again.
			ctx = actx
			r = r.WithContext(ctx)
		}
	}

	// Branch / PR / commit attribution. Extracted up front so we can set
	// response headers before WriteHeader; the actual DB write happens
	// after the response body is sent so a slow INSERT can't hold up the
	// client.
	var attr attribution.Attribution
	if p.tracker != nil {
		attr = p.tracker.ExtractAttribution(r)
	}
	willAttribute := p.tracker != nil && (attr.Branch != "" || attr.PRNumber != "")

	// Template detection runs BEFORE PII detection so we count hits against
	// the actual system prompt the caller sent. When the system prompt
	// contains PII we record only the hash + a redacted placeholder so we
	// never persist the raw value to prompt_templates. Once a template
	// crosses the pin threshold we rewrite the body to opt the upstream
	// call into Anthropic's prompt-caching feature; OpenAI caches long
	// system prompts automatically, so its hook is a no-op.
	// B18.4: a logging_policy = none workspace's system prompt is not recorded — prompt_templates
	// keeps its text — so it forgoes the pinned prompt-caching rewrite.
	if p.templateDetector != nil && loggingPolicy != workspace.LoggingNone {
		if sysPrompt, found := p.templateDetector.ExtractSystemPrompt(body); found {
			contentForRecord := sysPrompt
			if p.piiDetector != nil {
				if rr := p.piiDetector.Detect(sysPrompt); rr.WasRedacted {
					contentForRecord = "[REDACTED-PII]"
				}
			}
			tmpl, pinned := p.templateDetector.RecordAndPin(ctx, contentForRecord, cfg.ProviderName())
			if pinned && cfg.ProviderName() == "anthropic" {
				if rewritten, err := p.templateDetector.ApplyAnthropicCaching(body, tmpl); err == nil {
					body = rewritten
				}
			}
		}
	}

	// Named-prompt resolution. Swap any "lens:prompt:<name>" system
	// message for the active prompt body stored under that name in this
	// workspace. Runs after template detection (which already records a
	// hash of the placeholder system prompt) and before PII detection so
	// the PII gate scans the resolved content. Body is only mutated when
	// Resolve actually found a match.
	if p.promptManager != nil {
		if resolved, err := p.promptManager.Resolve(ctx, body, wsID); err == nil && !bytes.Equal(resolved, body) {
			body = resolved
			w.Header().Set("X-Talyvor-Prompt-Resolved", "true")
		}
	}

	// Guardrails pipeline. One call subsumes the previous PII + injection
	// blocks plus topic / word filter / custom-regex rules. Block-action
	// violations short-circuit with a 400; redact-action results rewrite
	// the prompt going forward. piiDetected is preserved as a downstream
	// signal so the cache layer still refuses to persist PII responses.
	piiDetected := false
	guardrailFired := false // WorkTier sensitivity cause B (a non-blocking guardrail tripped)
	var redactedPrompt string
	if p.guardrails != nil {
		gr := p.guardrails.Check(ctx, wsID, prompt, body)
		w.Header().Set("X-Talyvor-Risk-Score", strconv.FormatFloat(gr.RiskScore, 'f', 2, 64))
		// Per-type/action guardrail metrics (Upgrade 13). Bounded labels.
		for _, v := range gr.Violations {
			metrics.GuardrailTriggered(v.Type, string(v.Action))
			if v.Action == guardrails.ActionRedact {
				metrics.GuardrailRedaction(v.Type)
			}
		}
		if !gr.Passed {
			blockType := "guardrail"
			for _, v := range gr.Violations {
				if v.Action == guardrails.ActionBlock {
					blockType = v.Type
				}
			}
			metrics.GuardrailBlock(blockType)
			slog.Warn("proxy: guardrail blocked",
				slog.String("provider", cfg.ProviderName()),
				slog.String("workspace_id", wsID),
				slog.Float64("risk_score", gr.RiskScore),
				slog.Int("violation_count", len(gr.Violations)),
			)
			metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "guardrail_blocked").Inc()
			w.Header().Set("X-Talyvor-Guardrail-Blocked", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":      "guardrail violation",
				"violations": gr.Violations,
				"risk_score": gr.RiskScore,
			})
			return
		}
		for _, v := range gr.Violations {
			if v.Type == "pii" {
				piiDetected = true
				w.Header().Set("X-Talyvor-PII-Detected", "true")
			}
		}
		if gr.RedactedPrompt != "" && gr.RedactedPrompt != prompt {
			redactedPrompt = gr.RedactedPrompt
			w.Header().Set("X-Talyvor-Guardrail-Redacted", "true")
		}
		// WorkTier sensitivity input: a guardrail FIRED (redact/warn) on a served
		// request. A blocked request returned above, so this only marks non-block
		// fires. Carried, NON-CONTENT (a bool, never the matched span).
		guardrailFired = len(gr.Violations) > 0
		if len(gr.Violations) > 0 && piiDetected {
			metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "pii_skip_cache").Inc()
		}
	}
	// B27.36 — never with PII or a tripped guardrail, and never for a workspace that keeps no content.
	if !guardrailFired && loggingPolicy != workspace.LoggingNone {
		p.keepTareTrace(ctx, r, wsID, tareInput)
	}

	// Token-budget enforcement: rewrite the body in place so max_tokens
	// honours the workspace's policy before anything reaches the LLM.
	// Parse errors leave the body untouched — extractPrompt below would
	// have surfaced the same problem anyway.
	if p.budgetEnforcer != nil {
		if newBody, br, err := p.budgetEnforcer.EnforceOnBody(ctx, wsID, body); err == nil && br.Rewritten {
			body = newBody
			w.Header().Set("X-Talyvor-Budget-Applied", "true")
			slog.Info("proxy: budget enforced",
				slog.String("workspace_id", wsID),
				slog.String("reason", br.Reason),
			)
		}
	}

	// B15.1 — everything in this request that shapes the answer besides the prompt text (system,
	// tools, temperature, max_tokens, ...), read from the body exactly as it goes upstream: after
	// distill, Tare and the budget rewrite. Every cache read below and every cache write (buffered,
	// streamed, local and node routing) carries it, so an entry is served only to a request asked
	// under the same settings — in this workspace and, through the pool, in any other.
	reqFP := cache.RequestFingerprint(body)
	// B16.1 — what the semantic layers compare: the latest question, and a hash of the history before
	// it. Read from the same body as reqFP; every semantic read and write below carries it.
	turn := cache.LatestTurn(body)
	// B17.1 — a synthetic workspace's entries live in a partition of their own: both fingerprints move,
	// so no cache layer, own or pooled, buffered or streamed, can match across the line either way.
	if p.isSynthetic(wsID) {
		reqFP = cache.PartitionedFingerprint(reqFP, cache.SyntheticPartition)
		if turn.AloneFP != "" {
			turn.AloneFP = cache.PartitionedFingerprint(turn.AloneFP, cache.SyntheticPartition)
		}
	}
	if rc := runCodeAnswerOf(ctx); rc != nil && !rc.keyed {
		plainFP := cache.RequestFingerprint(withoutRunCodeTool(body))
		if p.isSynthetic(wsID) {
			plainFP = cache.PartitionedFingerprint(plainFP, cache.SyntheticPartition)
		}
		*rc = runCodeAnswer{keyed: true, provider: cfg.ProviderName(), model: model, cachePrompt: cachePrompt, prompt: prompt,
			reqFP: reqFP, plainFP: plainFP, wsID: wsID, requestID: requestID, turn: turn}
	}

	// B15.2: X-Talyvor-Cache: bypass asks for a fresh answer — no cache layer is read, own or
	// pooled, on either path. The answer it gets is stored as usual, so the next asker is served the
	// newest one. The chat's Regenerate sends it.
	bypassCache := strings.EqualFold(r.Header.Get(CacheBypassHeader), "bypass")
	if bypassCache {
		w.Header().Set(CacheBypassHeader, "bypassed")
	}
	ctx = withCacheStoreHeader(ctx, r)

	if !piiDetected && !bypassCache {
		var cached []byte
		var layer string
		// pooledHit is non-nil ONLY when the response came from the shared
		// pool — Stage 2.1 royalty attribution, captured at lookup but
		// consumed exclusively at the SERVE points below. A found-but-not-
		// served hit (e.g. SSE replay failure falling through to the live
		// LLM) therefore reports nothing and mints nothing.
		var pooledHit *poolroyalty.ServedHit
		// B23.1: which stored answer served this request — the rows (hitIDs) and the keys its copies
		// are stored under (hitHashes) — remembered at the serve points so a thumbs-down removes it.
		var hitIDs, hitHashes []string
		// ⚠ NON-CHAT ENDPOINTS NEVER CONSULT THE CACHE. extractPrompt reads `messages`, so an
		// embeddings body (which carries `input`) derives the EMPTY prompt — every embeddings
		// request would share one cache key and the second would be served the first's vector,
		// silently. See endpoint.cacheable().
		span.AddEvent("cache.check.exact")
		if !endpoint.cacheable() {
			span.AddEvent("cache.skip.non_chat")
		} else if c := p.tryExact(ctx, cfg.ProviderName(), model, cache.FingerprintedKey(cachePrompt, reqFP), wsID); c != nil {
			cached, layer = c, "cache_hit_exact"
			hitHashes = answerHashes(cfg.ProviderName(), model, cachePrompt, prompt, reqFP)
			span.AddEvent("cache.hit.exact")
		} else {
			span.AddEvent("cache.check.semantic")
			if id, c := p.trySemantic(ctx, cfg.ProviderName(), model, turn, reqFP, wsID); c != nil {
				cached, layer = c, "cache_hit_semantic"
				hitIDs = []string{id}
				span.AddEvent("cache.hit.semantic")
			} else if p.poolGate.Participant(wsID) {
				// Private miss + this workspace opted into pooling: a poolable
				// requester may be served an entry CONTRIBUTED by another poolable
				// workspace, found under the un-prefixed pooled key. The serve
				// requires the contributor to ALSO be opted in (verified via the
				// owner stamped on the entry) — gated by MaybeAllowPooledHit, which
				// needs global + requester + contributor all true. Inert by
				// default: Participant is false when the gate is nil/off, so this
				// whole branch (and its extra cache read) never runs.
				if c, owner := p.tryExactPooled(ctx, cfg.ProviderName(), model, prompt, reqFP); c != nil && p.poolGate.MaybeAllowPooledHit(ctx, wsID, owner) {
					cached, layer = c, "cache_hit_pooled"
					hitHashes = answerHashes(cfg.ProviderName(), model, cachePrompt, prompt, reqFP)[1:]
					pooledHit = &poolroyalty.ServedHit{
						RequestID:            requestID,
						RequesterWorkspace:   wsID,
						ContributorWorkspace: owner,
						Layer:                "exact",
						EntryID:              p.exact.Key(cfg.ProviderName(), model, cache.FingerprintedKey(pooledPromptKey(prompt), reqFP)),
						Provider:             cfg.ProviderName(),
						Model:                model,
					}
					span.AddEvent("cache.hit.pooled")
				} else if c, owner, entryID, sim := p.trySemanticPooled(ctx, cfg.ProviderName(), model, turn, reqFP); c != nil && p.poolGate.MaybeAllowPooledHit(ctx, wsID, owner) {
					cached, layer = c, "cache_hit_pooled_semantic"
					hitIDs = []string{entryID}
					pooledHit = &poolroyalty.ServedHit{
						RequestID:            requestID,
						RequesterWorkspace:   wsID,
						ContributorWorkspace: owner,
						Layer:                "semantic",
						EntryID:              entryID,
						Provider:             cfg.ProviderName(),
						Model:                model,
						Similarity:           sim,
					}
					span.AddEvent("cache.hit.pooled_semantic")
				} else {
					metrics.RecordCacheMiss("cache_miss")
				}
			} else {
				// Both exact and semantic missed — a true cache miss. Symmetric
				// with the RecordCacheHit calls below.
				metrics.RecordCacheMiss("cache_miss")
			}
		}
		// B17.178: an answer stored before empty ones were refused at the write is not served either; the
		// request goes upstream and its fresh answer replaces the stored one.
		// B17.246: with Run code on, nor is one stored without the runs that worked it out — it would show no code.
		if cached != nil && (emptyAnswer(cached) || wrongSum(cmp.Or(turn.Latest, prompt), cached) ||
			runCodeAnswerOf(ctx) != nil && len(codeRunsIn(cached)) == 0) {
			cached, pooledHit, hitIDs, hitHashes = nil, nil, nil, nil
			metrics.RecordCacheMiss("cache_miss")
		}
		if cached != nil {
			// B17.246: a code-running answer is stored with the runs that worked it out; it is priced, counted,
			// served and replayed as answered, without them — serveRunningCode sends them as frames of their own.
			answered := withoutCodeRuns(cached)
			// Record the session turn first so the headers below reflect
			// the count + cost AFTER this turn lands. Cache hits have
			// zero cost.
			// LoggingNone skips the turn write entirely (privacy mode): the
			// turn's Prompt/Response are held in memory, so a none workspace
			// must not retain them — exactly like the upstream turn write
			// (~1390), which already gates on None. This cache-hit path did not.
			if sess != nil && loggingPolicy != workspace.LoggingNone {
				p.recordSessionTurn(ctx, sessionID, prompt, string(answered), model, len(prompt)/4, len(answered)/4, 0, true)
				setSessionHeaders(w, p, sessionID)
			}
			// PRICE THE HIT BEFORE ANY BYTES GO OUT. The serve precedes the settle by design (a
			// billing failure must never affect an already-served response), so the saving can only
			// reach the client if it is computed and headered here. Pure: no I/O, no settle.
			// Zero-valued and header-free for an own-cache hit (pooledHit == nil), which is free.
			// B17.29 — a pooled answer this workspace has already paid for is its own earlier answer:
			// priced and minted as an own-cache hit, free and nothing more to the contributor. chargedHit
			// is pooledHit unless so; a thumbs-down still reaches the pooled entry through pooledHit.
			// B27.26: a BYOK requester pays nothing for a pooled answer, so it funds — and mints — nothing.
			// B17.214: "auto" names no priced model, so a hit served to an Auto request is priced and recorded as
			// the model Lens would have chosen for it — the router's pick on this prompt, as the delegated path
			// below picks — never on the catalog's fallback floor (≈ 2 µLXC, with the contributor minted nothing).
			servedModel := model
			if strings.EqualFold(strings.TrimSpace(model), "auto") && p.router != nil {
				if pick := p.router.Route(ctx, cfg.ProviderName(), model, prompt).Model; pick != "" {
					servedModel = pick
				}
			}
			if pooledHit != nil {
				pooledHit.Model = servedModel
			}
			chargedHit := pooledHit
			if byok || p.pooledPaidBefore(ctx, wsID, pooledHit, cached) {
				chargedHit = nil
			}
			price := p.pricePooledServe(chargedHit, prompt, answered, holdMaxOut)
			price.setSavingHeaders(w)
			// B17.207 — a replay is free: a charged pooled serve marked one is a spend row its caller books as nothing.
			if price.ChargedULXC == 0 {
				w.Header().Set("X-Talyvor-Cache-Replay", "true")
			}
			if streaming {
				// SSE replay: synthesises the provider's streaming wire
				// format from the cached JSON so strict SSE clients can
				// consume cache hits the same way they consume live
				// responses. On parse failure we fall through and let
				// the regular streaming path call the LLM.
				if err := replayAsSSE(w, cfg.ProviderName(), answered); err == nil {
					if rc := runCodeAnswerOf(ctx); rc != nil {
						rc.served = cached
					}
					metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), layer).Inc()
					metrics.RecordCacheHit(layer)
					// SERVE point. FUNDING INVARIANT (settle-then-mint): resolve the pre-serve HOLD FIRST — a
					// pooled hit bills the consumer avoided_COGS (returned), an own hit is free — then mint a
					// royalty funded by what the consumer ACTUALLY paid ($0 charge ⇒ $0 mint).
					funded := p.settlePooledServe(ctx, wsID, price)
					p.mintPooledRoyalty(ctx, chargedHit, prompt, cached, funded, loggingPolicy)
					p.markPooledPaid(ctx, wsID, chargedHit, cached)
					// Cache-serve spend visibility (0100): the served request
					// becomes a zero-provider-cost token_events row tagged with
					// its layer, so hit rate is countable next to every miss.
					p.recordCacheServe(ctx, wsID, team, sprint, feature, servedModel, prompt, answered, modSet, sessionID, requestID, loggingPolicy, layer, price)
					p.rememberServed(ctx, wsID, requestID, layer, hitIDs, hitHashes, hitContributor(pooledHit), cached)
					span.SetAttributes(
						attribute.Bool("lens.cached", true),
						attribute.Float64("lens.cost_usd", 0),
					)
					span.SetStatus(codes.Ok, "")
					return
				}
				// NOT served: fall through to the live LLM below — the
				// pooled hit (if any) must NOT mint (claim at serve, not
				// at lookup).
				// ⚠ The saving headers were set above, before replayAsSSE could report failure. This
				// request is now going UPSTREAM and the consumer will pay full price for a live call,
				// so a surviving "you saved 276" header would promise a discount the ledger will not
				// show. Nothing has been flushed (replayAsSSE validates every frame before writing),
				// so removing them here is both safe and necessary.
				clearSavingHeaders(w)
				slog.Warn("proxy: cached payload not replayable as SSE; falling through to LLM",
					slog.String("provider", cfg.ProviderName()),
				)
			} else {
				writeBytes(w, http.StatusOK, answered)
				metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), layer).Inc()
				metrics.RecordCacheHit(layer)
				// SERVE point. FUNDING INVARIANT (settle-then-mint): resolve the pre-serve HOLD FIRST — a
				// pooled hit bills the consumer avoided_COGS (returned), an own hit is free — then mint a
				// royalty funded by what the consumer ACTUALLY paid ($0 charge ⇒ $0 mint).
				funded := p.settlePooledServe(ctx, wsID, price)
				p.mintPooledRoyalty(ctx, chargedHit, prompt, cached, funded, loggingPolicy)
				p.markPooledPaid(ctx, wsID, chargedHit, cached)
				// Cache-serve spend visibility (0100): see recordCacheServe.
				p.recordCacheServe(ctx, wsID, team, sprint, feature, servedModel, prompt, answered, modSet, sessionID, requestID, loggingPolicy, layer, price)
				p.rememberServed(ctx, wsID, requestID, layer, hitIDs, hitHashes, hitContributor(pooledHit), cached)
				span.SetAttributes(
					attribute.Bool("lens.cached", true),
					attribute.Float64("lens.cost_usd", 0),
				)
				span.SetStatus(codes.Ok, "")
				return
			}
		}
	}

	// Local-model short-circuit: simple queries from the default
	// workspace can be served by a local Ollama instance for free. On
	// any failure we fall through to the regular cloud path — local
	// routing must never break the main request. Multimodal requests
	// skip local entirely (the local text models can't serve images) and
	// fall through to the capability-aware cloud path below.
	if !byok && !modSet.Multimodal() && p.tryLocalRouting(w, ctx, cfg.ProviderName(), model, prompt, cachePrompt, reqFP, turn, wsID, team, sprint, feature, sessionID, requestID, piiDetected, redactedPrompt, p.agentStrategy(agentKeyID)) {
		return
	}

	// A PINNED multimodal stream to a model that can't serve the modality fails fast here rather than
	// streaming a wrong answer. An auto-routed one goes on to the capability redirect below, which
	// streams now reach (B15.3), exactly as a buffered auto request does.
	if streaming && modSet.Multimodal() && !modality.Supports(model, modSet) && !isAutoRoute(r, model) {
		metrics.ModalityUnsupported()
		writeError(w, http.StatusUnprocessableEntity,
			"streaming request contains "+modSet.Label()+" content but model "+model+" does not support it")
		return
	}
	// Output guardrails (Upgrade 13) can't inspect an in-flight stream. By
	// default they're not applied to streams (marked on the response); a
	// workspace can opt into buffering — which gives up streaming so the full
	// response can be inspected — by trading the SSE fast-path for the
	// buffered non-streaming path below.
	bufferedStream := false
	if streaming && p.guardrails != nil && p.guardrails.ShouldBufferStream(wsID) {
		w.Header().Set("X-Talyvor-Stream-Buffered", "true")
		streaming = false
		bufferedStream = true
	}

	// Compress the prompt before forwarding upstream — ONLY when this workspace
	// asked for it (compression_policy, migration 0117; see compression_gate.go
	// for the measurement that made it opt-in). Cache lookups above still key on
	// the uncompressed prompt so repeat callers hit cache. With the gate closed the
	// REWRITER changes nothing — ⚠ but NOT "the caller's bytes are forwarded", which
	// this said: rebuildBody collapses them (message_collapse_wire_test.go).
	compressedPrompt := prompt
	var savingsPct float64
	// compressionGateOpened is the POPULATION BOUNDARY for the durable measurement
	// below: it is true iff the rewriter actually ran on this request, whether or
	// not it changed a byte. The zeroes belong in the record — they are the
	// denominator, and 0-of-308 is what this rewriter measured — but the requests
	// it never saw do not.
	var compressionGateOpened bool
	if p.compressor != nil && p.shouldCompress(r, wsID) {
		compressionGateOpened = true
		result := p.compressor.Compress(ctx, prompt)
		compressedPrompt = result.CompressedPrompt
		savingsPct = result.SavingsPct
	}

	// Pick the model to send upstream. Router may downgrade to a cheaper
	// model in the same provider family; it never silently upgrades.
	upstreamModel := model
	var overrideModel, overrideReason string
	// Hoisted for the post-flush route-decision capture (descriptive, mint-free): the pre-cohort baseline, the
	// cohort's dims, and whether cohort intelligence overrode the baseline. Set only on the auto-route path.
	var rdBaselineModel, rdCohortBasis string
	var rdCohortOverrode, rdRouteWasAuto bool
	var rdCohortN int
	// Hoisted for the post-flush routing-PREDICTION emit (money-path producer, farm-gated on rdCohortOverrode):
	// the cohort-recommended model and the canonical complexity bucket of THIS request's cohort. Set only when
	// the auto-route branch runs; the emit binds them (with the input bucket) to cohort.DeriveInputCohort.
	var rdCohortModel, rdComplexityBucket string

	// Routing intelligence (Upgrade 22) engages ONLY when enabled AND the
	// request explicitly cedes the model choice ("auto" pseudo-model or the
	// X-Talyvor-Auto-Route header). A concrete model is PINNED and falls
	// through to the existing router path below — byte-for-byte unchanged.
	if p.routingAdvisor.Enabled() && isAutoRoute(r, model) {
		var allowedModels, allowedProviders []string
		if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok {
			allowedModels, allowedProviders = ws.AllowedModels, ws.AllowedProviders
		}
		// In-memory lookup only — never a DB query on the request path. The
		// Advisor is workspace-BLIND (it discards wsID); the Shape-1 gate below
		// adds NO cross-tenant read — its decisionTier is computed from THIS
		// request only (issue #198).
		// Compute the request's complexity ONCE (pure substring scans) and reuse it for both the
		// Advisor's pre-serve tier projection and the Shape-1 decisionTier below — never the
		// persisted post-serve WorkTier (unknowable here). The tier conditions the pick only when
		// LENS_ROUTING_TIER_COHORTS_ENABLED is on; otherwise Recommend ignores it.
		complexityScore := router.AnalyseComplexity(compressedPrompt).Score()
		rdComplexityBucket = string(worktier.ComplexityBucketFor(complexityScore)) // canonical cohort bucket (same input the router analysed)
		rec := p.routingAdvisor.Recommend(ctx, wsID, feature, len(compressedPrompt)/4, rdComplexityBucket, cfg.ProviderName(), allowedModels, allowedProviders)
		// Base-path pick: the model an auto request gets if NO recommendation is
		// applied (the complexity router). Pure/in-memory (no DB, no side
		// effects), computed once — it is BOTH the downgrade baseline for the
		// Shape-1 gate AND the model used when the gate suppresses the rec.
		var base router.RoutingDecision
		if p.router != nil {
			base = p.router.Route(ctx, cfg.ProviderName(), model, compressedPrompt)
			// B27.23: a named model the router would downgrade stays the baseline unless the
			// cheaper pick measures at or above it on this cohort ("auto" names no model, so passes).
			if ok, why := p.downgradeKeepsQuality(feature, len(compressedPrompt)/4, cfg.ProviderName(), model, base.Model); !ok {
				base = router.RoutingDecision{Model: model, Provider: base.Provider, Reason: why}
			}
		}
		// Shape-1 work-tier gate: a request-local, PRE-SERVE decisionTier can
		// only SUPPRESS the recommendation (sensitivity opt-out or downgrade
		// veto) — subtractive, never selecting a third model. decisionTier is
		// deliberately DISTINCT from the persisted WorkTier (input-size not
		// total, no cost, never stored; see routing_decision_tier.go).
		dt := newDecisionTier(len(compressedPrompt)/4, complexityScore, piiDetected, guardrailFired, loggingPolicy)
		res := resolveAutoRoute(p.router, rec, base, dt)
		// B27.23: a recommendation cheaper than the baseline stands only on measured quality.
		if res.applied {
			if ok, why := p.downgradeKeepsQuality(feature, len(compressedPrompt)/4, cfg.ProviderName(), base.Model, res.model); !ok {
				res = autoRouteResult{model: base.Model, reason: "routing held at the default (" + why + "): " + base.Reason, gated: gateQualityFloor, fallback: true}
			}
		}
		// Route-decision evidence (descriptive, mint-free): the pre-cohort baseline, the cohort dims, and
		// whether the cohort recommendation was APPLIED (i.e. overrode the baseline). res.applied is exactly
		// "cohort intelligence overrode the default"; res.model is baseline when not applied.
		rdRouteWasAuto = true
		rdBaselineModel = base.Model
		rdCohortOverrode = res.applied
		rdCohortBasis = string(rec.Basis)
		rdCohortN = rec.DistinctWorkspaces
		if res.model != "" {
			upstreamModel = res.model
			overrideModel = res.model
			overrideReason = res.reason
			// The cohort's ASSERTED model — captured here (not from upstreamModel, which a later circuit-breaker
			// override could replace) so the emitted prediction records what the cohort actually recommended.
			rdCohortModel = res.model
		}
		if res.applied {
			metrics.RoutingIntelligenceApplied()
		}
		if res.gated != "" {
			metrics.RoutingTierGated(res.gated)
		}
		if res.fallback {
			metrics.RoutingFallback()
		}
	} else if p.router != nil && p.routingDelegated(r, wsID, model) {
		// DELEGATED path ONLY — an "auto"/X-Talyvor-Auto-Route request, or a workspace
		// that opted in to cost-optimised routing. An explicitly NAMED model on a
		// non-consenting workspace is HONOURED: it never enters this branch, so the
		// base router can never silently downgrade it. The customer chose the model;
		// quality is theirs to decide (the founder's rule). The capability redirect
		// below is untouched — a correctness substitution, not a cost one.
		// B27.23: the router's downgrade is a guess from the prompt's shape; it stands only
		// when the cheaper model measures at or above the named one on this request's cohort.
		decision := p.router.Route(ctx, cfg.ProviderName(), model, compressedPrompt)
		// B17.116: "auto" names no model to rank a downgrade against, and no provider serves it — the router's pick
		// is the model, when the workspace's allow-list (if it keeps one) names it.
		namesNoModel := strings.EqualFold(strings.TrimSpace(model), "auto") && decision.Model != ""
		if p.workspaceManager != nil && namesNoModel {
			if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok && len(ws.AllowedModels) > 0 {
				namesNoModel = slices.Contains(ws.AllowedModels, decision.Model)
			}
		}
		if namesNoModel || p.router.ShouldOverride(model, decision) {
			if ok, why := p.downgradeKeepsQuality(feature, len(compressedPrompt)/4, cfg.ProviderName(), model, decision.Model); ok {
				upstreamModel = decision.Model
				overrideModel = decision.Model
				overrideReason = decision.Reason
				if why != "" {
					overrideReason += "; " + why
				}
			}
		}
	}

	// Transparency: when routing (on the delegated path) substituted a different
	// model than the caller named, ANNOUNCE it — mirroring X-Talyvor-Vision-Redirect.
	// A pinned/honoured model reaches here unchanged, so this fires only for auto /
	// opted-in traffic. The capability redirect and circuit breaker below set their
	// own headers for their own (correctness/safety) substitutions.
	if upstreamModel != model {
		w.Header().Set("X-Talyvor-Routed", model+"→"+upstreamModel)
	}

	// Routing Brain (H8.1) — OFFLINE-computed recommendation read as a cheap in-memory
	// lookup. AUTO-ROUTE only (a pinned model is the caller's explicit choice, never
	// touched); disabled/nil → the whole block is skipped and routing is byte-identical.
	// ADVISORY (default) surfaces the advice and leaves upstreamModel as the router's;
	// AUTONOMOUS (per-workspace opt-in) applies the brain's pick ONLY within the hard
	// floor (cost cap + verified/allowed), else falls back. Placed BEFORE the circuit
	// breaker + modality gates so those hard safety overrides still win over the brain.
	if isAutoRoute(r, model) && p.routingBrain.Enabled() && upstreamModel != "" {
		var brainAllowed []string
		if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok {
			brainAllowed = ws.AllowedModels
		}
		bModel, bReason, bApplied, bSurfaced := p.applyRoutingBrain(
			wsID, len(compressedPrompt)/4, router.AnalyseComplexity(compressedPrompt).Score(), upstreamModel, brainAllowed)
		// B27.23: the brain's pick, when cheaper, is held to the same measured floor — held, it stays advisory.
		if bApplied {
			if ok, _ := p.downgradeKeepsQuality(feature, len(compressedPrompt)/4, cfg.ProviderName(), upstreamModel, bModel); !ok {
				bApplied = false
			}
		}
		if bSurfaced {
			if bApplied {
				upstreamModel = bModel
				overrideModel = bModel
				overrideReason = bReason
				w.Header().Set("X-Talyvor-Brain", "applied")
				metrics.RoutingBrainApplied()
			} else {
				w.Header().Set("X-Talyvor-Brain", "advisory")
				metrics.RoutingBrainAdvisory()
			}
		}
	}

	// Circuit breaker override. When the alert manager has tripped a
	// circuit for this (workspace, team, feature), force the cheapest model for the
	// provider regardless of what the router decided. The X-Talyvor-Circuit-Open
	// header below tells the client this happened.
	circuitOpen := false
	if p.alertManager != nil && p.alertManager.IsCircuitOpen(wsID, team, feature) {
		circuitOpen = true
		upstreamModel = p.alertManager.GetDowngradeModel(cfg.ProviderName(), model)
	}

	// Capability hard constraint (Upgrade 15). A multimodal request MUST go
	// to a model that can serve the modality — unlike routing intelligence
	// (advisory), this is a hard gate on the FINAL model, and we never
	// silently strip the content and send text-only:
	//   - auto-route + incapable → redirect to the cheapest capable allowed
	//     model of this provider; if none → fail fast 422.
	//   - pinned + incapable → fail fast 422 (never silently serve an image
	//     at a model the caller explicitly pinned that can't see it).
	if modSet.Multimodal() && !modality.Supports(upstreamModel, modSet) {
		var allowedModels []string
		if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok {
			allowedModels = ws.AllowedModels
		}
		if isAutoRoute(r, model) {
			capable, ok := modality.CapableModel(cfg.ProviderName(), modSet, allowedModels)
			if !ok {
				metrics.ModalityUnsupported()
				writeError(w, http.StatusUnprocessableEntity,
					"request contains "+modSet.Label()+" content but no configured "+cfg.ProviderName()+" model supports it")
				return
			}
			w.Header().Set("X-Talyvor-Vision-Redirect", upstreamModel+"→"+capable)
			upstreamModel = capable
			overrideModel = capable
			overrideReason = "modality redirect: " + modSet.Label() + " requires a capable model"
			metrics.VisionRouteRedirect()
		} else {
			metrics.ModalityUnsupported()
			writeError(w, http.StatusUnprocessableEntity,
				"request contains "+modSet.Label()+" content but the requested model "+model+" does not support it")
			return
		}
	}

	// Streaming path: detected by "stream": true in the request JSON. The stream handler forwards SSE
	// chunks unbuffered, then caches the assembled response after the upstream stream completes.
	// B15.3: it branches HERE, after compression, routing (auto, cost-optimised, the brain), the
	// circuit breaker and the capability redirect, so a stream is sent to the same model with the same
	// prompt a buffered request would be, and billed at that model. The provider's own SSE carries the
	// answering model's id, so a client names the model that actually answered.
	if streaming {
		if p.guardrails != nil && p.guardrails.OutputEnabled() {
			w.Header().Set("X-Talyvor-Output-Guardrails", "not-applied-streaming")
		}
		// The routing headers the buffered path sets before WriteHeader, set before the stream commits.
		if overrideModel != "" {
			w.Header().Set("X-Talyvor-Model-Override", overrideModel)
		}
		if overrideReason != "" {
			w.Header().Set("X-Talyvor-Route-Reason", overrideReason)
		}
		if circuitOpen {
			w.Header().Set("X-Talyvor-Circuit-Open", "true")
		}
		if attr.Branch != "" {
			w.Header().Set("X-Talyvor-Branch", attr.Branch)
		}
		if willAttribute {
			w.Header().Set("X-Talyvor-Attributed", "true")
		}
		// The caller's body goes upstream as sent unless something above changed it: the compressed
		// prompt (rebuilt exactly as the buffered path rebuilds it) or a routed model.
		streamBody := body
		if compressedPrompt != prompt {
			rb, rerr := rebuildBody(body, upstreamModel, compressedPrompt)
			if rerr != nil {
				metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "error").Inc()
				writeError(w, http.StatusBadGateway, "rebuild request body: "+rerr.Error())
				return
			}
			streamBody = rb
		} else if upstreamModel != model {
			streamBody = setModelInBody(body, upstreamModel)
		}
		sh := &StreamHandler{proxy: p}
		// The billed model is the one sent upstream. The fallback input estimate mirrors the
		// non-streaming path: modality-aware for multimodal, else len(prompt)/4.
		estIn := len(prompt) / 4
		if modSet.Multimodal() {
			estIn = modSet.EstimateInputTokens()
		}
		sc := streamSpend{
			wsID: wsID, team: team, sprint: sprint, feature: feature,
			model: upstreamModel, asked: model, requestID: requestID, sessionID: sessionID,
			modality: modSet.Label(), logging: loggingPolicy, estInputTokens: estIn,
			tare: tareMeter, distillMethod: distillMethod, visionOCR: visionOCR, reqFP: reqFP, turn: turn,
			post: streamPostServe{
				provider: cfg.ProviderName(), requestBody: body, requestStart: requestStart,
				trackSession: sess != nil, piiDetected: piiDetected, guardrailFired: guardrailFired,
				compressedPrompt: compressedPrompt, savingsPct: savingsPct, compressionGateOpened: compressionGateOpened,
				routeWasAuto: rdRouteWasAuto, baselineModel: rdBaselineModel, cohortBasis: rdCohortBasis,
				cohortOverrode: rdCohortOverrode, cohortN: rdCohortN, cohortModel: rdCohortModel,
				complexityBucket: rdComplexityBucket, distillFacts: distillFacts,
			},
		}
		// B18.7: each provider streams through its OWN upstream and key — this was "OpenAI, else
		// Anthropic", so every other provider's stream was sent to Anthropic's URL with its key.
		streamCfg := cfg
		if byok {
			streamCfg, _ = p.ownKeyConfig(own, cfg.ProviderName())
			sc.byok = true
		}
		ops, oerr := providerStreamOps(streamCfg, upstreamModel, streamBody)
		if oerr != nil {
			writeError(w, http.StatusBadRequest, oerr.Error())
			metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "stream_unsupported").Inc()
			return
		}
		serr := sh.serve(w, r, cfg.ProviderName(), model, prompt, cachePrompt, streamBody, piiDetected, sc, ops)
		if serr != nil {
			metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "stream_error").Inc()
			return
		}
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "streamed").Inc()
		return
	}

	// B18.6: the caller's body goes upstream as sent — every turn, every role — with only the model
	// replaced when routing chose another, exactly as the streaming branch does. It used to be rebuilt
	// here as ONE user message holding every turn joined by "\n" whether or not anything had been
	// compressed, so a system instruction and the model's own earlier answers reached the provider as
	// things the user said. rebuildBody now runs only for a compressed prompt, which no workspace can
	// ask for any more (the prompt rewriter is retired).
	upstreamBodyOut := body
	err = nil
	if compressedPrompt != prompt {
		upstreamBodyOut, err = rebuildBody(body, upstreamModel, compressedPrompt)
	} else if upstreamModel != model {
		upstreamBodyOut = setModelInBody(body, upstreamModel)
	}
	if err == nil && bufferedStream {
		// Buffering for output guardrails: force the UPSTREAM call non-streaming
		// so it returns a parseable completion CheckOutput can inspect (the
		// client still gets a stream-shaped response below). Every OpenAI-
		// compatible provider — including Anthropic — reads this top-level
		// "stream" field verbatim; Google already uses the non-streaming
		// endpoint, so a single override covers all providers.
		upstreamBodyOut = disableStream(upstreamBodyOut)
	}
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "error").Inc()
		writeError(w, http.StatusBadGateway, "rebuild request body: "+err.Error())
		return
	}

	// forwardWithFallback owns translation, retry, and provider switching.
	// Input is the canonical OpenAI-shape body; output is also OpenAI-shape
	// (Gemini etc. are reverse-translated internally) so all downstream
	// caching / scoring / spend code operates on one schema regardless of
	// which provider actually answered.
	span.AddEvent("llm.forward.start")
	upstreamBody, statusCode, fbResult, err := p.forwardWithFallback(
		ctx, r, cfg.ProviderName(), upstreamModel, wsID, upstreamBodyOut, w, own,
	)
	attempts := fbResult.Attempts
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "error").Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusBadGateway, "upstream LLM error: "+err.Error())
		p.releaseReservation(ctx, "upstream error")
		return
	}

	// Score the response so we can gate caching on quality. Scoring is
	// pure-Go heuristics — fast enough to do on the hot path. Score is
	// only meaningful for a successful upstream (200); on errors we skip
	// scoring entirely.
	var qualityScore *quality.QualityScore
	if p.scorer != nil && statusCode == http.StatusOK {
		q := p.scorer.ScoreResponse(ctx, prompt, string(upstreamBody), cfg.ProviderName(), model)
		qualityScore = &q
	}

	scoreVal := 0.0
	if qualityScore != nil {
		scoreVal = qualityScore.Score
	}
	span.AddEvent("llm.forward.complete", trace.WithAttributes(
		attribute.Int("lens.attempts", attempts),
		attribute.Float64("lens.quality_score", scoreVal),
	))

	// Output guardrails (Upgrade 13) — full, non-streaming response. Off by
	// default (CheckOutput no-ops when disabled → behaves as today). On block
	// we write a 422 + reject the content but still record spend (the upstream
	// call already ran); on redact we re-inject the masked content so the
	// client, cache, and spend record all see the masked response.
	clientBody := upstreamBody
	clientStatus := statusCode
	outputBlocked := false
	if statusCode == http.StatusOK && p.guardrails != nil && p.guardrails.OutputEnabled() {
		w.Header().Set("X-Talyvor-Output-Guardrails", "applied")
		ogr := p.guardrails.CheckOutput(ctx, wsID, extractResponseContent(upstreamBody))
		for _, v := range ogr.Violations {
			metrics.GuardrailTriggered(v.Type, string(v.Action))
		}
		switch {
		case !ogr.Passed:
			outputBlocked = true
			blockType := "output"
			for _, v := range ogr.Violations {
				if v.Action == guardrails.ActionBlock {
					blockType = v.Type
				}
			}
			metrics.GuardrailBlock(blockType)
			w.Header().Set("X-Talyvor-Output-Guardrail-Blocked", "true")
			clientStatus = http.StatusUnprocessableEntity
			clientBody, _ = json.Marshal(map[string]any{
				"error":      "output guardrail violation",
				"violations": ogr.Violations,
			})
		case ogr.RedactedPrompt != "":
			upstreamBody = replaceResponseContent(upstreamBody, ogr.RedactedPrompt)
			clientBody = upstreamBody
			metrics.GuardrailRedaction("output_pii")
			w.Header().Set("X-Talyvor-Output-Redacted", "true")
		}
	}

	// Headers must be set BEFORE WriteHeader. X-Talyvor-* surface routing
	// decisions to the client; all of these go on the response, never on
	// the upstream request.
	if overrideModel != "" {
		w.Header().Set("X-Talyvor-Model-Override", overrideModel)
	}
	if overrideReason != "" {
		w.Header().Set("X-Talyvor-Route-Reason", overrideReason)
	}
	if circuitOpen {
		w.Header().Set("X-Talyvor-Circuit-Open", "true")
	}
	if qualityScore != nil {
		w.Header().Set("X-Talyvor-Quality-Score", strconv.FormatFloat(qualityScore.Score, 'f', 2, 64))
	}
	if attr.Branch != "" {
		w.Header().Set("X-Talyvor-Branch", attr.Branch)
	}
	if willAttribute {
		w.Header().Set("X-Talyvor-Attributed", "true")
	}
	if attempts > 1 {
		w.Header().Set("X-Talyvor-Attempts", strconv.Itoa(attempts))
	}
	// K4 CODE LOOP — return the gateway-bound output identity to the caller (a HASH, no raw content, safe to
	// expose to the workspace that produced it). Same (ws, model, prompt, response, served_at) as the
	// post-flush capture, so the header EQUALS the stored output_id. Pre-flush here (upstreamBody is the full
	// response, incl. buffered streams) → no added latency; TRUE incremental streaming (stream.go) is OUT for
	// now. DEFAULT-OFF (same flag as the verifier).
	if p.outputVerdictOn() && statusCode == http.StatusOK && len(upstreamBody) > 0 {
		oid, _, _ := deriveOutputID(wsID, upstreamModel, prompt, upstreamBody, requestStart)
		w.Header().Set("X-Talyvor-Output-Id", oid)
	}
	// Record session turn here (BEFORE WriteHeader) so the headers we set
	// next reflect the post-turn totals. Cost is computed against the
	// actually-billed upstream model so it matches the alerts pipeline.
	// LoggingNone skips the turn write entirely (privacy mode); metadata
	// and full both record it.
	if sess != nil && statusCode == http.StatusOK && loggingPolicy != workspace.LoggingNone {
		// B17.133: the provider's token counts and the catalog price the spend row is charged below, not len/4.
		turnIn, turnOut := len(prompt)/4, len(upstreamBody)/4
		turnCost, _ := alerts.CostUSDResolved(upstreamModel, catalog.PurposeCharge, turnIn, 0, 0, turnOut)
		if u, ok := cfg.ExtractUsage(upstreamBody); ok {
			turnIn, turnOut = u.InputTokens, u.OutputTokens
			turnCost, _ = alerts.CostUSDResolved(upstreamModel, catalog.PurposeCharge,
				u.UncachedInputTokens, u.CachedInputTokens, u.CacheWriteInputTokens, u.OutputTokens)
		}
		p.recordSessionTurn(ctx, sessionID, prompt, string(upstreamBody), upstreamModel, turnIn, turnOut, turnCost, false)
		setSessionHeaders(w, p, sessionID)
	}
	// forwardWithFallback always returns OpenAI-shape JSON, so we default
	// Content-Type to application/json. Streaming responses are handled in
	// a different code path (stream.go) and don't pass through here.
	if bufferedStream && clientStatus == http.StatusOK {
		// The client asked for a stream; we buffered the full response so the
		// output guardrails could inspect it. Deliver the (possibly redacted)
		// result as a single SSE event so the client still gets a stream-shaped
		// response — just not incrementally. A blocked output falls through to
		// the JSON 422 below (errors aren't streamed).
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len("data: ")+len(clientBody)+len("\n\ndata: [DONE]\n\n")))
		w.WriteHeader(clientStatus)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(clientBody) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- text/event-stream or application/json model answer, never HTML
		_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	} else {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(clientBody)))
		w.WriteHeader(clientStatus)
		_, _ = w.Write(clientBody) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/json unless the upstream set its own type; nosniff is set on every response
	}
	// B39.5: the answer is complete on the wire now, so the client is not kept waiting on the writes below
	// (each bounded at postAnswerWriteTimeout, but run one after another — 20 s with Postgres frozen, #174).
	_ = http.NewResponseController(w).Flush()

	// A blocked output is treated like a blocked request: no caching, no
	// spend, no attribution. The guardrail block itself is recorded
	// (metric + log + the 422 response); the workspace isn't billed for a
	// response we refused to return.
	if statusCode == http.StatusOK && !outputBlocked {
		// B17.282: the bill first. The answer is already on the wire, and the cache writes below can take seconds
		// (an embedding call, the pooled write); a client reading its ledger meanwhile saw the hold and no spend row.
		// Bill on the provider's REPORTED usage when it surfaces one — that
		// is the exact count the provider charged (and for multimodal it
		// already folds in the image cost, beating the flat per-image
		// estimate). Fall back to a len/4 estimate only when usage is absent,
		// and mark the row estimated HONESTLY so budgets/ROI know which rows
		// are real. For multimodal the fallback uses a modality-aware figure
		// (text chars + per-image) rather than len(prompt)/4, which would
		// count the flattened base64 blob.
		inT, outT := len(prompt)/4, len(upstreamBody)/4
		costEstimated := true
		spendSource := "estimated"
		// servedCostUSD is the ONE cost basis for this response — fed to the reservation SETTLE (the
		// customer's actual bill), the shadow debit, the in-memory budget feed, telemetry, and attribution
		// below. When the provider reports usage we price it CACHE-AWARE (CostUSDDetailed): cache reads and
		// writes at their OWN catalog rate, not the flat input rate. Without provider usage there is no cache
		// breakdown, so it stays the flat estimate — byte-identical to CostUSD(upstreamModel, inT, outT),
		// today's behaviour (CostUSDDetailed with zero cached/write equals CostUSD by construction anyway).
		var servedCostUSD float64
		// servedPriceBasis is "fallback" when the served model is absent from the catalog, so the charge
		// below is a derived floor rather than its real price. It rides onto the spend row.
		servedPriceBasis := ""
		// rdTokens carries the cache breakdown to the post-flush route-decision capture, which prices BOTH the
		// served and the counterfactual model with it. Hoisted out of the usage block below because that block
		// scopes `u`, and the capture runs after the response is flushed. ProviderReported=false until proven
		// otherwise, so a request with no usage report is labelled flat rather than assumed measured.
		rdTokens := routeTokens{UncachedInput: len(compressedPrompt) / 4, Output: outT}
		// B27.32: the input classes servedCostUSD is priced on, so the list price is priced on the same ones.
		var listUncached, listCached, listWrite int
		if u, ok := cfg.ExtractUsage(upstreamBody); ok {
			inT, outT = u.InputTokens, u.OutputTokens
			costEstimated = false
			spendSource = "provider_usage"
			listUncached, listCached, listWrite = u.UncachedInputTokens, u.CachedInputTokens, u.CacheWriteInputTokens
			var prov catalog.Provenance
			servedCostUSD, prov = alerts.CostUSDResolved(upstreamModel, catalog.PurposeCharge,
				u.UncachedInputTokens, u.CachedInputTokens, u.CacheWriteInputTokens, u.OutputTokens)
			if prov == catalog.ProvenanceFallback {
				servedPriceBasis = prov.String()
			}
			rdTokens = routeTokens{
				UncachedInput: u.UncachedInputTokens, CachedInput: u.CachedInputTokens,
				CacheWriteInput: u.CacheWriteInputTokens, Output: u.OutputTokens, ProviderReported: true,
			}
		} else if modSet.Multimodal() {
			inT = modSet.EstimateInputTokens()
		}
		if costEstimated {
			// No provider usage → no cache breakdown exists; the flat estimate IS today's basis. Priced
			// through the resolver so an unknown model still cannot come out at zero.
			var prov catalog.Provenance
			servedCostUSD, prov = alerts.CostUSDResolved(upstreamModel, catalog.PurposeCharge, inT, 0, 0, outT)
			listUncached = inT
			if prov == catalog.ProvenanceFallback {
				servedPriceBasis = prov.String()
			}
		}
		// Billing redesign: SETTLE the reservation to the DELIVERED cost (served model, real tokens) — the
		// customer pays what routing/batch/compression actually produced. shadow and settle are mutually
		// exclusive by the reservation flag, so no config ever charges twice.
		// settledChargeUSD = what the consumer ACTUALLY paid for this request. settleReservation RETURNS
		// the settled amount, which it CLAMPS to the hold (never bills above it) — so this is the real
		// charge, not the (possibly larger) delivered servedCostUSD. It funds the distill royalty below.
		// 0 when reservations are off (no settle) ⇒ the distill handoff writes a row the sweeper skips.
		// B23.2: OUTSIDE the logging gate. The settle is the agent's bill, not a log: inside it, a
		// LoggingNone workspace's hold was never settled and the stranded sweeper refunded it in full.
		// B27.26: a BYOK request was answered on the workspace's own key and is charged nothing here.
		settledChargeUSD := 0.0
		if byok {
			// no hold was taken and no charge is made
		} else if p.reservationActive() {
			settledChargeUSD = p.settleReservationBasis(ctx, servedCostUSD, upstreamModel, servedPriceBasis)
		} else {
			// B23.13: reservations off, the pre-serve agent debit was the estimate — settle it to the delivered cost.
			p.settleAgentDebit(ctx, wsID, servedCostUSD, upstreamModel, servedPriceBasis)
		}
		// The LXC charge — B17.17: OUTSIDE the logging gate too, for B23.2's reason. It is the bill, not
		// a log (the ledger row is an amount and "chat: metered usage", no content): inside the gate a
		// LoggingNone workspace's chat was shown a price and charged nothing. Void, post-serve, same ctx.
		// B1.6: a subscriber's non-agent request draws the plan allowance on EITHER arm below —
		// the session-key chat has no reservation, so the settle alone would charge it nothing.
		// B9.8: a chat request is charged by chargeChatUsage (allowance, then prepaid) and nothing else.
		subscriber := byok || p.chargeChatUsage(ctx, wsID, servedCostUSD) || p.chargeSubscriberUsage(ctx, wsID, servedCostUSD)
		if !p.reservationActive() && !subscriber {
			p.shadowSpendLXC(ctx, wsID, servedCostUSD)
		}
		// Cache iff the prompt has no PII AND the response is judged
		// cacheable by the quality scorer. Low-quality responses are
		// forwarded to the client but never persisted.
		shouldCache := !piiDetected
		if qualityScore != nil && !qualityScore.ShouldCache {
			shouldCache = false
		}
		if shouldCache {
			// Cache against the workspace-scoped (uncompressed) prompt +
			// originally requested model so repeat callers in the same
			// workspace get cache hits but other workspaces don't. The raw
			// prompt + wsID also feed the opt-in pooled (cross-tenant) write.
			p.storeAnswer(ctx, cfg.ProviderName(), model, cachePrompt, prompt, reqFP, turn, wsID, requestID, upstreamBody)
		}
		// W4.9 SHADOW POOL LOG — void, post-serve, default-off. Records that a fresh cacheable
		// response was produced, so the cross-tenant pooled hit rate is computable from repeats
		// while pooling stays OFF. Gated on the SAME shouldCache as the cache write above: a
		// response the product would not cache could never have been pooled either, so including
		// it would deflate the rate by construction.
		//
		// It honours LoggingNone, as storeCaches does since B18.4: a LoggingNone workspace's
		// response is neither cached nor pooled, so leaving it out of the observations is exact —
		// and writing a metadata row for a tenant that asked for no rows would not be acceptable.
		if shouldCache && loggingPolicy != workspace.LoggingNone {
			p.shadowPoolObservation(ctx, wsID, cfg.ProviderName(), model, prompt, reqFP,
				p.sharesAnswers(ctx, wsID))
		}
		eventPrompt := prompt
		if piiDetected {
			eventPrompt = redactedPrompt
		}
		// Logging policy gates the per-request observability writes. None
		// is the privacy escape hatch — every DB and NATS sink is bypassed.
		// Metadata keeps cost/token rows but strips prompt_text. Full keeps
		// everything (the historic behaviour).
		spendPrompt := eventPrompt
		if loggingPolicy == workspace.LoggingMetadata {
			spendPrompt = ""
		}
		if loggingPolicy == workspace.LoggingFull {
			// Learner publishes to NATS — too verbose for metadata mode.
			p.recordTokenEvent(ctx, cfg.ProviderName(), model, eventPrompt, upstreamBody, savingsPct, piiDetected, wsID)
		}
		// RecordSpend prices the model that was actually billed by the
		// LLM (the upstream model, after any router or circuit override).
		// Fire-and-forget — alert manager failures must never break a
		// successful request.
		if p.alertManager != nil && loggingPolicy != workspace.LoggingNone {
			// spendPrompt is "" in metadata mode (no prompt text persisted)
			// and the redacted form in full mode when PII was detected.
			// wsID + sprint travel on this single billing write so spend is
			// attributable per workspace / team / sprint (see migration 0028);
			// modality + estimated ride along too (migration 0029). distillMethod
			// rides along too (migration 0040): "convert" for a distilled request
			// (the saving is IMPLICIT in this row's lower count), "" otherwise.
			metrics.SpendRecord(spendSource)
			// B27.32: the row carries what this request would have cost at the model it asked for, and what
			// it was charged — the request's measured saving. On this one write only (sctx), never the OCR row.
			sctx := alerts.WithSaving(ctx, alerts.Saving{RequestedModel: model, ChargedUSD: servedCostUSD,
				ListUSD: alerts.ListCostUSD(model, upstreamModel, servedCostUSD, listUncached, listCached, listWrite, outT)})
			var recErr error
			if tareMeter.Kind != "" {
				recErr = p.alertManager.RecordSpendWithTare(sctx, wsID, team, sprint, feature, upstreamModel, inT, outT, spendPrompt, sessionID, requestID, modSet.Label(), costEstimated, distillMethod, tareMeter)
			} else if distillMethod != "" {
				recErr = p.alertManager.RecordSpendWithDistill(sctx, wsID, team, sprint, feature, upstreamModel, inT, outT, spendPrompt, sessionID, requestID, modSet.Label(), costEstimated, distillMethod)
			} else {
				recErr = p.alertManager.RecordSpend(sctx, wsID, team, sprint, feature, upstreamModel, inT, outT, spendPrompt, sessionID, requestID, modSet.Label(), costEstimated)
			}
			if recErr != nil {
				slog.Warn("alerts: RecordSpend failed",
					slog.String("err", recErr.Error()),
				)
			}
			// Routing-pattern capture (Phase-3) — post-serve, VOID, structurally
			// mint-free. Same logging gate + post-serve position as the cost_usd
			// write (a LoggingNone workspace gets neither); the opt-in WRITE gate
			// is in the sink SQL. cacheHit=false: this is the upstream model-call
			// path (cache hits short-circuit far earlier). Quality is the
			// just-scored value; latency is the real request elapsed. A finished
			// stream is scored and captured by its own seam (recordStreamPostServe).
			// scored = we actually computed a quality score (scorer wired AND
			// statusCode==200); an unscored/non-200 response must NOT write a
			// quality=0 row that poisons the Advisor's averages.
			// Routing-pattern earn-or-capture (S4) — mutually exclusive, ONE row.
			// earnPattern returns true ONLY when it took the corpus row (flag on
			// AND authenticated non-default workspace AND opted-in); in every
			// other state (incl. flag-OFF, the first guard) it returns false and
			// capturePattern runs byte-identical to before. Both are post-flush,
			// detached, void — neither can affect the served response.
			// Tier-cohort dimension for the routing corpus: the complexity bucket on the SAME
			// compressedPrompt the router/decisionTier/WorkTier use, so the captured/earned row's
			// bucket equals the live lookup bucket (write == lookup). Inert until
			// LENS_ROUTING_TIER_COHORTS_ENABLED reads it; '' on legacy rows is excluded from the tiered aggregate.
			routingComplexityBucket := string(worktier.ComplexityBucketFor(router.AnalyseComplexity(compressedPrompt).Score()))
			if !p.earnPattern(ctx, piiDetected, guardrailFired, loggingPolicy, feature, upstreamModel, cfg.ProviderName(), prompt, upstreamBody,
				len(compressedPrompt)/4, outT, scoreVal, qualityScore != nil, time.Since(requestStart).Milliseconds(), routingComplexityBucket) {
				p.capturePattern(ctx, piiDetected, guardrailFired, loggingPolicy, wsID, feature, upstreamModel, cfg.ProviderName(),
					len(compressedPrompt)/4, outT, scoreVal, qualityScore != nil, time.Since(requestStart).Milliseconds(), false, routingComplexityBucket)
			}
			// WorkTier descriptive classification — post-flush, off-hot-path, void,
			// best-effort, shares the obsLimiter; default-off. DESCRIPTIVE + mint-free
			// (the sink has no ledger handle). Complexity is derived on the SAME input
			// the router analyzed (compressedPrompt) so it equals the routing decision's.
			p.captureWorkTier(ctx, wsID, feature, upstreamModel, cfg.ProviderName(), compressedPrompt,
				len(compressedPrompt)/4, outT, piiDetected, guardrailFired, string(loggingPolicy))
			// Route-decision evidence — POST-FLUSH, off-path, obsLimiter-shed, detached, mint-free. Only on the
			// auto-route path (rdRouteWasAuto). Records baseline vs served model + estimated cost (the go/no-go
			// substrate). Cannot affect the already-flushed response. See routedecision_capture.go.
			if rdRouteWasAuto {
				p.captureRouteDecision(ctx, wsID, rdBaselineModel, upstreamModel, rdCohortBasis,
					rdCohortOverrode, rdCohortN, rdTokens)
				// Routing-PREDICTION live emit — POST-FLUSH, off-path, obsLimiter-shed, mint-free, default-off.
				// Fires ONLY when the cohort OVERRODE the baseline (rdCohortOverrode = the farm gate): that live
				// decision becomes a contributor prediction ("cohort C → rdCohortModel") the offline scorer grades
				// on the held eval slice and the minter pays on. Trivial requests (no override) emit nothing. See
				// routing_predict_emit.go.
				p.emitRoutingPrediction(ctx, wsID, feature, rdCohortModel, cfg.ProviderName(),
					rdCohortOverrode, len(compressedPrompt)/4, rdComplexityBucket)
			}
			// K4 intrinsic output verdict — POST-FLUSH, off-path, default-off, mint-free. Derives the
			// gateway-bound output id + checks the response against a constraint the REQUEST declared;
			// records a verdict (hashes only). Cannot affect the already-flushed response. See
			// output_verdict_capture.go.
			p.captureOutputVerdict(ctx, wsID, upstreamModel, cfg.ProviderName(), body, upstreamBody, prompt, requestStart)
			// COMPRESSION MEASUREMENT (W6.1 step 2) — POST-FLUSH, off-path, void,
			// obsLimiter-shed. Fires ONLY when the 0117 gate opened, and then whether
			// or not a byte changed: the unmodified rows ARE the denominator.
			//
			// It is written HERE, and not beside the rewrite at ~1240, for one
			// reason: inT and costEstimated do not exist yet up there. Those two are
			// what stop this from being a saving nobody received — with no provider
			// usage block the spend row is len(ORIGINAL)/4, so the bytes the
			// rewriter removed moved no money at all. Recording the wire delta
			// without recording which basis billed it would be the more flattering
			// number and the less true one.
			if compressionGateOpened {
				p.captureCompression(ctx, compressionObservation{
					requestID: requestID, workspaceID: wsID, model: upstreamModel,
					original: prompt, sent: compressedPrompt,
					billedInputTokens: inT, costEstimated: costEstimated,
				})
			}
			// S1 distill attribution (MINT-FREE) — record any consented
			// cross-tenant pooled-distill serves surfaced from MaybeDistill.
			// Post-flush, void, detached, swallowed (mirrors capturePattern);
			// self-serve already skipped upstream, LoggingNone suppressed in the
			// sink. Inert unless an attribution sink is wired.
			p.recordDistillServes(ctx, wsID, loggingPolicy, distillFacts, settledChargeUSD)
			// The vision-OCR sub-call's cost is its OWN row, tagged 'vision_ocr',
			// priced on the vision model, flagged estimated — a COST, never a
			// saving, NEVER blended into the 'convert' row above. The durable
			// monthly spend cap (SUM(cost_usd) over token_events) then includes it.
			p.recordVisionOCRSpend(ctx, wsID, team, sprint, feature, sessionID, requestID, visionOCR)
		}
		// Feed the in-memory budget totals from the SAME billed cost. This is
		// a memory update (+ threshold checks), not a second hot-path DB
		// write — token_events above is the durable record.
		if p.budgetService != nil {
			p.budgetService.RecordSpend(ctx, wsID, team, sprint, servedCostUSD)
		}
		p.limits.recordSpend(ctx, wsID, servedCostUSD) // B18.3: the month the spending cap is checked against
		// (The legacy branch_spend double-write was retired in #157 — it had no
		// reader since #158. request_attribution below is the sole attribution
		// write now; attr/willAttribute still drive the X-Talyvor-Branch echo.)
		// Upgraded per-request attribution. Always fired (the store
		// handles the empty-workspace case by skipping the insert) and
		// always async so a slow Postgres can't slow the response. Token
		// figures match the alerts pipeline; the cost is now the cache-aware
		// served basis (servedCostUSD — the same basis the reservation settles
		// on). NOTE: token_events.cost_usd is still computed inside RecordSpend
		// from raw tokens (flat, cache-blind), so it over-states cached requests
		// until that sink takes the breakdown — a known follow-up, out of the
		// proxy.go/stream.go scope. The customer's BILL (the settle) is correct.
		if p.attrStore != nil && loggingPolicy != workspace.LoggingNone {
			cost := servedCostUSD
			// The request id is what makes the issue joinable to the spend row; without it the
			// issue is stored and unusable (migration 0116).
			attrCtx := attribution.ExtractFromRequest(r)
			attrCtx.RequestID = requestID
			p.attributeRequester(r, wsID, &attrCtx)
			p.attrStore.RecordAsync(
				attrCtx,
				inT, outT, cost,
				upstreamModel, cfg.ProviderName(),
				time.Since(requestStart),
			)
		}
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "forwarded").Inc()
		span.SetAttributes(
			attribute.Bool("lens.cached", false),
			attribute.Float64("lens.cost_usd", servedCostUSD),
		)
		span.SetStatus(codes.Ok, "")
	} else {
		// B17.128: a refused request or a blocked output is charged nothing, so its hold goes back now
		// rather than staying taken until the stranded sweeper refunds it.
		released := fmt.Sprintf("upstream status %d", statusCode)
		if outputBlocked {
			released = "output guardrail block"
		}
		p.releaseReservation(ctx, released)
		metrics.RequestsTotal.WithLabelValues(cfg.ProviderName(), "upstream_error").Inc()
		span.SetStatus(codes.Error, fmt.Sprintf("upstream status %d", statusCode))
	}
}

// attributeRequester fills who made a request into its attribution from the credential (B18.18), so the
// author and user_id columns — and the by-author rollup — have a producer: a signed-in person's user id,
// with the name of the workspace the suite provisioned for them as the author, or an API key's id and
// name. Whatever the client sent itself is kept (attribution.FillRequester).
func (p *Proxy) attributeRequester(r *http.Request, wsID string, a *attribution.AttributionContext) {
	ctx := r.Context()
	actx := auth.GetAuthContext(ctx)
	switch {
	case actx != nil && actx.UserID != "":
		author := actx.UserID
		if actx.UserID == wsID && p.workspaceManager != nil {
			if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok && ws.Name != "" {
				author = ws.Name
			}
		}
		a.FillRequester(actx.UserID, author)
	case auth.GetAPIKey(ctx) != nil:
		k := auth.GetAPIKey(ctx)
		author := k.Name
		if author == "" {
			author = "key:" + k.ID
		}
		a.FillRequester("key:"+k.ID, author)
	case actx != nil && actx.APIKeyID != "":
		a.FillRequester("key:"+actx.APIKeyID, "key:"+actx.APIKeyID)
	}
}

// rebuildBody re-emits the JSON request body with the (possibly overridden)
// model and the compressed prompt collapsed into a single user message.
// All other fields (temperature, max_tokens, tools, ...) are preserved.
func rebuildBody(originalBody []byte, model, prompt string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(originalBody, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	m["messages"] = []map[string]any{
		{"role": "user", "content": prompt},
	}
	return json.Marshal(m)
}

// nodeInferReq / nodeInferResp mirror cmd/node's /inference wire shapes (the node package isn't
// importable here) — only the fields the gateway sends/reads.
type nodeInferReq struct {
	Model    string         `json:"model"`
	Messages []nodeInferMsg `json:"messages"`
}
type nodeInferMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type nodeInferResp struct {
	Text         string `json:"text"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// tryNodeRouting forwards the request to a REGISTERED inference node picked by the multi-router,
// authenticated with a short-lived gateway-signed token bound to {node_id, request_id, body_sha256,
// exp}. Returns true iff a node served it (response written). ANY miss/error returns false so the
// caller falls through to the existing path — never errors, never drops the request.
//
// Touches NO receipt/mint code: the node auto-signs + auto-submits its OWN receipt off the response
// path; minting stays gated downstream by stake + earn_verified. The gateway only forwards traffic.
func (p *Proxy) tryNodeRouting(
	w http.ResponseWriter,
	ctx context.Context,
	provider, model, prompt, cachePrompt, reqFP string, turn cache.Turn, wsID, team, sprint, feature, sessionID, requestID string,
	piiDetected bool,
	redactedPrompt string,
	strategy localrouter.RoutingStrategy, // F4 C.1: threaded from tryLocalRouting (price-aware for agents)
) bool {
	ep, err := p.nodeRouter.SelectEndpoint(model, strategy)
	if err != nil {
		return false // ErrNoHealthyEndpoint (or any selection error) → fall through to the direct path
	}
	body, err := json.Marshal(nodeInferReq{Model: model, Messages: []nodeInferMsg{{Role: "user", Content: prompt}}})
	if err != nil {
		return false
	}
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	exp := time.Now().Add(30 * time.Second).Unix() // short window; node enforces with a few-seconds skew
	token, err := povi.SignNodeAuthToken(p.lensChallengePriv, ep.ID, requestID, bodyHash, exp)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep.URL, "/")+"/inference", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lens-Node-Token", token)
	req.Header.Set("X-Request-ID", requestID)
	client := p.nodeHTTPClient
	if client == nil {
		client = safehttp.Client(30 * time.Second) // SSRF-guarded fallback (node URL is caller-supplied)
	}
	nodeStart := time.Now()
	resp, err := client.Do(req)
	nodeLatencyMs := time.Since(nodeStart).Milliseconds() // gateway-measured node round-trip — the latency signal
	if err != nil {
		slog.Warn("node-autoroute: forward failed, falling through", slog.String("node", ep.ID), slog.String("err", err.Error()))
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("node-autoroute: node returned non-200, falling through", slog.String("node", ep.ID), slog.Int("status", resp.StatusCode))
		return false
	}
	var nr nodeInferResp
	if err := json.NewDecoder(resp.Body).Decode(&nr); err != nil {
		slog.Warn("node-autoroute: decode failed, falling through", slog.String("err", err.Error()))
		return false
	}
	// Mint-basis gate (migration 0099): record Lens's OWN measurement of this
	// served request BEFORE flushing the response, so it is committed before the
	// node (which echoed X-Request-ID) submits its receipt. The PoVI mint prices
	// on THIS measured output-token count and requires node_id == ep.ID — never the
	// node's claimed nr.OutputTokens. len(text)/4 is the same house token
	// approximation billing uses, bounded by what the node actually produced and
	// Lens actually forwarded. Best-effort: on failure the row is absent and the
	// mint fails closed (mints nothing) — never over-mints, never fails the serve.
	if p.servedMeasure != nil {
		measuredOut := len(nr.Text) / 4
		if err := p.servedMeasure.Record(ctx, requestID, ep.ID, wsID, measuredOut); err != nil {
			slog.Warn("node-autoroute: served-measurement record failed (mint will fail closed)",
				slog.String("node", ep.ID), slog.String("request_id", requestID), slog.String("err", err.Error()))
		}
	}
	out, err := json.Marshal(nodeOpenAIEnvelope(model, nr))
	if err != nil {
		return false
	}
	w.Header().Set("X-Talyvor-Node-Served", ep.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out) // JSON API response (application/json), mirrors tryLocalRouting  // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/json from json.Marshal
	// B26.5: the agent's hold settles to what the node delivered, on the same len/4 measurement as the mint basis.
	p.settleServedOffSeam(ctx, wsID, model, len(prompt)/4, len(nr.Text)/4)
	// B27.2: and a chat or subscriber question pays the same, and counts toward budgets and limits. The node
	// decides how long its answer is, so no more output is charged than chat admission's own output bound
	// (the node is not sent max_tokens) — a floor, never an over-bill.
	chargedOut := min(len(nr.Text)/4, boundedMaxOut(0, p.reservationMaxOut))
	servedCostUSD, _ := alerts.CostUSDResolved(model, catalog.PurposeCharge, len(prompt)/4, 0, 0, chargedOut)
	p.chargeServedOffSeam(ctx, wsID, team, sprint, servedCostUSD)

	if !piiDetected {
		p.storeAnswer(ctx, provider, model, cachePrompt, prompt, reqFP, turn, wsID, requestID, out)
	}
	eventPrompt := prompt
	if piiDetected {
		eventPrompt = redactedPrompt
	}
	p.recordTokenEvent(ctx, provider, model, eventPrompt, out, 0, piiDetected, wsID) // learner store (pattern insights), NOT token_events — gated on the logging policy inside
	// Node-serve spend visibility: the token_events row that makes THIS request countable in the cache
	// hit rate as a MISS (serve_source='node') and, at cost_usd = its charge (B27.2), in budgets and the
	// spending cap. Without it a node serve is absent from the denominator and the rate reads HIGH.
	// Output tokens use nr.Text (len/4), matching the mint-basis measurement above. Void, post-serve,
	// best-effort — never affects the already-flushed response.
	p.recordNodeServe(ctx, wsID, team, sprint, feature, model, eventPrompt, nr.Text, sessionID, requestID, servedCostUSD)
	// DESCRIPTIVE (P3 #6): capture the gateway-measured node latency into the per-(node,cohort) aggregate,
	// off the serve path via the obsLimiter. Best-effort, void, mint-free — a capture failure never affects
	// the already-flushed response. Pure additive observation before the early-return.
	p.captureNodeLatency(ep.ID, model, feature, prompt, nodeLatencyMs)
	return true
}

// nodeOpenAIEnvelope wraps a node InferResponse in the OpenAI chat.completion shape the caller expects.
func nodeOpenAIEnvelope(model string, nr nodeInferResp) map[string]any {
	return map[string]any{
		"id":     "chatcmpl-node",
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": nr.Text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     nr.InputTokens,
			"completion_tokens": nr.OutputTokens,
			"total_tokens":      nr.InputTokens + nr.OutputTokens,
		},
	}
}

// chargeServedOffSeam makes the money calls the upstream seams make (proxy.go's buffered seam and
// recordStreamSpend) for a question answered by a node or by local routing — B27.2. Before it, a chat
// or subscriber question served off the seams was free and invisible to budgets and spending limits.
// The agent's own charge is settleServedOffSeam's; the calls below skip an agent exactly as they do on
// the seams. Both paths sit ahead of the streaming split, so a streamed request is charged here too.
func (p *Proxy) chargeServedOffSeam(ctx context.Context, wsID, team, sprint string, servedCostUSD float64) {
	subscriber := p.chargeChatUsage(ctx, wsID, servedCostUSD) || p.chargeSubscriberUsage(ctx, wsID, servedCostUSD)
	if !p.reservationActive() && !subscriber {
		p.shadowSpendLXC(ctx, wsID, servedCostUSD)
	}
	if p.budgetService != nil {
		p.budgetService.RecordSpend(ctx, wsID, team, sprint, servedCostUSD)
	}
	p.limits.recordSpend(ctx, wsID, servedCostUSD)
}

// tryLocalRouting attempts to serve the request from a locally-hosted
// Ollama model. Returns true if the request was fully handled (response
// written, caches/events updated). Any failure returns false so the
// caller falls through to the regular cloud path.
func (p *Proxy) tryLocalRouting(
	w http.ResponseWriter,
	ctx context.Context,
	provider, model, prompt, cachePrompt, reqFP string, turn cache.Turn, wsID, team, sprint, feature, sessionID, requestID string,
	piiDetected bool,
	redactedPrompt string,
	strategy localrouter.RoutingStrategy, // F4 C.1: StrategyPriceAware for agents, else the default
) bool {
	// Blocker 6: gateway auto-route to a REGISTERED node (flag-gated, default off). On any miss —
	// flag off, no node registry, no healthy node for the model, or any forward error — this
	// returns false and we fall through to the EXISTING legacy localRouter / cloud path below.
	// Byte-identical to today when the flag is off (the branch is never entered).
	if p.nodeAutoRouteEnabled && p.nodeRouter != nil {
		if p.tryNodeRouting(w, ctx, provider, model, prompt, cachePrompt, reqFP, turn, wsID, team, sprint, feature, sessionID, requestID, piiDetected, redactedPrompt, strategy) {
			return true
		}
	}
	if p.localRouter == nil {
		return false
	}
	decision := p.localRouter.ShouldUseLocal(router.AnalyseComplexity(prompt), wsID)
	if !decision.UseLocal {
		return false
	}

	raw, err := p.localRouter.Forward(ctx, decision.Model, prompt)
	if err != nil {
		slog.Warn("localrouter: forward failed, falling through to cloud",
			slog.String("model", decision.Model),
			slog.String("err", err.Error()),
		)
		return false
	}
	formatted, err := p.localRouter.FormatAsOpenAI(raw, decision.Model)
	if err != nil {
		slog.Warn("localrouter: format failed, falling through to cloud",
			slog.String("err", err.Error()),
		)
		return false
	}

	w.Header().Set("X-Talyvor-Local-Model", decision.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(formatted) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/json model answer
	// B26.5: the agent's hold settles to what the local model delivered, on the token counts the spend row uses.
	p.settleServedOffSeam(ctx, wsID, decision.Model, len(prompt)/4, len(formatted)/4)
	// B27.2: a local run costs Talyvor nothing, so a chat or subscriber question is charged 0 — through the
	// same calls as every other serve, and still recorded by the zero-cost spend row below.
	p.chargeServedOffSeam(ctx, wsID, team, sprint, 0)

	if !piiDetected {
		p.storeAnswer(ctx, provider, model, cachePrompt, prompt, reqFP, turn, wsID, requestID, formatted)
	}
	eventPrompt := prompt
	if piiDetected {
		eventPrompt = redactedPrompt
	}
	// Local runs are free to Talyvor, so the cost recorded by RecordSpend is 0
	// (the model isn't in the price table). An agent's hold is still settled
	// above, at the model's catalog charge rate or its floor (B26.5). recordTokenEvent stores
	// the local model name so usage analytics distinguish local from
	// cloud traffic (gated on the logging policy inside — full only).
	p.recordTokenEvent(ctx, provider, decision.Model, eventPrompt, formatted, 0, piiDetected, wsID)
	// Honour the logging policy for the token_events write, exactly like the
	// upstream (~1459) and node (recordNodeServe) paths: none persists NOTHING
	// (skip the row entirely), metadata strips prompt_text, full keeps it.
	// Without this a none/metadata local serve leaked the prompt to token_events
	// — the same shape as the ungated learner call above.
	if logging := p.loggingPolicyFor(wsID); p.alertManager != nil && logging != workspace.LoggingNone {
		spendPrompt := eventPrompt
		if logging == workspace.LoggingMetadata {
			spendPrompt = ""
		}
		// Local routing is text-only (multimodal skips it upstream). The token
		// counts here are ESTIMATED (len/4) — a deliberate, known asymmetry
		// with the cloud paths, which now meter on the provider's reported
		// usage object. Local backends are reached via the local router / node
		// path and surface only aggregate counts, not a per-request provider
		// usage object, so there is nothing exact to read here. It's harmless:
		// the billed COST is exactly 0 (local is free — the model isn't in the
		// price table), so the estimated token counts never affect spend, and
		// the row is marked not-estimated because the cost itself is exact (0).
		// Fold real usage in here only if/when local routing exposes a
		// per-request usage object.
		_ = p.alertManager.RecordSpend(ctx, wsID, team, sprint, feature, decision.Model, len(prompt)/4, len(formatted)/4, spendPrompt, sessionID, requestID, "text", false)
	}
	metrics.RequestsTotal.WithLabelValues(provider, "local").Inc()
	return true
}

// loggingPolicyFor reads a workspace's logging policy the single canonical way
// (default Metadata when there is no manager or the workspace is unknown), so
// every content-persistence gate — the learner and the local-path RecordSpend
// below — decides on ONE value: the same one serve() resolved at request entry
// and recordNodeServe re-reads.
func (p *Proxy) loggingPolicyFor(wsID string) workspace.LoggingPolicy {
	if p.workspaceManager == nil {
		return workspace.LoggingMetadata
	}
	return p.workspaceManager.GetLoggingPolicy(wsID)
}

func (p *Proxy) recordTokenEvent(ctx context.Context, provider, model, prompt string, response []byte, savingsPct float64, piiDetected bool, wsID string) {
	if p.learner == nil {
		return
	}
	// Honour the workspace logging policy at the CHOKEPOINT. The learner
	// persists the raw prompt+response to a 30-day NATS stream, so it fires ONLY
	// on LoggingFull — exactly like the upstream path (~1435). none (the privacy
	// escape hatch, every DB+NATS sink bypassed) AND metadata (cost/tokens only,
	// prompt_text stripped) both opt out of content persistence, so neither may
	// reach the learner. Gating here — not per call site — closes the class of
	// bug: the node-serve and local-routing callers were missed once; any future
	// caller is gated by construction.
	if p.loggingPolicyFor(wsID) != workspace.LoggingFull {
		return
	}
	// len/4 is the same token approximation the router and compressor use.
	_ = p.learner.Record(ctx, learner.TokenEvent{
		Provider:     provider,
		Model:        model,
		Prompt:       prompt,
		Response:     string(response),
		InputTokens:  len(prompt) / 4,
		OutputTokens: len(response) / 4,
		Cached:       false,
		Compressed:   savingsPct > 0,
		SavingsPct:   savingsPct,
		PIIDetected:  piiDetected,
	})
}

// recordStreamSpend closes the streamed-spend gap. Streamed requests used to
// record no spend at all — invisible to budgets/alerts. This bills on the
// captured provider usage when present (cost_estimated=false), else a len/4
// estimate (cost_estimated=true), so a streamed request is never invisible.
// Respects the workspace logging policy (None opts out, like non-streaming).
// Prompt text isn't persisted on the streamed spend row (metadata-equivalent);
// the durable prompt record is the learner token-event written alongside.
// recordStreamSpend writes a streamed request's spend row and charge, and returns what the consumer
// actually paid (the settled reservation, 0 when reservations are off) — the distill royalty's funding,
// as on the buffered seam — and what a chat request was charged in µLXC, -1 for any other (B17.129).
func (p *Proxy) recordStreamSpend(ctx context.Context, sc streamSpend, u streamUsage, outputText string) (float64, int64) {
	inT, outT, estimated, servedCostUSD := streamServedCost(sc, u, outputText)
	// Feed the in-memory budget totals from the SAME billed cost whatever the logging policy, as the
	// buffered seam does (B15.3: this sat behind the logging gate, so a none-logging workspace's streams
	// never reached its budgets).
	if p.budgetService != nil {
		p.budgetService.RecordSpend(ctx, sc.wsID, sc.team, sc.sprint, servedCostUSD)
	}
	p.limits.recordSpend(ctx, sc.wsID, servedCostUSD) // B18.3: as on the buffered seam
	// Reservation SETTLE (the customer's bill). Fires on storeCtx = WithoutCancel(r.Context()) (stream.go),
	// which carries the reservation handle, so the streamed settle happens in-band instead of stranding the
	// hold for the sweeper to refund. B23.2: before the logging gate, as on the buffered seam — a
	// LoggingNone workspace's agent is billed too.
	settled := 0.0
	if sc.byok {
		// B27.26: answered on the workspace's own key — no hold was taken and no charge is made, as buffered
	} else if p.reservationActive() {
		settled = p.settleReservation(ctx, servedCostUSD, sc.model)
	} else {
		p.settleAgentDebit(ctx, sc.wsID, servedCostUSD, sc.model, "") // B23.13, as on the buffered seam
	}
	// The LXC charge, mutually exclusive with the settle above by the flag. B17.17: before the logging
	// gate, as on the buffered seam — a LoggingNone workspace's chat is charged too.
	// B1.6: the same allowance draw as the buffered seam (see there).
	chatULXC, chat := int64(-1), false
	if !sc.byok {
		if c, ok := p.chargeChat(ctx, sc.wsID, servedCostUSD); ok {
			chatULXC, chat = c, true
		}
	}
	subscriber := sc.byok || chat || p.chargeSubscriberUsage(ctx, sc.wsID, servedCostUSD)
	if !p.reservationActive() && !subscriber {
		p.shadowSpendLXC(ctx, sc.wsID, servedCostUSD)
	}
	if p.alertManager == nil || sc.logging == workspace.LoggingNone {
		return settled, chatULXC
	}
	source := "estimated"
	if !estimated {
		source = "provider_usage"
	}
	metrics.SpendRecord(source)
	// B27.32: the request's measured saving on its own row, as on the buffered seam — never the OCR row.
	sctx := alerts.WithSaving(ctx, streamSaving(sc, u, inT, outT, servedCostUSD))
	var recErr error
	// B7.3 — the same three writers, in the same order, as the buffered seam: a distilled streamed
	// request tags its row 'convert', and its OCR sub-call gets its own 'vision_ocr' row.
	if sc.tare.Kind != "" {
		recErr = p.alertManager.RecordSpendWithTare(sctx, sc.wsID, sc.team, sc.sprint, sc.feature, sc.model, inT, outT, "", sc.sessionID, sc.requestID, sc.modality, estimated, sc.distillMethod, sc.tare)
	} else if sc.distillMethod != "" {
		recErr = p.alertManager.RecordSpendWithDistill(sctx, sc.wsID, sc.team, sc.sprint, sc.feature, sc.model, inT, outT, "", sc.sessionID, sc.requestID, sc.modality, estimated, sc.distillMethod)
	} else {
		recErr = p.alertManager.RecordSpend(sctx, sc.wsID, sc.team, sc.sprint, sc.feature, sc.model, inT, outT, "", sc.sessionID, sc.requestID, sc.modality, estimated)
	}
	if recErr != nil {
		slog.Warn("alerts: streamed RecordSpend failed", slog.String("err", recErr.Error()))
	}
	p.recordVisionOCRSpend(ctx, sc.wsID, sc.team, sc.sprint, sc.feature, sc.sessionID, sc.requestID, sc.visionOCR)
	return settled, chatULXC
}

// streamServedCost is the ONE cost basis for a streamed response — the reservation SETTLE (the customer's
// bill), the budget feed, the shadow debit and the attribution row. Cache-aware when the provider reports
// usage; the flat estimate otherwise. Mirrors the buffered path exactly.
//
// ⚠ AND "EXACTLY" NOW MEANS IT (W6.13-series, W6.16). This priced through alerts.CostUSDDetailed /
// alerts.CostUSD, and BOTH return exactly ZERO for a model the catalog does not hold — while the
// buffered path prices through the RESOLVER on both of its branches, for the reason it states there:
// "so an unknown model still cannot come out at zero". The catalog holds 45 models; `gpt-4`,
// `gpt-4-turbo` and `claude-3-opus` are not among them. So the same request cost two different
// amounts depending on whether the client asked for a stream, and a hard_block budget could not be
// reached by streamed traffic it booked at zero. The fallback is not a price invented here: it is the
// one alerts.WarnUnpricedModel already describes — the provider's cheapest known model for a charge,
// "a floor, never an over-bill".
// The fallback, when it is taken, announces itself: CostUSDResolved calls
// alerts.WarnUnpricedModel, which increments UnpricedModelRequests and logs at
// ERROR with the model name. That is the half the plain helpers had no way to do.
func streamServedCost(sc streamSpend, u streamUsage, outputText string) (inT, outT int, estimated bool, costUSD float64) {
	if u.present {
		costUSD, _ = alerts.CostUSDResolved(sc.model, catalog.PurposeCharge,
			u.uncachedInputTokens, u.cachedInputTokens, u.cacheWriteInputTokens, u.outputTokens)
		return u.inputTokens, u.outputTokens, false, costUSD
	}
	inT, outT = sc.estInputTokens, len(outputText)/4
	costUSD, _ = alerts.CostUSDResolved(sc.model, catalog.PurposeCharge, inT, 0, 0, outT)
	return inT, outT, true, costUSD
}

// streamSaving is a streamed request's measured saving, priced on the same token classes as
// streamServedCost: the provider's breakdown when it reported one, the flat estimate otherwise.
func streamSaving(sc streamSpend, u streamUsage, inT, outT int, chargedUSD float64) alerts.Saving {
	asked := sc.asked
	if asked == "" { // a direct StreamHandler call names only the model it sent
		asked = sc.model
	}
	list := alerts.ListCostUSD(asked, sc.model, chargedUSD, inT, 0, 0, outT)
	if u.present {
		list = alerts.ListCostUSD(asked, sc.model, chargedUSD, u.uncachedInputTokens, u.cachedInputTokens, u.cacheWriteInputTokens, u.outputTokens)
	}
	return alerts.Saving{RequestedModel: asked, ListUSD: list, ChargedUSD: chargedUSD}
}

// recordVisionOCRSpend books a distilled request's vision-OCR sub-call as its OWN 'vision_ocr'
// token_events row. ONE writer for the buffered and streamed seams (B7.3): the streamed seam had
// none, so a scanned document sent with stream:true never recorded what its OCR call cost.
func (p *Proxy) recordVisionOCRSpend(ctx context.Context, wsID, team, sprint, feature, sessionID, requestID string, visionOCR visionSpend) {
	if !visionOCR.recorded() {
		return
	}
	// The OCR row is a cost_estimated row (document/image token
	// accounting is approximate), so keep the SpendRecord source label
	// within its bounded domain (provider_usage|estimated).
	metrics.SpendRecord("estimated")
	// A successful OCR must name its model so the cost prices (an empty
	// model → cost_usd=0, unbudgeted). Production always sets it; warn
	// loudly if a dispatcher ever doesn't, rather than silently $0.
	if visionOCR.model == "" {
		slog.Warn("distill: vision-OCR cost recorded WITHOUT a model — it cannot be priced (cost_usd=0)",
			slog.String("workspace_id", wsID),
			slog.Int("ocr_input_tokens", visionOCR.inputTokens),
			slog.Int("ocr_output_tokens", visionOCR.outputTokens),
		)
	}
	if err := p.alertManager.RecordSpendWithDistill(ctx, wsID, team, sprint, feature, visionOCR.model, visionOCR.inputTokens, visionOCR.outputTokens, "", sessionID, requestID, "document", true, "vision_ocr"); err != nil {
		slog.Warn("alerts: vision-OCR RecordSpend failed",
			slog.String("err", err.Error()),
		)
	}
}

// tryExact and trySemantic read the workspace's OWN copies. B26.7: a LoggingNone workspace has none —
// storeCaches writes nothing for it — so neither reads one either, and an answer kept before the switch
// to "none" is never replayed: every repeat goes to the model. Gated here, as the write is, so both
// seams (the SSE replay is the streamed one) are covered. The pooled reads are another workspace's
// answer and stay open to it.
func (p *Proxy) tryExact(ctx context.Context, provider, model, prompt, workspaceID string) []byte {
	if p.exact == nil || p.loggingPolicyFor(workspaceID) == workspace.LoggingNone {
		return nil
	}
	cached, err := p.exact.Get(ctx, provider, model, prompt)
	if err != nil || cached == nil {
		return nil
	}
	return cached
}

func (p *Proxy) trySemantic(ctx context.Context, provider, model string, turn cache.Turn, reqFP, workspaceID string) (string, []byte) {
	if p.semantic == nil || p.loggingPolicyFor(workspaceID) == workspace.LoggingNone {
		return "", nil
	}
	id, cached, err := p.semantic.GetWithID(ctx, provider, model, turn, reqFP, workspaceID)
	if err != nil || cached == nil {
		return "", nil
	}
	return id, cached
}

// trySemanticPooled is the cross-tenant SEMANTIC read surface: a similarity
// search over is_poolable rows only (a separate keyspace from the private
// search, which filters those out). It embeds the latest question (B16.1) —
// matching how pooled rows are written — and returns the body plus the contributing
// workspace plus the matched row's id and similarity (Stage-2.1 royalty
// attribution data). A miss is (nil, "", "", 0).
func (p *Proxy) trySemanticPooled(ctx context.Context, provider, model string, turn cache.Turn, reqFP string) ([]byte, string, string, float64) {
	if p.semantic == nil {
		return nil, "", "", 0
	}
	body, owner, entryID, sim, err := p.semantic.GetPooled(ctx, provider, model, turn, reqFP)
	if err != nil || body == nil {
		return nil, "", "", 0
	}
	return body, owner, entryID, sim
}

// mintPooledRoyalty fires the Stage-2.1 Pool-B royalty mint for a SERVED
// pooled hit. Called ONLY at the serve points (after replayAsSSE succeeds or
// the cached body was written) — never at lookup. hit is nil for private hits
// and for non-pooled traffic, making this a no-op on every pre-Stage-2.1
// path; a nil royaltyMinter (flag off / not wired) is equally inert.
//
// avoided_COGS is what the requester's live call would have cost, priced with
// the existing estimated-tokens convention (len/4 — the proxy.go:1091 shape).
// Exactly-once enforcement lives in the Minter's request_id claim row; mint
// failure is logged and never affects the already-served response (the claim
// rolls back with the credit, so a later retry can still mint).
// recordCacheServe books the token_events row for a CACHE-SERVED response, so the cache hit rate
// — the closed trial's core number, the one that prices the BYOK subscription — is countable from
// the SAME table every other spend reader uses (0100: serve_source). Post-serve, void,
// best-effort: a failure must never affect the already-served response.
//
// ⚠ The row's cost_usd = 0 is TALYVOR'S provider cost (nothing was bought upstream). What the
// REQUESTER pays is decided by the reservation seam, not this row (billing redesign):
//   - OWN cache hit: FREE. No upstream call, no contributor, so the pre-serve hold is RELEASED in
//     full (resolveCacheReservation → releaseReservation). The customer's balance is unchanged; a
//     spend UI renders it "served from cache — free". Charging a self-hit would be pure extraction.
//   - CROSS-TENANT (pooled) hit: the requester is billed avoided_COGS — the value received — and the
//     CONTRIBUTOR is minted s·avoided_COGS (s=0.5) from the SAME number; Talyvor keeps (1−s). This is
//     the ONE case Talyvor earns from a cache hit, and it earns by paying the contributor, never by
//     charging a user for their own repeat.
//
// (The prior comment here claimed the estimate debit "stands" on a hit and that "our margin is the
// cache" — that was the wrong model and is deleted: every bill reduction is the customer's; Talyvor's
// margin is the cross-tenant royalty split, not an un-refunded charge for a self-serve hit.)
//
// Gates mirror the upstream recording seam exactly: alertManager wired + the workspace not opted
// out via LoggingNone (symmetric with recordStreamSpend and the buffered-path seam). Token counts
// are the same length-derived estimators the miss path uses when a provider reports no usage —
// a cache serve never has provider usage, so the row is always cost_estimated.
//
// B27.32: the row also carries the serve's measured saving — the live call it replaced, at the model asked
// for, against what the hit was priced at (price: nothing for an own hit, the discounted price for a pooled one).
func (p *Proxy) recordCacheServe(ctx context.Context, wsID, team, sprint, feature, model, prompt string, served []byte, modSet modality.ModalitySet, sessionID, requestID string, loggingPolicy workspace.LoggingPolicy, layer string, price pooledPrice) {
	if p.alertManager == nil || loggingPolicy == workspace.LoggingNone {
		return
	}
	inT, outT := len(prompt)/4, len(served)/4
	if modSet.Multimodal() {
		inT = modSet.EstimateInputTokens()
	}
	charged := float64(price.ChargedULXC) * economy.LXCUSDValue / 1e6
	ctx = alerts.WithSaving(ctx, alerts.Saving{RequestedModel: model, ChargedUSD: charged,
		ListUSD: alerts.ListCostUSD(model, model, charged, inT, 0, 0, outT)})
	if err := p.alertManager.RecordCacheServe(ctx, wsID, team, sprint, feature, model, inT, outT, sessionID, requestID, modSet.Label(), layer); err != nil {
		slog.Warn("alerts: RecordCacheServe failed",
			slog.String("layer", layer),
			slog.String("err", err.Error()),
		)
	}
}

// recordNodeServe books the token_events row for a request served by a REGISTERED NODE, so it COUNTS
// in the cache hit-rate denominator as a MISS (no cache produced the bytes) — closing the node-serve
// denominator gap. Post-serve, void, mint-free: a failure never affects the already-flushed response.
// SKIPPED for LoggingNone workspaces, exactly like every other token_events write (upstream RecordSpend,
// cache recordCacheServe, streamed spend), preserving the privacy invariant — an opt-out workspace
// persists NOTHING, so its hit rate is computed over an empty set, not a partial one. The logging
// policy is read the same way the request handler reads it (GetLoggingPolicy, default Metadata).
// `served` is the node's output text (len/4 output tokens, matching the mint-basis measurement); the
// prompt is used only for the input-token length — no prompt bytes are persisted (the insert stores ”).
func (p *Proxy) recordNodeServe(ctx context.Context, wsID, team, sprint, feature, model, prompt, served, sessionID, requestID string, costUSD float64) {
	if p.alertManager == nil {
		return
	}
	logging := workspace.LoggingMetadata
	if p.workspaceManager != nil {
		logging = p.workspaceManager.GetLoggingPolicy(wsID)
	}
	if logging == workspace.LoggingNone {
		return
	}
	inT, outT := len(prompt)/4, len(served)/4
	// B27.32: a node answers at the model asked for, so its saving is only output it was not charged for.
	ctx = alerts.WithSaving(ctx, alerts.Saving{RequestedModel: model, ChargedUSD: costUSD,
		ListUSD: alerts.ListCostUSD(model, model, costUSD, inT, 0, 0, outT)})
	if err := p.alertManager.RecordNodeServe(ctx, wsID, team, sprint, feature, model, inT, outT, sessionID, requestID, "text", costUSD); err != nil {
		slog.Warn("alerts: RecordNodeServe failed", slog.String("request_id", requestID), slog.String("err", err.Error()))
	}
}

func (p *Proxy) mintPooledRoyalty(ctx context.Context, hit *poolroyalty.ServedHit, prompt string, served []byte, fundedUSD float64, loggingPolicy workspace.LoggingPolicy) {
	if p.royaltyMinter == nil || hit == nil {
		return
	}
	// B25.2: a pooled serve between two test (synthetic) workspaces mints its royalty, marked test (0173) —
	// never paid out, never counted in real totals. Between a test and a real one it mints nothing: the pool
	// is partitioned so that serve should not happen, and the minter's wall (B25.1) refuses it regardless.
	if p.isSynthetic(hit.RequesterWorkspace) != p.isSynthetic(hit.ContributorWorkspace) {
		slog.Info("poolroyalty: mint skipped — test and real money never mix",
			slog.String("request_id", hit.RequestID), slog.String("contributor", hit.ContributorWorkspace))
		return
	}
	// THE FUNDING INVARIANT: a cross-tenant royalty is funded by the consumer's ACTUAL charge for this
	// request — fundedUSD, the settled reservation from resolveCacheReservation. If the consumer paid
	// NOTHING (a plain key that never reserved, an unmetered lane, a released/own-cache hit, or a settle
	// that failed) mint NOTHING: an unfunded royalty mints value from nowhere and would let royalties-out
	// exceed payments-in. Refuse it and record WHY, so the contributor's absence of pay is explained and
	// not mysterious — the real remedy for free traffic is METERING THE CONSUMER, not minting regardless.
	if fundedUSD <= 0 {
		slog.Info("poolroyalty: mint skipped — consumer paid $0 for this request (unmetered/plain-key/own-cache/failed-settle); royalty UNFUNDED, not minted",
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
		)
		return
	}
	// Stage 2.3.0 — NO HASH -> NO MINT (the privacy-coherence gate): a
	// none-LoggingPolicy requester forbids persisting content-derived
	// artifacts, so we capture no evidence hashes and fire no mint at all.
	// The serve itself already happened and is unaffected. Defense in depth:
	// the Minter independently refuses empty-hash hits.
	if loggingPolicy == workspace.LoggingNone {
		slog.Info("poolroyalty: mint skipped — requester logging policy 'none' forbids evidence capture (no hash, no mint)",
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
		)
		return
	}
	// Evidence hashes, computed AT SERVE over the served cache-entry bytes
	// (on the SSE branch the wire format is synthesized FROM these bytes —
	// the entry, not the frames, is the adjudicable artifact): both cache
	// stores are mutable underneath the mint (Redis SET overwrite/TTL;
	// semantic upsert replaces response), so only a serve-time hash binds
	// the adjudicable content. Unsalted pure content hashes — the salted
	// identities already live on the claim row via entry_id. NOTE the
	// privacy posture: metadata-policy requesters persist NO prompt text,
	// but DO persist this prompt digest (pseudonymous, same class as the
	// existing prompt_embeddings.prompt_hash) — only policy 'none' opts a
	// workspace out of digests, at the cost of its pooled serves not minting.
	hit.AnswerSHA256 = poolroyalty.SHA256Hex(served)
	hit.PromptSHA256 = poolroyalty.SHA256Hex([]byte(prompt))
	// The mint BASIS is what the consumer ACTUALLY paid for this request (fundedUSD = the settled charge,
	// ≤ avoided_COGS and ≤ the hold), NEVER an independently computed avoided_COGS that could exceed the
	// bill. So mint = s × fundedUSD and Talyvor's margin = (1−s) × fundedUSD — both bounded by the payment.
	hit.AvoidedCOGSUSD = fundedUSD
	// The serve already happened — a client disconnect after receiving the
	// response must not cancel the contributor's royalty mid-transaction.
	// WithoutCancel keeps the request's values (trace) but detaches its
	// cancellation; the mint is synchronous-after-flush, so it adds no
	// client-visible latency either way. Bounded, so a mint stuck on a lock gives up (B39.4).
	mctx, cancel := detachBounded(ctx)
	defer cancel()
	res, err := p.royaltyMinter.MintServedHit(mctx, *hit)
	if err != nil {
		// On a clean error the tx rolled back (claim + credit together) and a
		// retry can mint; on an ambiguous commit error the claim may have
		// persisted, in which case the retry is suppressed — deflationary
		// either way, never a double-mint.
		slog.Warn("poolroyalty: mint failed (response already served; claim rolled back or commit ambiguous — a retry may mint, and can never double-mint)",
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
			slog.String("requester", hit.RequesterWorkspace),
			slog.String("error", err.Error()),
		)
		return
	}
	// ⚠ EVERY OTHER REFUSAL IS ALSO SAID OUT LOUD. The minter declines in nine places and returns
	// (Result{}, nil) from all of them, because a refused royalty must never fail the serve it rode
	// on. That was correct and completely opaque: a deployment with the flag ON, the gates open and
	// pooled hits serving produced no claim row, no ledger row and no log line, so "nothing was due"
	// and "something refused" were indistinguishable without a debugger.
	//
	// The most common reason is expected and benign — a workspace reading its OWN pooled entry is
	// recorded as serve_source='cache_hit_pooled' and correctly earns nothing — which is exactly why
	// counting that column is not a way to tell whether royalties should be appearing.
	//
	// INFO, not WARN: these are decisions, not faults, and the one that IS a fault (a mint error)
	// already has its own Warn above. Bounded by pooled traffic, one line per refused pooled hit.
	if res.Refused != "" && !res.Capped {
		attrs := []any{
			slog.String("gate", res.Refused),
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
			slog.String("requester", hit.RequesterWorkspace),
			slog.Float64("funded_usd", fundedUSD),
		}
		// ⚠ NAME THE SIGNAL, NOT JUST THE GATE. "owner_linkage" alone sent us looking for a defect
		// in a guard that was working: two workspaces funded by the SAME CARD are one operator, and
		// refusing a royalty between them is the wash-trade protection doing its job. Saying which
		// signal matched turns an alarming refusal into a self-explaining one.
		if res.LinkageSignal != "" {
			attrs = append(attrs,
				slog.String("linkage_signal", res.LinkageSignal),
				slog.String("meaning", "these two workspaces are treated as ONE OPERATOR, so a "+
					"royalty between them would be self-dealing. Expected when the same card funded "+
					"both. For a royalty to mint here the two sides must be separately funded."))
		}
		slog.Info("poolroyalty: mint refused — no royalty for this pooled hit", attrs...)
		return
	}
	if res.Capped {
		// 2.3b: serve-but-skip, same shape as the no-hash gate — the customer
		// was served before the mint ran; the pair's window budget is spent.
		slog.Info("poolroyalty: mint skipped — window cap reached (serve unaffected; exposure bounded)",
			slog.String("cap", res.CapReason),
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
			slog.String("requester", hit.RequesterWorkspace),
		)
		return
	}
	if res.Minted {
		slog.Info("poolroyalty: royalty minted",
			slog.String("request_id", hit.RequestID), // client X-Talyvor-Request-ID (tracing only)
			slog.String("mint_key", res.RequestID),   // SEC-11: server-derived claim key = the pool_royalty_mints row id
			slog.String("contributor", hit.ContributorWorkspace),
			slog.String("requester", hit.RequesterWorkspace),
			slog.String("layer", hit.Layer),
			slog.Int64("amount_ulens", res.Amount),
		)
	}
}

// pooledPromptKey is cache.PooledPromptKey — the shared-pool key material (raw prompt under the
// reserved NUL marker), kept as a local alias so the serve path reads unchanged. Single source of
// truth lives in internal/cache so the L·seed warm-start tool keys pooled entries identically.
func pooledPromptKey(prompt string) string { return cache.PooledPromptKey(prompt) }

// tryExactPooled looks up the shared POOLED key (marker + raw prompt, no wsID)
// and returns the cached body plus the contributing workspace recorded on it.
// It is the cross-tenant read surface: a separate keyspace from the
// workspace-private keys tryExact uses, so it can never leak a private entry. A
// miss is (nil, "").
func (p *Proxy) tryExactPooled(ctx context.Context, provider, model, rawPrompt, reqFP string) ([]byte, string) {
	if p.exact == nil {
		return nil, ""
	}
	body, owner, err := p.exact.GetWithOwner(ctx, provider, model, cache.FingerprintedKey(pooledPromptKey(rawPrompt), reqFP))
	if err != nil || body == nil {
		return nil, ""
	}
	return body, owner
}

// storeCaches writes the response to the workspace-private exact key (now
// owner-tagged via SetWithOwner — additive, the private key is unchanged) and to
// the semantic cache. When the contributing workspace has opted into pooling AND
// the global switch is on, it ALSO writes a copy under the un-prefixed pooled
// key, tagged with the contributor — the only cross-tenant-readable surface.
// Inert by default: a nil/off gate writes no pooled copy. Callers gate this on
// !piiDetected, so a PII-flagged entry is never stored, hence never pooled.
// reqFP (B15.1) is the request's fingerprint: every entry written here answers only a request with
// the same one. turn (B16.1) is what the semantic entries answer: its latest question, after its
// history.
func (p *Proxy) storeCaches(ctx context.Context, provider, model, cachePrompt, rawPrompt, reqFP string, turn cache.Turn, wsID string, response []byte) (stored bool) {
	// ⚠ B18.4: logging_policy = none STORES NO CONTENT — no exact entry, no semantic entry, no pool
	// contribution. Decided 27 Sep (docs/retention-none-and-the-semantic-cache.md): until then this
	// wrote the answer verbatim into prompt_embeddings whatever the policy. Gated HERE, not per call
	// site, so all four (buffered, local and node routing, streamed) are covered by construction.
	// Such a workspace can still be served from the shared pool: reading stores nothing.
	if p.loggingPolicyFor(wsID) == workspace.LoggingNone {
		return false
	}
	// B17.178: an answer that says nothing is passed to its asker, never stored for the next one.
	if emptyAnswer(response) {
		return false
	}
	// B17.238: nor is an answer that gets its question's sum wrong.
	if wrongSum(cmp.Or(turn.Latest, rawPrompt), response) {
		return false
	}
	if p.exact != nil {
		// Private (workspace-scoped) entry — today's behavior, now owner-stamped.
		_ = p.exact.SetWithOwner(ctx, provider, model, cache.FingerprintedKey(cachePrompt, reqFP), wsID, response)
		// Pooled (cross-tenant) copy under the reserved, namespace-disjoint pooled
		// key — opt-in, inert by default, and only from a workspace whose prompts are
		// checked for personal data (B15.7, sharesAnswers).
		if p.sharesAnswers(ctx, wsID) {
			_ = p.exact.SetWithOwner(ctx, provider, model, cache.FingerprintedKey(pooledPromptKey(rawPrompt), reqFP), wsID, response)
		}
	}
	if p.semantic != nil && p.embedder != nil && turn.Comparable() {
		// Private (workspace-scoped) semantic entry, is_poolable=false. B16.1: it embeds the latest
		// question only — the history is matched by hash, not by similarity — and that one vector is
		// the pooled copy's too.
		vec, err := p.embedder.Embed(ctx, turn.Latest)
		if err != nil {
			return true
		}
		_ = p.semantic.Set(ctx, provider, model, cachePrompt, turn, reqFP, response, vec, wsID)
		// Pooled (cross-tenant) semantic copy — opt-in, inert by default. Keyed on
		// the NUL-sentinel pooled prompt (disjoint hash) but carrying the latest
		// question's un-prefixed vector so cross-tenant similar questions match
		// cleanly; tagged with the contributor + is_poolable=true.
		// ⚠ AND AN UNVERIFIABLE PROMPT BUYS NOTHING. GetPooled refuses ahead of Embed for the
		// reason stated at that seam — "a prompt that can never be served must not cost a paid
		// embedding call to discover that" — and the WRITE owes the same answer. Since the
		// empty-extraction fix, a pooled row whose canonical form is empty is unreachable from
		// BOTH directions: the reader refuses on an empty canon, and `discriminators = $6` never
		// matches the NULL stored here. So this call bought a billed embedding, and a row, for an
		// entry with a structurally zero chance of ever being served.
		//
		// Measured over the poolsafety corpora (Canon is pure Go, so measuring costs nothing):
		// 118/150 deduped CONSUMER prompts and 19/146 ENGINEERING prompts have an empty canonical
		// form — on the consumer lane, four in five pooled writes.
		//
		// ⚠ SCOPED TO THE SEMANTIC WRITE ON PURPOSE. The exact pooled write above is keyed on
		// byte-identical prompt text, so it is servable without an entity gate. Gating it too
		// would drop real cross-tenant exact hits.
		//
		// B9.7: WITH THE PAIR VERIFIER WIRED the entity-free prompt IS servable — the verifier judges
		// the stored question against the asked one — so it is written, and the row keeps the
		// question text (SetPooled).
		if p.sharesAnswers(ctx, wsID) && (discriminator.Canon(turn.Latest).Verifiable() || p.semantic.VerifiesPairs()) {
			_ = p.semantic.SetPooled(ctx, provider, model, pooledPromptKey(rawPrompt), turn, reqFP, wsID, response, vec)
		}
	}
	return true
}

// sharesAnswers decides whether this workspace's answer may be written to the shared pool: it opted in
// (DecidePoolableOnWrite) AND its prompts are checked for personal data. B15.7 — "personal data is never
// shared" rests on that check, which keeps a flagged answer out of every cache (callers write only when
// !piiDetected); a workspace that turned it off would contribute answers nobody checked. Its own cache
// is unaffected, and turning detection back on resumes sharing on the next answer.
func (p *Proxy) sharesAnswers(ctx context.Context, wsID string) bool {
	return !poolOff(ctx) && p.poolGate.DecidePoolableOnWrite(ctx, wsID) && p.guardrails.DetectsPII(wsID)
}

// forward wraps the upstream call in retry.Do so transient 429/5xx
// responses are retried with exponential backoff before the proxy gives
// up. The closure builds a fresh request each attempt (bytes.NewReader
// keeps the body re-readable). Returns the final response, its body,
// the attempt count, and any non-retryable error.
func (p *Proxy) forward(ctx context.Context, r *http.Request, body []byte, model string, cfg providerConfig) (resp *http.Response, respBody []byte, attempts int, err error) {
	// Observe upstream provider latency + outcome on EVERY return path. The
	// named returns let one deferred RecordUpstream cover both success and
	// error without restructuring the call below. Bounded labels only; this
	// never alters control flow or the returned error.
	start := time.Now()
	defer func() {
		metrics.RecordUpstream(upstreamProviderLabel(cfg.ProviderName()), upstreamStatusClass(resp, err), time.Since(start))
	}()

	// The round-trip body moved VERBATIM to internal/inference.RunUpstream (PR-3b A′) — header-copy
	// skip-Host + setAuth-overwrite + retry.Do/httpClient.Do, exactly as forward_authheaders_test.go and
	// forward_retry_test.go pin it. forward keeps its signature + the deferred RecordUpstream above and
	// passes the closures cfg already holds (the ProviderConfig type does NOT move).
	//
	// ⚠ THE INBOUND CREDENTIAL IS REMOVED HERE AND NOWHERE ELSE. RunUpstream copies every header it
	// is given onto the provider request and then runs setAuth — which is a PER-PROVIDER closure, so
	// "setAuth overwrites the client's Authorization" holds only for the branches that write
	// Authorization. Measured across all nine shapes of that switch: google's setAuth is empty (its
	// key rides the URL), vllm's is conditional on an operator key, bedrock's discards its own error
	// when AWS credentials are absent — and NO branch touches X-Talyvor-Key, while only anthropic's
	// collides with X-API-Key. The caller's `tlv_` workspace key (or, via the legacy X-API-Key path,
	// the global admin key) was therefore forwarded verbatim to the provider. Sanitising the header
	// set at this seam keeps RunUpstream the byte-for-byte round-trip its oracles pin.
	// upstream_credential_leak_test.go drives all nine shapes × all three locations.
	// A scorer passes nil; StripCredentialHeaders answers nil for nil.
	// THE URL IS ENDPOINT-DEPENDENT. Every provider config returns a fixed chat URL regardless of
	// the inbound path (see endpoint.go), so an embeddings request was forwarded to
	// /v1/chat/completions and 400'd. Non-chat endpoints resolve their own URL from the same
	// configured base, so an operator's override still applies.
	upstreamURL := cfg.UpstreamURL(model)
	if ep := classifyEndpoint(r); ep == endpointEmbeddings {
		eu, ok := embeddingsURLFor(cfg.ProviderName(), upstreamURL)
		if !ok {
			// Refuse rather than forward an embeddings body to a chat URL — that silent
			// mismatch is exactly the defect this replaces.
			return nil, nil, 0, fmt.Errorf("proxy: provider %q has no embeddings endpoint", cfg.ProviderName())
		}
		upstreamURL = eu
	}
	body = inference.AdaptReasoningParams(model, body)
	// B17.11: OpenAI serves its -pro and -codex models only on /v1/responses (inference/responses_api.go), and
	// B17.104 GPT-5.6 and later their function tools. The chat body goes there translated, and a successful
	// reply comes back as a chat completion.
	translate := cfg.ProviderName() == "openai" && inference.ServedByResponses(model, body) && classifyEndpoint(r) == endpointChat
	if translate {
		ru, ok := inference.ResponsesURLFor(upstreamURL)
		if !ok {
			return nil, nil, 0, fmt.Errorf("proxy: %s needs OpenAI's /v1/responses, and %q has no such endpoint", model, upstreamURL)
		}
		if body, err = inference.ToResponsesBody(body); err != nil {
			return nil, nil, 0, fmt.Errorf("proxy: %s: %w", model, err)
		}
		upstreamURL = ru
	}
	resp, respBody, attempts, err = inference.RunUpstream(ctx, p.httpClient, p.retryConfig, upstreamURL, cfg.ApplyAuth, body, auth.StripCredentialHeaders(r.Header))
	if translate && err == nil && resp != nil && resp.StatusCode/100 == 2 {
		if chat, cerr := inference.FromResponsesBody(respBody); cerr == nil {
			respBody = chat
		}
	}
	return resp, respBody, attempts, err
}

// upstreamProviderLabel guards the provider metric label so an empty provider
// can never produce a blank series (and never panics).
func upstreamProviderLabel(provider string) string {
	if provider == "" {
		return "unknown"
	}
	return provider
}

// upstreamStatusClass normalizes an upstream result to a BOUNDED status label:
// "1xx"/"2xx"/"3xx"/"4xx"/"5xx", or "error" for a transport failure / no
// response. Never the raw code or error string (cardinality).
func upstreamStatusClass(resp *http.Response, err error) string {
	if err != nil || resp == nil {
		return "error"
	}
	switch resp.StatusCode / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "error"
	}
}

func readLimitedBody(r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func extractPrompt(body []byte) (model, prompt string, err error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", err
	}

	var sb strings.Builder
	for i, m := range req.Messages {
		if i > 0 {
			sb.WriteByte('\n')
		}
		// Content is usually a string but may be an array of content blocks
		// (Anthropic). For caching purposes, string form is canonical; fall
		// back to raw JSON so block-form prompts still hash deterministically.
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			sb.WriteString(s)
		} else {
			sb.Write(m.Content)
		}
	}
	return req.Model, sb.String(), nil
}

// streamRequested reports whether the JSON body has "stream": true. Parse
// errors fall back to false — the request is treated as non-streaming and
// downstream JSON validation in extractPrompt will surface real malformations.
func streamRequested(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	return m.Stream
}

// disableStream sets the top-level "stream" field to false on a canonical
// (OpenAI-shape) request body so the upstream returns a non-streaming
// completion. Used when buffering for output guardrails. Every OpenAI-
// compatible provider (incl. Anthropic, which Lens forwards verbatim) reads
// this field; Google uses a non-streaming endpoint regardless. Best-effort:
// on a parse error the body is returned unchanged.
func disableStream(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["stream"] = false
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func writeBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/json
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// recordSessionTurn is the per-request hook into SessionTracker. It
// keeps the call sites concise: a single helper that captures all the
// per-turn fields without spreading session.Turn construction across
// the proxy. Errors are best-effort — session tracking must never
// break the main response.
func (p *Proxy) recordSessionTurn(ctx context.Context, sessionID, prompt, response, model string, inT, outT int, cost float64, cached bool) {
	if p.sessionTracker == nil || sessionID == "" {
		return
	}
	_ = p.sessionTracker.RecordTurn(ctx, sessionID, session.Turn{
		Role:         "user",
		Prompt:       prompt,
		Response:     response,
		Model:        model,
		InputTokens:  inT,
		OutputTokens: outT,
		CostUSD:      cost,
		Cached:       cached,
		CreatedAt:    time.Now().UTC(),
	})
}

// setSessionHeaders stamps the post-turn totals onto the response. Call
// AFTER recordSessionTurn but BEFORE any WriteHeader so the headers are
// part of the committed response.
func setSessionHeaders(w http.ResponseWriter, p *Proxy, sessionID string) {
	if p.sessionTracker == nil || sessionID == "" {
		return
	}
	if s, ok := p.sessionTracker.GetSession(sessionID); ok {
		w.Header().Set("X-Talyvor-Session-Cost", strconv.FormatFloat(s.TotalCostUSD, 'f', 6, 64))
		w.Header().Set("X-Talyvor-Session-Turns", strconv.Itoa(s.TurnCount))
	}
}

// replayAsSSE re-emits a cached non-streaming response as the provider's
// SSE wire format so strict streaming clients can consume cache hits.
// Frames are computed before any header is committed: on parse failure
// the function returns the error and w is left untouched so the caller
// can fall through cleanly.
func replayAsSSE(w http.ResponseWriter, provider string, cached []byte) error {
	var frames [][]byte
	var err error
	switch provider {
	case "openai", "google", "mistral", "groq", "vllm", "bedrock":
		// B18.7: every provider but Anthropic streams OpenAI chunks, and its cached entry is OpenAI-shaped.
		frames, err = openAIReplayFrames(cached)
	case "anthropic":
		frames, err = anthropicReplayFrames(cached)
	default:
		return fmt.Errorf("replayAsSSE: unknown provider %q", provider)
	}
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		_, _ = w.Write(frame) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- text/event-stream replay of cached frames
		if flusher != nil {
			flusher.Flush()
		}
	}
	return nil
}

func openAIReplayFrames(cached []byte) ([][]byte, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(cached, &parsed); err != nil {
		return nil, fmt.Errorf("replayAsSSE openai: decode: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("replayAsSSE openai: no choices in cached payload")
	}
	id := "cache-" + uuid.NewString()
	content := parsed.Choices[0].Message.Content

	deltaPayload, err := json.Marshal(map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"delta":         map[string]any{"content": content},
			"finish_reason": nil,
		}},
	})
	if err != nil {
		return nil, err
	}
	stopPayload, err := json.Marshal(map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{
		[]byte("data: " + string(deltaPayload) + "\n\n"),
		[]byte("data: " + string(stopPayload) + "\n\n"),
		[]byte("data: [DONE]\n\n"),
	}, nil
}

func anthropicReplayFrames(cached []byte) ([][]byte, error) {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(cached, &parsed); err != nil {
		return nil, fmt.Errorf("replayAsSSE anthropic: decode: %w", err)
	}
	if len(parsed.Content) == 0 {
		return nil, fmt.Errorf("replayAsSSE anthropic: no content blocks in cached payload")
	}
	text := parsed.Content[0].Text

	startPayload, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	deltaPayload, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	stopPayload, _ := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	messageStopPayload, _ := json.Marshal(map[string]any{
		"type": "message_stop",
	})

	return [][]byte{
		[]byte("event: content_block_start\ndata: " + string(startPayload) + "\n\n"),
		[]byte("event: content_block_delta\ndata: " + string(deltaPayload) + "\n\n"),
		[]byte("event: content_block_stop\ndata: " + string(stopPayload) + "\n\n"),
		[]byte("event: message_stop\ndata: " + string(messageStopPayload) + "\n\n"),
	}, nil
}

// fallbackAttempt describes one entry in the ordered list of (provider,
// model) pairs forwardWithFallback walks until something succeeds.
type fallbackAttempt struct {
	provider string
	model    string
}

// forwardWithFallback dispatches the request to the original provider
// and, if that attempt is fallback-eligible (5xx, 429, or transport
// error), walks the configured chain trying alternates. Input body is
// canonical OpenAI shape; output is always reverse-translated to that
// same shape so downstream cache + scoring + spend logic doesn't have
// to know which provider actually replied.
func (p *Proxy) forwardWithFallback(
	ctx context.Context,
	r *http.Request,
	provider, model, wsID string,
	body []byte,
	w http.ResponseWriter,
	own map[string]string,
) ([]byte, int, fallback.FallbackResult, error) {
	attempts := []fallbackAttempt{{provider: provider, model: model}}
	if p.fallbackRouter != nil {
		chain := p.fallbackRouter.GetChain(provider)
		// Spec caps total to 3 attempts: original + 2 fallbacks.
		for i, t := range chain {
			if i >= 2 {
				break
			}
			attempts = append(attempts, fallbackAttempt{provider: t.Provider, model: t.Model})
		}
	}

	var (
		lastBody   []byte
		lastStatus int
		lastErr    error
		lastUsed   = attempts[0]
	)

	for i, a := range attempts {
		cfg := p.configForProvider(a.provider)
		if cfg.ProviderName() == "" {
			// Unknown provider name in the chain — treat as a no-op and move on.
			continue
		}
		// B27.26: a BYOK request goes only to providers the workspace holds a key for, on that key — never on
		// Talyvor's, which it is not charged for.
		if own != nil {
			ownCfg, ok := p.ownKeyConfig(own, a.provider)
			if !ok {
				continue
			}
			cfg = ownCfg
		}

		// Key selection. When a pool is configured we pick a healthy key
		// per attempt and override cfg's auth/url closures to use it; the
		// per-attempt choice means a fallback retry doesn't reuse a key
		// that just failed. When the pool is empty for this provider we
		// silently fall back to the single configured key.
		var poolKey *keypool.PoolKey
		if p.keyPool != nil && own == nil {
			if pk, perr := p.keyPool.Get(a.provider); perr == nil && pk != nil {
				cfg = p.applyKey(cfg, pk.Key)
				poolKey = pk
			}
		}

		// Non-original attempts: rewrite the body's model field so the
		// fallback target sees the model it actually supports. The other
		// fields (messages, temperature, tools…) carry through unchanged.
		attemptBody := body
		if i > 0 {
			attemptBody = setModelInBody(body, a.model)
		}
		// B17.217: an Anthropic Messages body goes to another provider rewritten as a chat body, or not at all.
		crossWire := provider == "anthropic" && a.provider != "anthropic"
		if crossWire {
			ob, ok := anthropicToOpenAIChat(attemptBody)
			if !ok {
				continue
			}
			attemptBody = ob
		}

		// BuildRequest nil-guards the translator: identity for OpenAI-compatible providers, the gemini/
		// bedrock request translation otherwise — same result as the prior `if translateRequest != nil`.
		sendBody, terr := cfg.BuildRequest(attemptBody)
		if terr != nil {
			if poolKey != nil {
				p.keyPool.RecordError(poolKey.ID)
			}
			lastErr = terr
			lastUsed = a
			continue
		}

		resp, rb, _, ferr := p.forward(ctx, r, sendBody, a.model, cfg)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}

		// Pool accounting: transport failure is the only signal the spec
		// asks us to count against a key. Upstream 5xx/429s are tracked
		// by the fallback router (which decides whether to switch
		// providers), not against the individual key.
		if poolKey != nil {
			if ferr != nil {
				p.keyPool.RecordError(poolKey.ID)
			} else {
				p.keyPool.RecordSuccess(poolKey.ID)
			}
		}

		if p.fallbackRouter != nil && p.fallbackRouter.ShouldFallback(status, ferr) {
			// Record-and-continue. Logging here is metadata only — no
			// prompt content, no response body.
			slog.Warn("fallback: attempt failed",
				slog.String("original_provider", provider),
				slog.String("attempt_provider", a.provider),
				slog.String("attempt_model", a.model),
				slog.Int("attempt_index", i),
				slog.Int("status", status),
				slog.String("err", errString(ferr)),
				slog.String("workspace_id", wsID),
			)
			lastBody = rb
			lastStatus = status
			lastErr = ferr
			lastUsed = a
			continue
		}

		// Non-fallbackable outcome (success, or 4xx that's the client's
		// fault either way). Reverse-translate Gemini responses so the
		// returned body is OpenAI-shaped for everyone downstream.
		if ferr == nil && status == http.StatusOK {
			// ParseResponse nil-guards the reverse-translator: identity for OpenAI-shaped providers,
			// gemini/bedrock → OpenAI shape otherwise — same result as the prior translateResponse branch.
			if translated, terr := cfg.ParseResponse(rb, a.model); terr == nil {
				rb = translated
			}
			// B17.217: and its answer comes back as an Anthropic message — an Anthropic client reads no `choices`.
			if crossWire {
				rb = openAIChatToAnthropic(rb, a.model)
			}
		}

		result := fallback.FallbackResult{
			UsedProvider: a.provider,
			UsedModel:    a.model,
			Attempts:     i + 1,
			FellBack:     i > 0,
		}
		if i > 0 {
			w.Header().Set("X-Talyvor-Fallback-Provider", a.provider)
			w.Header().Set("X-Talyvor-Fallback-Model", a.model)
			slog.Info("fallback: succeeded",
				slog.String("original_provider", provider),
				slog.String("used_provider", a.provider),
				slog.String("used_model", a.model),
				slog.Int("attempts", i+1),
				slog.String("workspace_id", wsID),
			)
		}
		return rb, status, result, ferr
	}

	// Chain exhausted — return whatever the last attempt produced. The
	// caller's serve() turns a non-nil err into a 502; otherwise the
	// upstream's own 5xx body is forwarded to the client.
	return lastBody, lastStatus, fallback.FallbackResult{
		UsedProvider: lastUsed.provider,
		UsedModel:    lastUsed.model,
		Attempts:     len(attempts),
		FellBack:     len(attempts) > 1,
	}, lastErr
}

// configForProvider returns a providerConfig built fresh per call. The
// closures capture the proxy's URL + key fields so test overrides of
// openAIURL/anthropicURL/googleURL propagate naturally.
func (p *Proxy) configForProvider(name string) providerConfig {
	return inference.ConfigFor(name, p.endpoints())
}

// Endpoints exposes the gateway's configured provider URLs + keys (by value) so the routing-prediction
// scorer's provider-backed Inferer can call the SAME providers with the SAME credentials the gateway uses
// — without importing the serve path. Returns a copy; mutating it does not affect the proxy.
func (p *Proxy) Endpoints() inference.Endpoints { return p.endpoints() }

// endpoints snapshots the proxy's configured provider URLs + keys into an inference.Endpoints VALUE so a
// built ProviderConfig captures them by-copy (the bedrock snapshot the by-value test pinned), never a
// pointer back into mutable proxy state.
func (p *Proxy) endpoints() inference.Endpoints {
	return inference.Endpoints{
		OpenAIURL:     p.openAIURL,
		OpenAIKey:     p.openAIKey,
		AnthropicURL:  p.anthropicURL,
		AnthropicKey:  p.anthropicKey,
		GoogleURL:     p.googleURL,
		GoogleKey:     p.googleKey,
		MistralURL:    p.mistralURL,
		MistralKey:    p.mistralKey,
		GroqURL:       p.groqURL,
		GroqKey:       p.groqKey,
		VLLMURL:       p.vllmURL,
		VLLMKey:       p.vllmKey,
		BedrockConfig: p.bedrockConfig,
		BedrockURL:    p.bedrockURL,
	}
}

// applyKey returns a providerConfig identical to cfg except that the
// auth and (for Google) URL closures use the supplied key instead of
// the Proxy's single configured value. Used when keyPool.Get returns a
// pooled credential; the original closures stay untouched in cfg's
// source so each call gets a fresh closure capturing the right key.
func (p *Proxy) applyKey(cfg providerConfig, key string) providerConfig {
	return inference.ConfigForKey(cfg.ProviderName(), p.endpoints(), key)
}

// setModelInBody re-emits the JSON body with the model field swapped.
// Parse errors leave the body untouched — forward() will simply send
// the original bytes upstream.
func setModelInBody(body []byte, model string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// SetReservation wires the reservation-billing path: enabled gates HOLD→SETTLE (vs the legacy estimate
// debit), maxOut supplies the bounded output allowance for the conservative hold when a request omits
// max_tokens. Read per-call so both stay live. nil-safe (reservationActive / the seam helpers check).
func (p *Proxy) SetReservation(enabled func() bool, maxOut func() int) {
	p.reservationEnabled = enabled
	p.reservationMaxOut = maxOut
}

// extractMaxTokens returns the request's declared output cap (max_tokens, or OpenAI's newer
// max_completion_tokens), else 0 when unset. Parse failure ⇒ 0 (the caller falls back to the configured cap).
func extractMaxTokens(body []byte) int {
	var m struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return 0
	}
	if m.MaxTokens != nil && *m.MaxTokens > 0 {
		return *m.MaxTokens
	}
	if m.MaxCompletionTokens != nil && *m.MaxCompletionTokens > 0 {
		return *m.MaxCompletionTokens
	}
	return 0
}

// boundedMaxOut is the output allowance for the conservative hold: the request's EXPLICIT max_tokens when
// set (the agent declared it — hold honestly for it), else a sane configured CAP — never the catalog maximum
// (which would over-hold and block legitimate work on a modest ceiling).
func boundedMaxOut(explicit int, cap func() int) int {
	if explicit > 0 {
		return explicit
	}
	if cap != nil {
		if c := cap(); c > 0 {
			return c
		}
	}
	return 4096 // last-resort default if the cap fn is unwired
}

// pricePooledServe prices a cache serve for the consumer, and does NOTHING ELSE — no settle, no
// I/O, no context. That purity is the point: it must run BEFORE the response body is written, which
// is the only moment the saving can still be put in a header (the serve precedes the settle so a
// billing failure can never affect an already-served response).
//
// A CROSS-TENANT POOLED hit is priced at avoided_COGS — what the live call would have cost — and
// then DISCOUNTED by r, so the consumer keeps part of a saving that costs Talyvor nothing to give:
// there was no provider call, so the whole charge was margin.
//
// An OWN-CACHE hit returns the zero value (Pooled false). It is FREE and stays free — no charge, no
// headers, nothing to discount. Charging someone for their own repeat would be pure extraction, and
// discounting a charge that does not exist is meaningless.
//
// A model the catalog does not know is priced at the provider's cheapest known rate and MARKED
// (PriceBasis), exactly as before: pricing it at 0 would bill the consumer nothing for value they
// received AND leave the contributor's royalty unfunded.
//
// B35.3 — WHAT THE LIVE CALL WOULD HAVE COST IS THE STORED ANSWER'S OWN USAGE, priced as the live seam
// prices a reply. It used to be len(served)/4 output tokens: the whole stored JSON reply, envelope and all,
// so a four-character answer asked with max_tokens 16 priced as about 124 output tokens, eight times its
// hold. The settle cut every such charge to the hold, silently, while the header promised the full price.
// Without a usage block it is still the prompt's and the reply's len/4. Either way it is never above what
// this request's hold priced (its prompt and maxOut, the output the hold allows), so the hold covers it.
func (p *Proxy) pricePooledServe(pooledHit *poolroyalty.ServedHit, prompt string, served []byte, maxOut int) pooledPrice {
	if pooledHit == nil {
		return pooledPrice{}
	}
	uncached, cached, written, out := len(prompt)/4, 0, 0, len(served)/4
	if u, ok := inference.ExtractUsage(pooledHit.Provider, served); ok {
		uncached, cached, written, out = u.UncachedInputTokens, u.CachedInputTokens, u.CacheWriteInputTokens, u.OutputTokens
	}
	avoided, prov := alerts.CostUSDResolved(pooledHit.Model, catalog.PurposeCharge, uncached, cached, written, out)
	if held, _ := alerts.CostUSDResolved(pooledHit.Model, catalog.PurposeCharge, len(prompt)/4, 0, 0, maxOut); avoided > held {
		avoided = held
	}
	basis := ""
	if prov == catalog.ProvenanceFallback {
		basis = prov.String()
	}
	pp := pricePooledHit(avoided, p.poolDiscount, basis)
	pp.modelForRow = pooledHit.Model
	return pp
}

// settlePooledServe resolves the held reservation at a CACHE SERVE point and RETURNS the USD the
// consumer was ACTUALLY charged.
//
// THE FUNDING TIE, UNCHANGED AND NOW STRICTLY TIGHTER: the caller feeds this return into
// mintPooledRoyalty as the royalty's funding basis, so the contributor is minted a share of what the
// consumer REALLY paid. Because the DISCOUNT is applied here — before the settle, not after — the
// royalty comes out of the discounted charge automatically, with no change to the mint at all. The
// invariant (#351/#353) maintains itself: royalty = s × charged and Talyvor keeps (1−s) × charged,
// both still bounded by the payment, so a discount can never mint against money nobody paid.
//
// Returns 0 with no reservation on the context (non-agent / plain key / reservation path off) — a
// plain-key cache hit is not metered and so funds NO royalty. An own-cache hit (price.Pooled false)
// RELEASES the hold in full: free, exactly as before.
//
// B26.9 — WITH RESERVATIONS OFF there is no hold: the agent was debited its pre-serve estimate, so this
// settles that debit to the serve's real price as B23.13 settles a model serve — an own-cache hit to
// nothing, a pooled hit to its discounted price, never past the owner's limit. B27.6: that settle reports
// the cash-backed part of the charge, and this returns it, so the mint is funded exactly as above.
//
// The LIST price and the rate ride onto the ledger row so the customer's evidence is the row itself.
// The SAVING is not passed — the row derives it from what was actually debited, because the settle
// clamps to the hold and a passed-in saving could then contradict the amount charged.
func (p *Proxy) settlePooledServe(ctx context.Context, wsID string, price pooledPrice) float64 {
	if !p.reservationActive() {
		cashBacked, debited := p.settleAgentDebitULXC(ctx, wsID, price.ChargedULXC, economy.AgentDebitMeta{ServedModel: price.modelForRow,
			PriceBasis: price.PriceBasis, PoolListULXC: price.ListULXC, PoolDiscountRate: price.Rate})
		if debited { // an own-cache hit settled to nothing, so it returns 0
			return float64(cashBacked) * economy.LXCUSDValue / 1e6
		}
	}
	if !price.Pooled {
		p.releaseReservation(ctx, "own cache hit")
		return 0
	}
	// B9.3 — the browser chat has no reservation; its pooled serve is charged here instead.
	if funded, chat := p.chargeChatPooled(ctx, price); chat {
		return funded
	}
	chargedUSD := float64(price.ChargedULXC) * economy.LXCUSDValue / 1e6
	return p.settleReservationPooled(ctx, chargedUSD, price)
}
