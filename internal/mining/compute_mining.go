package mining

// compute_mining.go — LENS-earning track for workspaces that
// volunteer GPU capacity. Sister file to cache_mining.go; both
// share LedgerStore.
//
// Lifecycle:
//   1. Operator POSTs an InferenceNode to /v1/workspaces/:wsID/nodes.
//      RegisterNode inserts a row with verified=false and kicks
//      off a background probe.
//   2. The probe hits the provider's models / tags endpoint;
//      on success it flips verified=true. Unverified nodes are
//      excluded from ListAvailableNodes.
//   3. When the local router serves a request through this node
//      for a *different* workspace, the proxy calls
//      RecordServedRequest with the actual server-side token
//      count. The miner credits the owner workspace and updates
//      node_metrics.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/safehttp"
)

// ─── constants ───────────────────────────────────

// ComputeMineBaseRate is the LENS earned for serving 1000 tokens
// on a baseline (RTX 4090-class) GPU. Scaled by GPUMultiplier
// to reflect that an H100 produces tokens many times faster
// than a CPU.
const ComputeMineBaseRate int64 = 50_000 // 0.050 LENS in µLENS (SEC-2)

// GPU multipliers. Keep them in sync with knownGPUTypes — the
// allowlist is the source of truth for what RegisterNode
// accepts.
const (
	GPUMultiplierCPU     = 0.5
	GPUMultiplierRTX4090 = 1.0
	GPUMultiplierA100    = 2.0
	GPUMultiplierH100    = 3.0
)

// knownGPUTypes is the closed set of acceptable values for the
// gpu_type column. Anything outside this set is rejected at
// RegisterNode time.
var knownGPUTypes = map[string]float64{
	"cpu":     GPUMultiplierCPU,
	"rtx4090": GPUMultiplierRTX4090,
	"a100":    GPUMultiplierA100,
	"h100":    GPUMultiplierH100,
}

// TypeComputeMine is the ledger row type for earnings from this
// track. Same shape as TypeCacheMine.
const TypeComputeMine = "compute_mine"

// ─── types ───────────────────────────────────────

// InferenceNode mirrors one row of inference_nodes.
type InferenceNode struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspace_id"`
	URL           string    `json:"url"`
	Provider      string    `json:"provider"`
	Models        []string  `json:"models"`
	GPUType       string    `json:"gpu_type"`
	MaxConcurrent int       `json:"max_concurrent"`
	PricePerToken float64   `json:"price_per_token"`
	Active        bool      `json:"active"`
	Verified      bool      `json:"verified"`
	CreatedAt     time.Time `json:"created_at"`
	// Ed25519PubKey is the node's registered ed25519 public key (base64) for
	// PoVI receipt verification (Token Economy Phase 1, Part 1). Optional —
	// nodes from before PoVI have none, and a node without a pubkey simply
	// can't have its receipts verified.
	Ed25519PubKey string `json:"ed25519_pubkey,omitempty"`
}

// NodeMetrics mirrors one row of node_metrics.
type NodeMetrics struct {
	NodeID         string    `json:"node_id"`
	RequestsServed int       `json:"requests_served"`
	TokensServed   int64     `json:"tokens_served"`
	AvgLatencyMs   int64     `json:"avg_latency_ms"`
	ErrorRate      float64   `json:"error_rate"`
	UptimePct      float64   `json:"uptime_pct"`
	LastActiveAt   time.Time `json:"last_active_at"`
}

// ComputeMiningStats is the response shape for the
// /v1/workspaces/:wsID/tokens/mining/compute endpoint.
type ComputeMiningStats struct {
	WorkspaceID       string  `json:"workspace_id"`
	NodesActive       int     `json:"nodes_active"`
	TokensServedTotal int64   `json:"tokens_served_total"`
	EarnedTotal       float64 `json:"earned_total"`
}

// ─── errors ──────────────────────────────────────

var (
	ErrInvalidNodeURL = errors.New("compute mining: node URL must be http:// or https:// with a host")
	ErrInvalidGPUType = errors.New("compute mining: unknown gpu_type (allowed: cpu, rtx4090, a100, h100)")
	ErrNodeNotFound   = errors.New("compute mining: node not found")
)

// ─── EarningRate ─────────────────────────────────

// GPUMultiplier returns the LENS multiplier for the gpu type.
// Unknown types fall back to the CPU multiplier so a stray
// value never produces a giant payout.
func GPUMultiplier(gpuType string) float64 {
	if m, ok := knownGPUTypes[strings.ToLower(gpuType)]; ok {
		return m
	}
	return GPUMultiplierCPU
}

// EarningRate is the µLENS payout for serving `tokens` tokens on a node of the
// given GPU type (SEC-2: integer smallest-unit). The Tier-2 GPU multiplier and
// tokens/1000 factor scale the integer base rate; the result is floored
// (MulFloor) because a mint rounds DOWN — the dropped sub-µLENS remainder is not
// minted (retained by the protocol). Integers make this exact; the old
// roundTo(_,6) IEEE-754 band-aid is gone.
func EarningRate(gpuType string, tokens int) int64 {
	if tokens <= 0 {
		return 0
	}
	return MulFloor(ComputeMineBaseRate, GPUMultiplier(gpuType)*(float64(tokens)/1000.0))
}

// ─── ComputeMiner ────────────────────────────────

// ComputeMiner is the persistence + earning engine for compute
// mining. Holds an HTTP client used for the async verification
// probe and a reference to the shared ledger.
type ComputeMiner struct {
	ledger         *LedgerStore
	pool           pgxDB
	httpClient     *http.Client
	holdbackWindow time.Duration // Phase-3 Item 1: the mint lands HELD, finalizes after this (default 72h)
}

func NewComputeMiner(ledger *LedgerStore, pool pgxDB) *ComputeMiner {
	return &ComputeMiner{
		ledger:         ledger,
		pool:           pool,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		holdbackWindow: 72 * time.Hour, // Phase-3 Item 1: default held window (mirrors cache/pool-royalty)
	}
}

// SetHoldbackWindow sets the held→spendable delay for compute mints (Phase-3
// Item 1). Non-positive keeps the 72h default. The mint lands HELD and the
// traffic finalize sweeper settles it after this window (revocable before then).
func (m *ComputeMiner) SetHoldbackWindow(d time.Duration) {
	if d > 0 {
		m.holdbackWindow = d
	}
}

// SetHTTPClient lets tests inject an httptest.Server-backed client
// for the async verification probe.
func (m *ComputeMiner) SetHTTPClient(c *http.Client) { m.httpClient = c }

// ─── RegisterNode ────────────────────────────────

// RegisterNode validates the inputs, inserts the row with
// verified=false, and (when pool + http client are wired) spawns
// a background goroutine that probes the node and flips
// verified=true on success.
//
// Returns the InferenceNode that ended up in the DB (with its
// generated ID) so the caller can echo it to the API client.
func (m *ComputeMiner) RegisterNode(ctx context.Context, in InferenceNode) (*InferenceNode, error) {
	if err := validateNodeURL(in.URL); err != nil {
		return nil, err
	}
	if _, ok := knownGPUTypes[strings.ToLower(in.GPUType)]; !ok {
		return nil, ErrInvalidGPUType
	}
	if in.Provider == "" {
		return nil, errors.New("compute mining: provider required")
	}
	if in.WorkspaceID == "" {
		return nil, errors.New("compute mining: workspace_id required")
	}
	if in.MaxConcurrent <= 0 {
		in.MaxConcurrent = 1
	}
	if in.PricePerToken <= 0 {
		// PricePerToken is a Tier-2 float LENS rate (inference_nodes.price_per_token
		// stays DOUBLE); convert the µLENS base rate back to whole-LENS for it.
		in.PricePerToken = MicroToFloat(ComputeMineBaseRate)
	}
	in.GPUType = strings.ToLower(in.GPUType)
	in.URL = strings.TrimRight(in.URL, "/")
	in.Active = true
	in.Verified = false

	if m.pool == nil {
		// Test path — synthesise an ID + timestamp so the caller
		// gets back a usable struct.
		in.ID = fmt.Sprintf("node_%d", time.Now().UnixNano())
		in.CreatedAt = time.Now().UTC()
		return &in, nil
	}

	// Both INSERTs run in one transaction: if metrics seeding fails
	// the node row is rolled back so RecordServedRequest never
	// silently no-ops against a metrics-less node.
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("compute mining: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO inference_nodes
			(workspace_id, url, provider, models, gpu_type, max_concurrent, price_per_token, ed25519_pubkey)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
		RETURNING id, created_at`,
		in.WorkspaceID, in.URL, in.Provider, in.Models,
		in.GPUType, in.MaxConcurrent, in.PricePerToken, in.Ed25519PubKey,
	)
	if err := row.Scan(&in.ID, &in.CreatedAt); err != nil {
		return nil, fmt.Errorf("compute mining: insert node: %w", err)
	}
	// Seed the metrics row so RecordServedRequest can UPDATE it.
	if _, err := tx.Exec(ctx,
		`INSERT INTO node_metrics (node_id) VALUES ($1) ON CONFLICT (node_id) DO NOTHING`,
		in.ID); err != nil {
		return nil, fmt.Errorf("compute mining: seed metrics: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("compute mining: commit node registration: %w", err)
	}

	// Async verification — don't block the registration response.
	go m.verifyNodeAsync(in.ID, in.URL, in.Provider)
	return &in, nil
}

// NodePubKey returns a node's registered ed25519 public key (base64) for PoVI
// receipt verification. Errors when the node is unknown or has no pubkey on
// file (a receipt from such a node simply can't be verified).
func (m *ComputeMiner) NodePubKey(ctx context.Context, nodeID string) (string, error) {
	if m.pool == nil {
		return "", errors.New("compute mining: no pool")
	}
	var pub *string
	if err := m.pool.QueryRow(ctx,
		`SELECT ed25519_pubkey FROM inference_nodes WHERE id = $1`, nodeID,
	).Scan(&pub); err != nil {
		return "", err
	}
	if pub == nil || *pub == "" {
		return "", fmt.Errorf("compute mining: node %q has no registered pubkey", nodeID)
	}
	return *pub, nil
}

// NodeWorkspace returns the operator workspace that owns a node (node→workspace
// is 1:1). Used by PoVI staking to resolve which workspace's LENS to lock as a
// node's collateral.
func (m *ComputeMiner) NodeWorkspace(ctx context.Context, nodeID string) (string, error) {
	if m.pool == nil {
		return "", errors.New("compute mining: no pool")
	}
	var ws string
	if err := m.pool.QueryRow(ctx,
		`SELECT workspace_id FROM inference_nodes WHERE id = $1`, nodeID,
	).Scan(&ws); err != nil {
		return "", err
	}
	return ws, nil
}

// NodeURL returns a node's reachable URL — used by PoVI Part 3 so Lens can
// issue a challenge request to the node that produced a receipt.
func (m *ComputeMiner) NodeURL(ctx context.Context, nodeID string) (string, error) {
	if m.pool == nil {
		return "", errors.New("compute mining: no pool")
	}
	var url string
	if err := m.pool.QueryRow(ctx,
		`SELECT url FROM inference_nodes WHERE id = $1`, nodeID,
	).Scan(&url); err != nil {
		return "", err
	}
	return url, nil
}

// validateNodeURL enforces http(s) + non-empty host.
func validateNodeURL(s string) error {
	if s == "" {
		return ErrInvalidNodeURL
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidNodeURL
	}
	return nil
}

// verifyNodeAsync probes the node and flips verified=true on a
// successful response. Best-effort — runs in a goroutine,
// errors are logged via slog if a logger were available; here
// we just leave the row unverified.
func (m *ComputeMiner) verifyNodeAsync(nodeID, nodeURL, provider string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probe := "/health"
	switch provider {
	case "ollama":
		probe = "/api/tags"
	case "vllm":
		probe = "/v1/models"
	}
	if err := safehttp.CheckURL(nodeURL); err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeURL+probe, nil)
	if err != nil {
		return
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	if m.pool == nil {
		return
	}
	_, _ = m.pool.Exec(ctx, `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`, nodeID)
}

// ─── RecordServedRequest ─────────────────────────

// RecordServedRequest credits the node owner and updates the
// per-node metrics row. Returns nil silently when:
//   - the node isn't found (it was deleted between routing and
//     accounting — rare race),
//   - the requester IS the owner (no self-serving),
//   - tokens is zero or negative.
//
// RecordServedRequest mints compute LENS to the node owner for a served
// cross-workspace request. requestID MUST be a SERVER-DERIVED work-product key
// (the future live wire-up threads it through NotifyServed; a caller-supplied id
// defeats idempotency). An empty requestID mints nothing (fail-closed). The mint
// is idempotent on (requestID, node-owner workspace) and gated on verified-to-earn.
func (m *ComputeMiner) RecordServedRequest(
	ctx context.Context,
	nodeID string,
	requestingWorkspace string,
	requestID string,
	tokens int,
	latencyMs int64,
) error {
	if tokens <= 0 || nodeID == "" {
		return nil
	}
	node, err := m.getNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return ErrNodeNotFound
	}
	// No self-serving — a workspace running its own GPU and
	// hitting its own request doesn't get to print LENS.
	if node.WorkspaceID == requestingWorkspace || requestingWorkspace == "" {
		// Still update metrics so the node owner sees activity.
		_ = m.updateMetrics(ctx, nodeID, tokens, latencyMs)
		return nil
	}

	earning := EarningRate(node.GPUType, tokens)
	meta := map[string]interface{}{
		"node_id":              nodeID,
		"gpu_type":             node.GPUType,
		"tokens":               tokens,
		"latency_ms":           latencyMs,
		"requesting_workspace": requestingWorkspace,
	}
	desc := fmt.Sprintf("compute serve: %d tokens on %s", tokens, node.GPUType)
	// Phase-3 Item 1: mint HELD (not spendable), mirroring cache. CreditOnceHeld
	// writes the uncounted TypeComputeMineHeld at the mint moment (U6 floor +
	// rate cap apply via mintTypeList) and a traffic_mint_holds claim; the
	// finalize sweeper settles it to the counted TypeComputeMine after the
	// window — and it is revocable before then (the clawback surface node mints
	// previously lacked). U6: idempotent on (requestID, node-owner). Empty
	// requestID ⇒ ErrNoMintRequestID ⇒ no mint (the dormant hook passes "" until
	// the live wire-up threads a server-derived id).
	if _, err := m.ledger.CreditOnceHeld(ctx, requestID, node.WorkspaceID, earning, TypeComputeMine, desc, m.holdbackWindow, meta); err != nil {
		return err
	}
	return m.updateMetrics(ctx, nodeID, tokens, latencyMs)
}

// updateMetrics is the small UPSERT path the public Record path
// funnels through. EMA weights are picked to track recent
// performance without thrashing.
func (m *ComputeMiner) updateMetrics(ctx context.Context, nodeID string, tokens int, latencyMs int64) error {
	if m.pool == nil {
		return nil
	}
	_, err := m.pool.Exec(ctx, `
		UPDATE node_metrics
		SET requests_served = requests_served + 1,
		    tokens_served   = tokens_served + $2,
		    avg_latency_ms  = CASE WHEN avg_latency_ms = 0 THEN $3
		                           ELSE (avg_latency_ms * 4 + $3) / 5 END,
		    last_active_at  = NOW()
		WHERE node_id = $1`,
		nodeID, tokens, latencyMs)
	if err != nil {
		return fmt.Errorf("compute mining: update metrics: %w", err)
	}
	return nil
}

// ─── lookups ─────────────────────────────────────

// getNode is the private "fetch row by id" helper used by
// RecordServedRequest.
func (m *ComputeMiner) getNode(ctx context.Context, nodeID string) (*InferenceNode, error) {
	if m.pool == nil {
		return nil, nil
	}
	row := m.pool.QueryRow(ctx, `
		SELECT id, workspace_id, url, provider, models, gpu_type, max_concurrent,
		       price_per_token, active, verified, created_at
		FROM inference_nodes WHERE id = $1`, nodeID)
	var n InferenceNode
	if err := row.Scan(&n.ID, &n.WorkspaceID, &n.URL, &n.Provider, &n.Models,
		&n.GPUType, &n.MaxConcurrent, &n.PricePerToken,
		&n.Active, &n.Verified, &n.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("compute mining: get node: %w", err)
	}
	return &n, nil
}

// ListNodes returns every node owned by `workspaceID`, active
// or not. Newest first.
func (m *ComputeMiner) ListNodes(ctx context.Context, workspaceID string) ([]InferenceNode, error) {
	if m.pool == nil {
		return nil, nil
	}
	rows, err := m.pool.Query(ctx, `
		SELECT id, workspace_id, url, provider, models, gpu_type, max_concurrent,
		       price_per_token, active, verified, created_at
		FROM inference_nodes WHERE workspace_id = $1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("compute mining: list nodes: %w", err)
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListAvailableNodes returns the verified + active nodes that
// serve `model`. Used by the local router when looking for
// network capacity.
func (m *ComputeMiner) ListAvailableNodes(ctx context.Context, model string) ([]InferenceNode, error) {
	if m.pool == nil {
		return nil, nil
	}
	// `$1 = ANY(models)` does the array containment check using
	// the GIN index defined in 0020.
	rows, err := m.pool.Query(ctx, `
		SELECT id, workspace_id, url, provider, models, gpu_type, max_concurrent,
		       price_per_token, active, verified, created_at
		FROM inference_nodes
		WHERE verified = TRUE AND active = TRUE AND $1 = ANY(models)
		ORDER BY price_per_token ASC, created_at ASC`, model)
	if err != nil {
		return nil, fmt.Errorf("compute mining: list available: %w", err)
	}
	defer rows.Close()
	return scanNodes(rows)
}

func scanNodes(rows pgx.Rows) ([]InferenceNode, error) {
	var out []InferenceNode
	for rows.Next() {
		var n InferenceNode
		if err := rows.Scan(&n.ID, &n.WorkspaceID, &n.URL, &n.Provider, &n.Models,
			&n.GPUType, &n.MaxConcurrent, &n.PricePerToken,
			&n.Active, &n.Verified, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("compute mining: scan node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeactivateNode flips active=false. We don't delete — keeping
// historical rows lets the ledger metadata's node_id references
// stay resolvable.
//
// workspaceID is required: the UPDATE is scoped to both id AND workspace_id
// so that an API key from workspace-A cannot deactivate workspace-B's nodes.
func (m *ComputeMiner) DeactivateNode(ctx context.Context, nodeID, workspaceID string) error {
	if m.pool == nil {
		return nil
	}
	tag, err := m.pool.Exec(ctx,
		`UPDATE inference_nodes SET active = FALSE WHERE id = $1 AND workspace_id = $2`,
		nodeID, workspaceID)
	if err != nil {
		return fmt.Errorf("compute mining: deactivate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNodeNotFound
	}
	return nil
}

// GetNodeStats returns the metrics row for `nodeID`. Zero-value
// stats (rather than nil) for an unknown node so dashboards
// don't have to nil-check.
func (m *ComputeMiner) GetNodeStats(ctx context.Context, nodeID string) (*NodeMetrics, error) {
	if m.pool == nil {
		return &NodeMetrics{NodeID: nodeID}, nil
	}
	row := m.pool.QueryRow(ctx, `
		SELECT node_id, requests_served, tokens_served, avg_latency_ms,
		       error_rate, uptime_pct, COALESCE(last_active_at, '1970-01-01'::timestamptz)
		FROM node_metrics WHERE node_id = $1`, nodeID)
	var s NodeMetrics
	if err := row.Scan(&s.NodeID, &s.RequestsServed, &s.TokensServed,
		&s.AvgLatencyMs, &s.ErrorRate, &s.UptimePct, &s.LastActiveAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &NodeMetrics{NodeID: nodeID, UptimePct: 100}, nil
		}
		return nil, fmt.Errorf("compute mining: get metrics: %w", err)
	}
	return &s, nil
}

// GetWorkspaceStats summarises compute earnings for a workspace.
// Reads the snapshot from the balance table + sums the active
// node count + total tokens served across all nodes the
// workspace owns.
func (m *ComputeMiner) GetWorkspaceStats(ctx context.Context, workspaceID string) (*ComputeMiningStats, error) {
	stats := &ComputeMiningStats{WorkspaceID: workspaceID}
	if m.pool == nil {
		return stats, nil
	}
	// Tokens served across the workspace's nodes.
	row := m.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE n.active),
		       COALESCE(SUM(nm.tokens_served), 0)
		FROM inference_nodes n
		LEFT JOIN node_metrics nm ON nm.node_id = n.id
		WHERE n.workspace_id = $1`, workspaceID)
	var active int
	var tokens int64
	if err := row.Scan(&active, &tokens); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("compute mining: workspace stats: %w", err)
	}
	stats.NodesActive = active
	stats.TokensServedTotal = tokens

	// Earnings — sum the compute_mine ledger rows for this
	// workspace. (Ledger amounts for credits are positive.)
	row = m.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM lens_token_ledger
		WHERE workspace_id = $1 AND type = $2`, workspaceID, TypeComputeMine)
	if err := row.Scan(&stats.EarnedTotal); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("compute mining: earnings: %w", err)
	}
	return stats, nil
}
