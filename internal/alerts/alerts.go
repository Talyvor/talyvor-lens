package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/metrics"
)

type AlertLevel string

const (
	AlertWarning  AlertLevel = "warning"
	AlertCritical AlertLevel = "critical"
)

type Alert struct {
	Level     AlertLevel `json:"level"`
	Feature   string     `json:"feature"`
	Team      string     `json:"team"`
	Message   string     `json:"message"`
	SpendUSD  float64    `json:"spend_usd"`
	Threshold float64    `json:"threshold_usd"`
	CreatedAt time.Time  `json:"created_at"`
	// WorkspaceID is the workspace whose request tripped the threshold (SEC-7).
	// Carried so the Track emitter can attribute the alert to the right workspace
	// (Track credits issues by workspace_id + feature). Additive — the NATS payload
	// gains a field, which existing consumers ignore. Empty for a monitor-triggered
	// alert (aggregate, no single workspace).
	WorkspaceID string `json:"workspace_id"`
}

type CircuitState string

const (
	CircuitClosed CircuitState = "closed"
	CircuitOpen   CircuitState = "open"
)

type SpendRule struct {
	ID          string
	Team        string
	Feature     string
	WindowHours int
	WarningUSD  float64
	CriticalUSD float64
	CircuitUSD  float64
}

// pgxDB is the subset of *pgxpool.Pool that AlertManager needs. Defined so
// tests can substitute pgxmock without a real database.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type AlertManager struct {
	pool     pgxDB
	nc       *nats.Conn
	rules    []SpendRule
	circuits map[circuitID]CircuitState
	mu       sync.RWMutex
	emitter  *Emitter // SEC-7: outbound Track spend-alert webhook (nil = disabled no-op)
}

// SetEmitter wires the Track spend-alert webhook emitter (SEC-7). Optional — a nil
// emitter (or one with no URL/secret) is a total no-op. The emit is async and can
// NEVER block a serve (see Emitter's security invariant).
func (a *AlertManager) SetEmitter(e *Emitter) { a.emitter = e }

func New(pool *pgxpool.Pool, nc *nats.Conn, rules []SpendRule) *AlertManager {
	return newAlertManager(pool, nc, rules)
}

func newAlertManager(pool pgxDB, nc *nats.Conn, rules []SpendRule) *AlertManager {
	return &AlertManager{
		pool:     pool,
		nc:       nc,
		rules:    rules,
		circuits: make(map[circuitID]CircuitState),
	}
}

// costUSD returns the realized USD cost for the request, pricing the model
// from the catalog (the single source of truth — Upgrade 16). Unknown models
// cost 0 — we'd rather miss an alert than fire a false one off bad data.
func costUSD(model string, inputTokens, outputTokens int) float64 {
	inP, _, _, outP, ok := catalog.PriceDetailedAt(model, inputTokens)
	if !ok {
		return 0
	}
	return (float64(inputTokens)*inP + float64(outputTokens)*outP) / 1_000_000
}

// CostUSD is the exported entry point on the per-million-token price table.
// Other packages (e.g. the A/B tester) call this so there's a single source
// of truth for model pricing across the system.
//
// This prices EVERY input token at the full input rate — it is blind to prompt
// caching. That over-states cost when input was served from a provider cache
// (OpenAI cached tokens are ~0.5x, Anthropic cache reads 0.1x) and misses
// Anthropic cache-write tokens entirely. It is retained BYTE-IDENTICAL because
// the in-flight billing arc and ~20 other call sites depend on this exact
// signature and result; callers move to CostUSDDetailed to get the honest basis.
func CostUSD(model string, inputTokens, outputTokens int) float64 {
	return costUSD(model, inputTokens, outputTokens)
}

// CostUSDDetailed is the cache-aware cost basis: the honest USD Talyvor pays a
// provider for one response. It prices each class of input token at its own
// catalog rate —
//
//	uncachedInput   at the input rate
//	cachedInput     at the cache-READ rate  (cheap: Anthropic 0.1x, OpenAI ~0.5x)
//	cacheWriteInput at the cache-WRITE rate (Anthropic 1.25x; none pre-GPT-5.6)
//	output          at the output rate
//
// It is the ADDITIVE counterpart to CostUSD: with zero cached and zero cache-write
// tokens it returns EXACTLY CostUSD(model, uncachedInput, output) — the input/output
// rates are the same catalog values, so the extra terms vanish. That equivalence is
// proven by TestCostUSDDetailed_EqualsCostUSDAtZeroCached, which is why adding this
// function cannot perturb any existing CostUSD caller. Unknown models cost 0, as CostUSD.
//
// Wire the counts from inference.Usage's normalized breakdown: UncachedInputTokens,
// CachedInputTokens, CacheWriteInputTokens, OutputTokens.
func CostUSDDetailed(model string, uncachedInput, cachedInput, cacheWriteInput, output int) float64 {
	inP, cachedP, writeP, outP, ok := catalog.PriceDetailedAt(model, uncachedInput+cachedInput+cacheWriteInput)
	if !ok {
		return 0
	}
	return (float64(uncachedInput)*inP +
		float64(cachedInput)*cachedP +
		float64(cacheWriteInput)*writeP +
		float64(output)*outP) / 1_000_000
}

// providerForModel derives the provider name from the model name. Used so
// the (provider, model) pair stays consistent in token_events even when the
// caller only supplies the model.
func providerForModel(model string) string {
	// Catalog is authoritative for known models (Upgrade 16) — this also
	// resolves dated-snapshot aliases to their canonical model's provider.
	if m, ok := catalog.Get(model); ok {
		return m.Provider
	}
	// Unknown to the catalog — derive the provider from the name prefix,
	// exactly as before. Bedrock model IDs ("anthropic.claude-…") are checked
	// before the generic claude- branch so a Bedrock-billed row never gets
	// logged as a direct Anthropic call.
	switch {
	case strings.HasPrefix(model, "anthropic."):
		return "bedrock"
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gemini-"):
		return "google"
	// vLLM is self-hosted; the user-facing model name is whatever the
	// operator loaded, namespaced with "vllm/" to keep cache keys disjoint.
	case strings.HasPrefix(model, "vllm/"):
		return "vllm"
	case strings.HasPrefix(model, "mistral-"), strings.HasPrefix(model, "open-mistral-"):
		return "mistral"
	case strings.HasPrefix(model, "llama-"), strings.HasPrefix(model, "mixtral-"), strings.HasPrefix(model, "gemma"):
		return "groq"
	}
	return "unknown"
}

const insertTokenEventSQL = `INSERT INTO token_events
  (workspace_id, provider, model, input_tokens, output_tokens, team, sprint_id, feature, cost_usd, prompt_text, session_id, request_id, modality, cost_estimated, distill_method,
   tare_kind, tare_tokens_in, tare_tokens_out, tare_delta_cost_usd, tare_work_item_id, auth_method,
   requested_model, list_cost_usd, charged_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`

// authMethodOf is the credential kind that made this request, recorded on every token_events row
// (B9.3, migration 0129) so a serve is never again of unknown origin. ” when no credential reached ctx.
func authMethodOf(ctx context.Context) string {
	if a := auth.GetAuthContext(ctx); a != nil {
		return a.AuthMethod
	}
	if auth.GetAPIKey(ctx) != nil {
		return auth.MethodWorkspaceKey
	}
	return ""
}

// RecordSpend takes a `prompt` (or already-redacted equivalent) so the
// cache warmer can later JOIN prompt_embeddings against token_events to
// recover the prompt text it needs to re-warm popular patterns. The
// session_id and request_id arguments are persisted so audit exports
// can correlate spend rows back to a chat session and HTTP request.
//
// workspaceID and sprintID land in token_events alongside the existing
// team so spend can be summed per workspace / team / sprint from this
// single billing write. CORRECTNESS NOTE: workspaceID was previously not
// persisted at all (the column fell back to its 'default'), so the
// per-workspace spend cap was summing globally; passing it here makes that
// cap — and the new workspace-scoped budgets — truly per-workspace. See
// migration 0028.
//
// modality + estimated land on this same single write (migration 0029) so
// budgets/forecast/anomaly/ROI can reflect image cost. modality is the
// canonical label ("text" / "image" / "image,audio" …); estimated is true
// when the cost is a documented estimate rather than exact accounting
// (multimodal input tokens are estimated).
func (a *AlertManager) RecordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool) error {
	// Non-distilled traffic: distill_method is '' (the column default). Existing
	// callers are unchanged.
	return a.recordSpend(ctx, workspaceID, team, sprint, feature, model, inputTokens, outputTokens, prompt, sessionID, requestID, modality, estimated, "", TareMeter{})
}

// RecordSpendWithDistill is RecordSpend plus the DISTILL method attribution
// written to token_events.distill_method: "convert" for a distilled request's
// (lower-count) spend row, or "vision_ocr" for the OCR sub-call's OWN cost row.
// Additive — it shares the exact billing/alert path; only the distill_method tag
// differs, so non-distilled traffic and all existing callers are untouched.
func (a *AlertManager) RecordSpendWithDistill(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string) error {
	return a.recordSpend(ctx, workspaceID, team, sprint, feature, model, inputTokens, outputTokens, prompt, sessionID, requestID, modality, estimated, distillMethod, TareMeter{})
}

// RecordSpendWithTare is RecordSpendWithDistill plus the Tare metering record (B6.4), written to
// the tare_* columns of the SAME token_events row. See tare_meter.go.
func (a *AlertManager) RecordSpendWithTare(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string, tare TareMeter) error {
	return a.recordSpend(ctx, workspaceID, team, sprint, feature, model, inputTokens, outputTokens, prompt, sessionID, requestID, modality, estimated, distillMethod, tare)
}

// RecordCacheServe writes the token_events row for a CACHE-SERVED request — the row that makes
// the cache hit rate (the trial's core number) countable from the same table every other spend
// reader uses. serveSource is the cache layer that produced the bytes (the metric label verbatim:
// cache_hit_exact / cache_hit_semantic / cache_hit_pooled / cache_hit_pooled_semantic; the 0100
// CHECK constraint enforces the vocabulary).
//
// ⚠ cost_usd = 0 IS TALYVOR'S PROVIDER COST — nothing was bought upstream. It is NOT what the
// requester paid: the agent-allocator's pre-serve LXC estimate debit (lxc_ledger) stands and is
// deliberately NOT refunded. A spend view must never render this row as "the request was free".
//
// No prompt text is persisted (the streaming spend row precedent — metadata-equivalent), the cost
// is always flagged estimated (token counts are length-derived, no provider usage exists), and the
// alert-rule ladder is skipped: rules threshold on SUM(cost_usd) windows, which a zero-cost row
// cannot move.
func (a *AlertManager) RecordCacheServe(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, sessionID, requestID, modality, serveSource string) error {
	provider := providerForModel(model)
	if modality == "" {
		modality = "text"
	}
	requested, list, charged := savingOf(ctx)
	if _, err := a.pool.Exec(ctx, insertCacheServeSQL,
		workspaceID, provider, model, inputTokens, outputTokens, team, sprint, feature, sessionID, requestID, modality, serveSource, authMethodOf(ctx),
		requested, list, charged,
	); err != nil {
		return fmt.Errorf("alerts: insert cache-serve token_event: %w", err)
	}
	// No rule evaluation: the alert ladder thresholds on SUM(cost_usd) windows,
	// which a zero-cost row cannot move — skipping avoids two pointless queries
	// on every cache hit.
	return nil
}

// insertCacheServeSQL owns the cache row's money semantics IN THE SQL so they are unmistakable:
// cost_usd = 0 (Talyvor's provider cost — the requester's LXC debit lives in lxc_ledger),
// prompt_text = ” (no prompt persisted — the streaming spend row precedent), cost_estimated =
// TRUE (length-derived tokens), distill_method = ” (a cache serve distills nothing). Column
// order mirrors insertTokenEventSQL with serve_source appended.
const insertCacheServeSQL = `INSERT INTO token_events
  (workspace_id, provider, model, input_tokens, output_tokens, team, sprint_id, feature, cost_usd, prompt_text, session_id, request_id, modality, cost_estimated, distill_method, serve_source, auth_method,
   requested_model, list_cost_usd, charged_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, '', $9, $10, $11, TRUE, '', $12, $13, $14, $15, $16)`

// RecordNodeServe writes the token_events row for a request served by a REGISTERED INFERENCE NODE —
// so it COUNTS in the cache hit-rate denominator as a MISS (no cache produced the bytes), closing the
// node-serve denominator gap. serve_source is always 'node' (the migration 0101 CHECK enum).
//
// B27.2: cost_usd is what the question was CHARGED (costUSD — the served model's charge rate on the
// Lens-measured tokens, the same figure the chat/subscriber charge and the agent settle book), because
// budgets and the spending cap both read SUM(cost_usd) over token_events: at 0 a node-served question
// was free to every budget the moment it next reconciled. It is still not a LENS figure — what the node
// may be OWED is a PoVI mint in lens_token_ledger, a different ledger and unit. The alert ladder is not
// run here; the next rule evaluation's SUM(cost_usd) window includes this row.
func (a *AlertManager) RecordNodeServe(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, sessionID, requestID, modality string, costUSD float64) error {
	provider := providerForModel(model)
	if modality == "" {
		modality = "text"
	}
	requested, list, charged := savingOf(ctx)
	if _, err := a.pool.Exec(ctx, insertNodeServeSQL,
		workspaceID, provider, model, inputTokens, outputTokens, team, sprint, feature, costUSD, sessionID, requestID, modality, authMethodOf(ctx),
		requested, list, charged,
	); err != nil {
		return fmt.Errorf("alerts: insert node-serve token_event: %w", err)
	}
	return nil
}

// insertNodeServeSQL is insertCacheServeSQL with the charge in cost_usd: no prompt persisted, length-
// derived tokens (cost_estimated TRUE), serve_source 'node'.
const insertNodeServeSQL = `INSERT INTO token_events
  (workspace_id, provider, model, input_tokens, output_tokens, team, sprint_id, feature, cost_usd, prompt_text, session_id, request_id, modality, cost_estimated, distill_method, serve_source, auth_method,
   requested_model, list_cost_usd, charged_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '', $10, $11, $12, TRUE, '', 'node', $13, $14, $15, $16)`

func (a *AlertManager) recordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string, tare TareMeter) error {
	cost := costUSD(model, inputTokens, outputTokens)
	provider := providerForModel(model)
	if modality == "" {
		modality = "text"
	}
	requested, list, charged := savingOf(ctx)

	if _, err := a.pool.Exec(ctx, insertTokenEventSQL,
		workspaceID, provider, model, inputTokens, outputTokens, team, sprint, feature, cost, prompt, sessionID, requestID, modality, estimated, distillMethod,
		tare.Kind, tare.TokensIn, tare.TokensOut, tare.deltaCostUSD(model), tare.WorkItemID, authMethodOf(ctx),
		requested, list, charged,
	); err != nil {
		return fmt.Errorf("alerts: insert token_event: %w", err)
	}

	for _, rule := range a.rules {
		if rule.Team != team || rule.Feature != feature {
			continue
		}
		spend, err := a.windowSpend(ctx, rule, workspaceID)
		if err != nil {
			slog.Warn("alerts: window spend query failed",
				slog.String("team", team),
				slog.String("feature", feature),
				slog.String("err", err.Error()),
			)
			continue
		}
		a.evaluateRule(ctx, rule, spend, workspaceID)
	}
	return nil
}

// windowSpend is one workspace's spend under the rule's team and feature labels: the labels are
// free-form, so another workspace sending the same ones must never count towards this one (B42.10).
func (a *AlertManager) windowSpend(ctx context.Context, rule SpendRule, workspaceID string) (float64, error) {
	// INTERVAL is parameterized only via a literal hours value because pgx
	// can't bind a SQL interval cleanly — the rule's WindowHours comes from
	// our own config, not user input, so interpolation here is safe.
	q := fmt.Sprintf(`SELECT COALESCE(SUM(cost_usd), 0)
FROM token_events
WHERE workspace_id = $1 AND team = $2 AND feature = $3
  AND created_at > NOW() - INTERVAL '%d hours'`, rule.WindowHours)
	var spend float64
	if err := a.pool.QueryRow(ctx, q, workspaceID, rule.Team, rule.Feature).Scan(&spend); err != nil {
		return 0, err
	}
	return spend, nil
}

// windowSpendByWorkspace is windowSpend for every workspace that spent under the rule's labels.
func (a *AlertManager) windowSpendByWorkspace(ctx context.Context, rule SpendRule) (map[string]float64, error) {
	q := fmt.Sprintf(`SELECT workspace_id, COALESCE(SUM(cost_usd), 0)
FROM token_events
WHERE team = $1 AND feature = $2
  AND created_at > NOW() - INTERVAL '%d hours'
GROUP BY workspace_id`, rule.WindowHours)
	rows, err := a.pool.Query(ctx, q, rule.Team, rule.Feature)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var ws string
		var spend float64
		if err := rows.Scan(&ws, &spend); err != nil {
			return nil, err
		}
		out[ws] = spend
	}
	return out, rows.Err()
}

// evaluateRule fires alerts and toggles the circuit breaker based on the
// rule's tier thresholds. Each tier fires independently — warning is still
// emitted when critical is also true so dashboards see the full ladder.
func (a *AlertManager) evaluateRule(ctx context.Context, rule SpendRule, spend float64, workspaceID string) {
	key := circuitID{workspaceID, rule.Team, rule.Feature}

	if spend > rule.CircuitUSD {
		a.mu.Lock()
		prev := a.circuits[key]
		a.circuits[key] = CircuitOpen
		a.mu.Unlock()
		if prev != CircuitOpen {
			slog.Warn("alerts: circuit opened",
				slog.String("workspace_id", workspaceID),
				slog.String("team", rule.Team),
				slog.String("feature", rule.Feature),
				slog.Float64("spend_usd", spend),
				slog.Float64("threshold_usd", rule.CircuitUSD),
			)
		}
	}

	if spend > rule.CriticalUSD {
		_ = a.fireAlert(ctx, Alert{
			Level:       AlertCritical,
			Team:        rule.Team,
			Feature:     rule.Feature,
			Message:     "Critical spend threshold exceeded",
			SpendUSD:    spend,
			Threshold:   rule.CriticalUSD,
			CreatedAt:   time.Now().UTC(),
			WorkspaceID: workspaceID,
		})
	}
	if spend > rule.WarningUSD {
		_ = a.fireAlert(ctx, Alert{
			Level:       AlertWarning,
			Team:        rule.Team,
			Feature:     rule.Feature,
			Message:     "Warning spend threshold exceeded",
			SpendUSD:    spend,
			Threshold:   rule.WarningUSD,
			CreatedAt:   time.Now().UTC(),
			WorkspaceID: workspaceID,
		})
	}
}

func (a *AlertManager) fireAlert(_ context.Context, alert Alert) error {
	data, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	// SEC-7: emit to Track's webhook — async, fire-and-forget, NEVER blocks (see
	// Emitter). Independent of and BEFORE the NATS publish, so a spend alert reaches
	// Track even when NATS is down; a nil/disabled emitter is a no-op.
	a.emitter.Emit(alert)

	if a.nc == nil {
		return errors.New("alerts: no NATS connection")
	}
	// Publish to the level-specific subject and the all-subject for dashboards.
	if err := a.nc.Publish("lens.alerts."+string(alert.Level), data); err != nil {
		slog.Warn("alerts: publish failed", slog.String("err", err.Error()))
	}
	if err := a.nc.Publish("lens.alerts.all", data); err != nil {
		slog.Warn("alerts: publish-all failed", slog.String("err", err.Error()))
	}

	switch alert.Level {
	case AlertCritical:
		slog.Error("alerts: critical spend",
			slog.String("team", alert.Team),
			slog.String("feature", alert.Feature),
			slog.Float64("spend_usd", alert.SpendUSD),
			slog.Float64("threshold_usd", alert.Threshold),
		)
	default:
		slog.Warn("alerts: warning spend",
			slog.String("team", alert.Team),
			slog.String("feature", alert.Feature),
			slog.Float64("spend_usd", alert.SpendUSD),
			slog.Float64("threshold_usd", alert.Threshold),
		)
	}
	return nil
}

func (a *AlertManager) IsCircuitOpen(workspaceID, team, feature string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.circuits[circuitID{workspaceID, team, feature}] == CircuitOpen
}

func (a *AlertManager) GetDowngradeModel(provider, _ string) string {
	switch provider {
	case "anthropic":
		return "claude-haiku-4-5"
	default:
		return "gpt-4o-mini"
	}
}

func (a *AlertManager) ResetCircuit(workspaceID, team, feature string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.circuits, circuitID{workspaceID, team, feature})
}

// openCircuitForTest is an unexported helper so tests can put a circuit in
// the open state without setting up the full RecordSpend pipeline.
func (a *AlertManager) openCircuitForTest(workspaceID, team, feature string) {
	a.mu.Lock()
	a.circuits[circuitID{workspaceID, team, feature}] = CircuitOpen
	a.mu.Unlock()
}

// OpenCircuit lets callers (admin endpoints, ops tooling) trip a circuit
// breaker without waiting for spend to cross the threshold.
func (a *AlertManager) OpenCircuit(workspaceID, team, feature string) {
	a.mu.Lock()
	a.circuits[circuitID{workspaceID, team, feature}] = CircuitOpen
	a.mu.Unlock()
}

// CircuitStates returns a copy of the current circuit-breaker states,
// keyed by "workspace:team:feature". Each value is the string form of CircuitState.
func (a *AlertManager) CircuitStates() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]string, len(a.circuits))
	for k, v := range a.circuits {
		out[k.workspace+":"+k.team+":"+k.feature] = string(v)
	}
	return out
}

// WorkspaceCircuitStates is CircuitStates for one workspace's circuits, keyed by "team:feature".
func (a *AlertManager) WorkspaceCircuitStates(workspaceID string) map[string]string {
	out := map[string]string{}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for k, v := range a.circuits {
		if k.workspace == workspaceID {
			out[k.team+":"+k.feature] = string(v)
		}
	}
	return out
}

// Rules returns a copy of the configured spend rules. Rules are
// effectively immutable after construction so we don't need a lock here.
func (a *AlertManager) Rules() []SpendRule {
	out := make([]SpendRule, len(a.rules))
	copy(out, a.rules)
	return out
}

// circuitID is one workspace's circuit for a team and feature: the labels are the caller's own, so
// the same labels from another workspace are a different circuit (B42.10).
type circuitID struct{ workspace, team, feature string }

const monitorInterval = 5 * time.Minute

// StartMonitor spawns a background goroutine that re-evaluates each rule
// every 5 minutes and closes circuits where the rolling-window spend has
// dropped below CircuitUSD. Exits when ctx is cancelled.
func (a *AlertManager) StartMonitor(ctx context.Context) {
	go a.monitorLoop(ctx, monitorInterval)
}

func (a *AlertManager) monitorLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.evaluateAllRules(ctx)
		}
	}
}

func (a *AlertManager) evaluateAllRules(ctx context.Context) {
	for _, rule := range a.rules {
		spends, err := a.windowSpendByWorkspace(ctx, rule)
		if err != nil {
			slog.Warn("alerts: monitor window spend failed",
				slog.String("rule_id", rule.ID),
				slog.String("err", err.Error()),
			)
			continue
		}

		a.mu.Lock()
		// An open circuit whose workspace spent nothing in the window is absent from spends: close it too.
		for key, state := range a.circuits {
			if state == CircuitOpen && key.team == rule.Team && key.feature == rule.Feature && spends[key.workspace] <= rule.CircuitUSD {
				delete(a.circuits, key)
				slog.Info("alerts: circuit closed by monitor",
					slog.String("workspace_id", key.workspace),
					slog.String("team", rule.Team),
					slog.String("feature", rule.Feature),
					slog.Float64("spend_usd", spends[key.workspace]),
				)
			}
		}
		for ws, spend := range spends {
			key := circuitID{ws, rule.Team, rule.Feature}
			if a.circuits[key] != CircuitOpen && spend > rule.CircuitUSD {
				a.circuits[key] = CircuitOpen
				slog.Warn("alerts: circuit opened by monitor",
					slog.String("workspace_id", ws),
					slog.String("team", rule.Team),
					slog.String("feature", rule.Feature),
					slog.Float64("spend_usd", spend),
				)
			}
		}
		a.mu.Unlock()
	}
}

// ─── MONEY-PATH PRICING (never zero) ─────────────────────────────────────────

// CostUSDResolved is the pricing entry point for anything that MOVES MONEY — the reservation hold and
// the delivered charge. It differs from CostUSD/CostUSDDetailed in the one way that matters: it cannot
// return zero for an unknown model, because catalog.ResolveRates has no unknown outcome. It returns the
// cost together with the PROVENANCE, so a fallback charge can be marked on its ledger row, alerted on,
// and repriced later.
//
// ⚠ DO NOT use CostUSD or CostUSDDetailed to price a charge or a hold. Their contract is "unknown
// models cost 0", which was written for alerting — where a missed alert beats a false one — and is a
// revenue hole when the same function prices a bill. money_path_pricing_guard_test.go enforces this.
//
// purpose selects the fallback direction for an unknown model: a hold falls back HIGH (refundable), a
// charge falls back LOW (a defensible floor). See catalog.Purpose.
func CostUSDResolved(model string, purpose catalog.Purpose,
	uncachedInput, cachedInput, cacheWriteInput, output int) (float64, catalog.Provenance) {
	rates, prov := catalog.ResolveRatesAt(model, purpose, uncachedInput+cachedInput+cacheWriteInput)
	usd := (float64(uncachedInput)*rates.InputPer1M +
		float64(cachedInput)*rates.CachedInputPer1M +
		float64(cacheWriteInput)*rates.CacheWritePer1M +
		float64(output)*rates.OutputPer1M) / 1_000_000
	if prov == catalog.ProvenanceFallback {
		WarnUnpricedModel(model, purpose, usd)
	}
	return usd, prov
}

// WarnUnpricedModel is the LOUD part, and the reason this bug survived: a log line nobody reads is how
// a revenue hole stays open. Every unknown-model pricing decision emits an ERROR (not Info, not Debug —
// an operator's alerting almost always ships on ERROR) naming the model, the purpose and the derived
// amount, and increments a metric so it is graphable and alertable without log scraping.
//
// The message states the ACTION, because the operator needs to know the fix is one env var away:
// LENS_MODEL_CATALOG_OVERRIDES extends the catalog on the box without a rebuild.
func WarnUnpricedModel(model string, purpose catalog.Purpose, derivedUSD float64) {
	metrics.UnpricedModelRequests.WithLabelValues(model, purposeLabel(purpose)).Inc()
	slog.Error("PRICING: model is NOT in the catalog — billed on a DERIVED FALLBACK rate, not its real price",
		slog.String("model", model),
		slog.String("purpose", purposeLabel(purpose)),
		slog.Float64("derived_usd", derivedUSD),
		slog.String("impact", "a charge on this basis is a guess: a hold falls back to the provider's most expensive known model, a charge to its cheapest (a floor, never an over-bill)"),
		slog.String("action", "add the model to the catalog NOW via LENS_MODEL_CATALOG_OVERRIDES (no rebuild, no restart of anything else) — until then this workspace is under-billed"),
	)
}

func purposeLabel(p catalog.Purpose) string {
	if p == catalog.PurposeHold {
		return "hold"
	}
	return "charge"
}
