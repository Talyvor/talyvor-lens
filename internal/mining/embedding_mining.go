package mining

// embedding_mining.go — third LENS mining track. Workspaces
// running a local embedding model (typically a small CPU-friendly
// model like nomic-embed-text) earn LENS when their endpoint
// serves embedding requests for other workspaces.
//
// Shares the same LedgerStore + verification-probe pattern as
// compute_mining.go; the differences are:
//   - registry table (embedding_nodes vs inference_nodes)
//   - rate matrix indexed by *model* family, not GPU
//   - dimensions are part of the marketplace filter
//   - the probe issues a real embedding request rather than a
//     health ping (embedding endpoints rarely have /health)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/safehttp"
)

// ─── constants ───────────────────────────────────

// EmbeddingMineBaseRate is the LENS earned for serving 1000
// embeddings on a baseline small model. Embedding work is CPU-
// friendly so the rate is much lower than ComputeMineBaseRate.
const EmbeddingMineBaseRate int64 = 2_000 // 0.002 LENS in µLENS (SEC-2)

// Model-family multipliers — bigger models do more semantic work
// per call, so we pay them more.
const (
	ModelMultiplierSmall  = 1.0
	ModelMultiplierMedium = 1.5
	ModelMultiplierLarge  = 2.0
)

// TypeEmbeddingMine is the ledger row type for this track.
const TypeEmbeddingMine = "embedding_mine"

// knownEmbeddingModels is the closed allowlist for the `model`
// column. New models go in here once we know their multiplier.
// Keep the values lowercase — RegisterNode lowercases input.
var knownEmbeddingModels = map[string]float64{
	"nomic-embed-text":       ModelMultiplierSmall,
	"e5-large":               ModelMultiplierMedium,
	"mxbai-embed-large":      ModelMultiplierLarge,
	"text-embedding-3-small": ModelMultiplierSmall,
	"text-embedding-3-large": ModelMultiplierLarge,
}

// knownEmbeddingDimensions is the validation set for the
// `dimensions` column — caps the marketplace to dimensions Lens
// actually understands.
var knownEmbeddingDimensions = map[int]bool{
	768:  true,
	1024: true,
	1536: true,
}

// ─── types ───────────────────────────────────────

// EmbeddingNode mirrors one row of embedding_nodes.
type EmbeddingNode struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	URL         string    `json:"url"`
	Model       string    `json:"model"`
	Dimensions  int       `json:"dimensions"`
	MaxBatch    int       `json:"max_batch"`
	SpeedTPS    int       `json:"speed_tps"`
	Active      bool      `json:"active"`
	Verified    bool      `json:"verified"`
	CreatedAt   time.Time `json:"created_at"`
	// NodeSecretHash is the bcrypt hash of the node's shared secret,
	// set by the HTTP handler before calling RegisterNode. Never
	// marshalled to JSON (json:"-") so the hash never leaks to API
	// consumers. Empty string stores NULL via NULLIF in the INSERT.
	NodeSecretHash string `json:"-"`
}

// EmbeddingMiningStats is the response shape for the
// /v1/workspaces/:wsID/tokens/mining/embeddings endpoint.
type EmbeddingMiningStats struct {
	WorkspaceID      string `json:"workspace_id"`
	NodesActive      int    `json:"nodes_active"`
	EmbeddingsServed int64  `json:"embeddings_served"`
	TotalEarned      int64  `json:"total_earned_ulens"`      // µLENS (SEC-2)
	EstimatedMonthly int64  `json:"estimated_monthly_ulens"` // µLENS (SEC-2)
}

// ─── errors ──────────────────────────────────────

var (
	ErrInvalidEmbeddingModel      = errors.New("embedding mining: unknown model (see EmbeddingRates() for allowed list)")
	ErrInvalidEmbeddingDimensions = errors.New("embedding mining: dimensions must be 768, 1024, or 1536")
)

// ─── EarningRate ─────────────────────────────────

// EmbeddingModelMultiplier looks up the LENS multiplier. Unknown
// models fall back to the small-model tier so a typo can't
// produce a giant payout.
func EmbeddingModelMultiplier(model string) float64 {
	if m, ok := knownEmbeddingModels[strings.ToLower(model)]; ok {
		return m
	}
	return ModelMultiplierSmall
}

// EmbeddingEarningRate is the µLENS payout for `count` embeddings on a `model`
// (SEC-2: integer smallest-unit). The Tier-2 model multiplier and count/1000
// factor scale the integer base rate; the result is floored (MulFloor) — a mint
// rounds DOWN, the dropped sub-µLENS remainder retained by the protocol.
func EmbeddingEarningRate(model string, count int) int64 {
	if count <= 0 {
		return 0
	}
	return MulFloor(EmbeddingMineBaseRate, EmbeddingModelMultiplier(model)*(float64(count)/1000.0))
}

// EmbeddingRates returns the public rate table — backs the
// embedding section of /v1/tokens/rates. base_per_1k is µLENS (SEC-2).
func EmbeddingRates() map[string]any {
	models := map[string]float64{}
	for m, mult := range knownEmbeddingModels {
		models[m] = mult
	}
	return map[string]any{
		"base_per_1k_ulens": EmbeddingMineBaseRate,
		"models":            models,
	}
}

// ─── EmbeddingMiner ──────────────────────────────

// EmbeddingMiner is the persistence + earning engine for
// embedding mining.
type EmbeddingMiner struct {
	ledger         *LedgerStore
	pool           pgxDB
	httpClient     *http.Client
	holdbackWindow time.Duration // Phase-3 Item 1: the mint lands HELD, finalizes after this (default 72h)
}

func NewEmbeddingMiner(ledger *LedgerStore, pool pgxDB) *EmbeddingMiner {
	return &EmbeddingMiner{
		ledger:         ledger,
		pool:           pool,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		holdbackWindow: 72 * time.Hour, // Phase-3 Item 1: default held window (mirrors cache/pool-royalty)
	}
}

// SetHoldbackWindow sets the held→spendable delay for embedding mints (Phase-3
// Item 1). Non-positive keeps the 72h default. The mint lands HELD and the
// traffic finalize sweeper settles it after this window (revocable before then).
func (m *EmbeddingMiner) SetHoldbackWindow(d time.Duration) {
	if d > 0 {
		m.holdbackWindow = d
	}
}

// SetHTTPClient lets tests inject an httptest.Server-backed
// client for the async verification probe.
func (m *EmbeddingMiner) SetHTTPClient(c *http.Client) { m.httpClient = c }

// ─── RegisterNode ────────────────────────────────

// RegisterNode validates the model + dimensions allowlist,
// inserts the row with verified=false, and kicks off an async
// probe that issues a real "hello world" embedding request and
// flips verified=true on success.
func (m *EmbeddingMiner) RegisterNode(ctx context.Context, in EmbeddingNode) (*EmbeddingNode, error) {
	if err := validateNodeURL(in.URL); err != nil {
		return nil, err
	}
	if in.WorkspaceID == "" {
		return nil, errors.New("embedding mining: workspace_id required")
	}
	in.Model = strings.ToLower(in.Model)
	if _, ok := knownEmbeddingModels[in.Model]; !ok {
		return nil, ErrInvalidEmbeddingModel
	}
	if in.Dimensions == 0 {
		in.Dimensions = 1536
	}
	if !knownEmbeddingDimensions[in.Dimensions] {
		return nil, ErrInvalidEmbeddingDimensions
	}
	if in.MaxBatch <= 0 {
		in.MaxBatch = 100
	}
	if in.SpeedTPS <= 0 {
		in.SpeedTPS = 500
	}
	in.URL = strings.TrimRight(in.URL, "/")
	in.Active = true
	in.Verified = false

	if m.pool == nil {
		in.ID = fmt.Sprintf("emb_%d", time.Now().UnixNano())
		in.CreatedAt = time.Now().UTC()
		return &in, nil
	}

	row := m.pool.QueryRow(ctx, `
		INSERT INTO embedding_nodes
			(workspace_id, url, model, dimensions, max_batch, speed_tps, node_secret_hash)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
		RETURNING id, created_at`,
		in.WorkspaceID, in.URL, in.Model, in.Dimensions, in.MaxBatch, in.SpeedTPS,
		in.NodeSecretHash,
	)
	if err := row.Scan(&in.ID, &in.CreatedAt); err != nil {
		return nil, fmt.Errorf("embedding mining: insert node: %w", err)
	}

	// Async verification — issue a real embedding for "hello world"
	// against the registered endpoint. We treat any 2xx response as
	// proof of life.
	go m.verifyNodeAsync(in.ID, in.URL, in.Model)
	return &in, nil
}

// verifyNodeAsync issues a "hello world" embedding probe. The
// OpenAI-style /v1/embeddings shape is the de-facto standard
// (Ollama, vLLM, and most local servers expose a compatible
// endpoint), so we send that everywhere — anything 2xx counts.
func (m *EmbeddingMiner) verifyNodeAsync(nodeID, nodeURL, model string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !safehttp.AllowedURL(nodeURL) {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"input": "hello world",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		nodeURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	if m.pool == nil {
		return
	}
	_, _ = m.pool.Exec(ctx, `UPDATE embedding_nodes SET verified = TRUE WHERE id = $1`, nodeID)
}

// ─── RecordEmbeddingsServed ──────────────────────

// RecordEmbeddingsServed credits the node owner when the request
// came from a different workspace. No-ops on self-serving and on
// zero/negative counts so callers can `defer` it safely.
// RecordEmbeddingsServed mints embedding LENS to the node owner. requestID MUST
// be a SERVER-DERIVED work-product key so the mint is idempotent on (requestID,
// node-owner); an empty requestID mints nothing (fail-closed). Dormant today (no
// production caller wires RecordEmbeddingsServed); the live wire-up supplies the
// id. Gated on verified-to-earn via CreditOnce.
func (m *EmbeddingMiner) RecordEmbeddingsServed(
	ctx context.Context,
	nodeID string,
	requestingWorkspace string,
	requestID string,
	embeddingCount int,
) error {
	if embeddingCount <= 0 || nodeID == "" {
		return nil
	}
	node, err := m.getNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return ErrNodeNotFound
	}
	if node.WorkspaceID == requestingWorkspace || requestingWorkspace == "" {
		return nil
	}
	earning := EmbeddingEarningRate(node.Model, embeddingCount)
	meta := map[string]interface{}{
		"node_id":              nodeID,
		"model":                node.Model,
		"embedding_count":      embeddingCount,
		"requesting_workspace": requestingWorkspace,
	}
	desc := fmt.Sprintf("embeddings served: %d on %s", embeddingCount, node.Model)
	// Phase-3 Item 1: mint HELD (not spendable), mirroring cache/compute. Writes
	// the uncounted TypeEmbeddingMineHeld + a traffic_mint_holds claim; the
	// finalize sweeper settles it to the counted TypeEmbeddingMine after the
	// window, and it is revocable before then.
	_, err = m.ledger.CreditOnceHeld(ctx, requestID, node.WorkspaceID, earning, TypeEmbeddingMine, desc, m.holdbackWindow, meta)
	return err
}

// ─── lookups ─────────────────────────────────────

func (m *EmbeddingMiner) getNode(ctx context.Context, nodeID string) (*EmbeddingNode, error) {
	if m.pool == nil {
		return nil, nil
	}
	row := m.pool.QueryRow(ctx, `
		SELECT id, workspace_id, url, model, dimensions, max_batch, speed_tps,
		       active, verified, created_at
		FROM embedding_nodes WHERE id = $1`, nodeID)
	var n EmbeddingNode
	if err := row.Scan(&n.ID, &n.WorkspaceID, &n.URL, &n.Model, &n.Dimensions,
		&n.MaxBatch, &n.SpeedTPS, &n.Active, &n.Verified, &n.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("embedding mining: get node: %w", err)
	}
	return &n, nil
}

// ListNodes returns every embedding node owned by `workspaceID`,
// active or not. Newest first.
func (m *EmbeddingMiner) ListNodes(ctx context.Context, workspaceID string) ([]EmbeddingNode, error) {
	if m.pool == nil {
		return nil, nil
	}
	rows, err := m.pool.Query(ctx, `
		SELECT id, workspace_id, url, model, dimensions, max_batch, speed_tps,
		       active, verified, created_at
		FROM embedding_nodes WHERE workspace_id = $1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("embedding mining: list nodes: %w", err)
	}
	defer rows.Close()
	return scanEmbeddingNodes(rows)
}

// ListAvailableNodes is the public marketplace surface — only
// verified + active nodes serving `model`. `minDimensions` is
// optional; pass 0 to skip the dimension filter.
func (m *EmbeddingMiner) ListAvailableNodes(
	ctx context.Context,
	model string,
	minDimensions int,
) ([]EmbeddingNode, error) {
	if m.pool == nil {
		return nil, nil
	}
	model = strings.ToLower(model)
	rows, err := m.pool.Query(ctx, `
		SELECT id, workspace_id, url, model, dimensions, max_batch, speed_tps,
		       active, verified, created_at
		FROM embedding_nodes
		WHERE verified = TRUE AND active = TRUE
		  AND model = $1
		  AND dimensions >= $2
		ORDER BY speed_tps DESC, created_at ASC`, model, minDimensions)
	if err != nil {
		return nil, fmt.Errorf("embedding mining: list available: %w", err)
	}
	defer rows.Close()
	return scanEmbeddingNodes(rows)
}

func scanEmbeddingNodes(rows pgx.Rows) ([]EmbeddingNode, error) {
	var out []EmbeddingNode
	for rows.Next() {
		var n EmbeddingNode
		if err := rows.Scan(&n.ID, &n.WorkspaceID, &n.URL, &n.Model, &n.Dimensions,
			&n.MaxBatch, &n.SpeedTPS, &n.Active, &n.Verified, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("embedding mining: scan node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeactivateEmbeddingNode flips active=false. Same rationale as
// compute mining: keep history so ledger metadata stays
// resolvable.
//
// workspaceID is required: the UPDATE is scoped to both id AND workspace_id
// so that an API key from workspace-A cannot deactivate workspace-B's nodes.
func (m *EmbeddingMiner) DeactivateEmbeddingNode(ctx context.Context, nodeID, workspaceID string) error {
	if m.pool == nil {
		return nil
	}
	tag, err := m.pool.Exec(ctx,
		`UPDATE embedding_nodes SET active = FALSE WHERE id = $1 AND workspace_id = $2`,
		nodeID, workspaceID)
	if err != nil {
		return fmt.Errorf("embedding mining: deactivate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNodeNotFound
	}
	return nil
}

// GetStats summarises embedding earnings for a workspace.
func (m *EmbeddingMiner) GetStats(ctx context.Context, workspaceID string) (*EmbeddingMiningStats, error) {
	stats := &EmbeddingMiningStats{WorkspaceID: workspaceID}
	if m.pool == nil {
		return stats, nil
	}

	// Count active nodes for the workspace.
	row := m.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM embedding_nodes WHERE workspace_id = $1 AND active`, workspaceID)
	if err := row.Scan(&stats.NodesActive); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("embedding mining: node count: %w", err)
	}

	// Embedding count + total LENS from the ledger.
	row = m.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((metadata->>'embedding_count')::BIGINT), 0),
		       COALESCE(SUM(amount), 0)
		FROM lens_token_ledger
		WHERE workspace_id = $1 AND type = $2`, workspaceID, TypeEmbeddingMine)
	if err := row.Scan(&stats.EmbeddingsServed, &stats.TotalEarned); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("embedding mining: totals: %w", err)
	}

	// EstimatedMonthly — naive 30-day projection. We don't have a
	// per-day breakdown so we extrapolate from "earnings so far /
	// days since first ledger row", floored at 30 days.
	if stats.TotalEarned > 0 {
		row = m.pool.QueryRow(ctx, `
			SELECT COALESCE(MIN(created_at), NOW())
			FROM lens_token_ledger
			WHERE workspace_id = $1 AND type = $2`, workspaceID, TypeEmbeddingMine)
		var first time.Time
		_ = row.Scan(&first)
		days := time.Since(first).Hours()/24 + 1
		if days < 30 {
			days = 30
		}
		stats.EstimatedMonthly = int64(float64(stats.TotalEarned) / days * 30)
	}

	return stats, nil
}
