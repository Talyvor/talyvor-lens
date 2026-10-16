package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultAgent    = "default"
	sessionMaxAge   = 24 * time.Hour
	cleanupInterval = 1 * time.Hour
	activeWindow    = 1 * time.Hour
)

// pgxDB is the subset of *pgxpool.Pool the tracker needs. nil pool is
// supported — the tracker keeps everything in memory.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type SessionTracker struct {
	pool     pgxDB
	mu       sync.RWMutex
	sessions map[string]*Session
}

type Session struct {
	ID                string    `json:"id"`
	WorkspaceID       string    `json:"workspace_id"`
	AgentName         string    `json:"agent_name"`
	StartedAt         time.Time `json:"started_at"`
	LastActiveAt      time.Time `json:"last_active_at"`
	TurnCount         int       `json:"turn_count"`
	TotalInputTokens  int       `json:"total_input_tokens"`
	TotalOutputTokens int       `json:"total_output_tokens"`
	TotalCostUSD      float64   `json:"total_cost_usd"`
	CacheHits         int       `json:"cache_hits"`
	CacheMisses       int       `json:"cache_misses"`
	Turns             []Turn    `json:"turns"`
}

// Turn carries the per-call record. Prompt and Response are kept in
// memory only — they never reach the session_turns DB row, per the spec's
// privacy constraint.
type Turn struct {
	TurnNumber   int       `json:"turn_number"`
	Role         string    `json:"role"`
	Prompt       string    `json:"-"`
	Response     string    `json:"-"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"`
	Cached       bool      `json:"cached"`
	CreatedAt    time.Time `json:"created_at"`
}

type SessionSummary struct {
	SessionID      string        `json:"session_id"`
	AgentName      string        `json:"agent_name"`
	TurnCount      int           `json:"turn_count"`
	TotalCostUSD   float64       `json:"total_cost_usd"`
	CacheHitRate   float64       `json:"cache_hit_rate"`
	AvgCostPerTurn float64       `json:"avg_cost_per_turn"`
	MostUsedModel  string        `json:"most_used_model"`
	StartedAt      time.Time     `json:"started_at"`
	Duration       time.Duration `json:"duration"`
}

func New(pool *pgxpool.Pool) *SessionTracker {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newTracker(db)
}

func newTracker(pool pgxDB) *SessionTracker {
	return &SessionTracker{pool: pool, sessions: make(map[string]*Session)}
}

const insertSessionSQL = `INSERT INTO sessions (id, workspace_id, agent_name)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO NOTHING`

// GetOrCreate returns the session for sessionID or creates one. It checks the
// in-memory map first, then falls back to a PG read so sessions created on
// another instance are visible here without a round-trip on every request.
// A session that already exists in another workspace is nil (B42.2): the
// caller refuses it and nothing of that workspace's session changes.
func (t *SessionTracker) GetOrCreate(ctx context.Context, sessionID, workspaceID, agentName string) *Session {
	if agentName == "" {
		agentName = defaultAgent
	}

	// Fast path: already in memory.
	t.mu.RLock()
	if s, ok := t.sessions[sessionID]; ok {
		t.mu.RUnlock()
		return ownedBy(s, workspaceID)
	}
	t.mu.RUnlock()

	// Slow path: try PG — another instance may have created this session.
	if loaded, err := t.loadFromDB(ctx, sessionID); err == nil && loaded != nil {
		t.mu.Lock()
		if s, ok := t.sessions[sessionID]; ok { // double-check after lock
			t.mu.Unlock()
			return ownedBy(s, workspaceID)
		}
		t.sessions[sessionID] = loaded
		t.mu.Unlock()
		return ownedBy(loaded, workspaceID)
	}

	// New session.
	now := time.Now().UTC()
	s := &Session{
		ID:           sessionID,
		WorkspaceID:  workspaceID,
		AgentName:    agentName,
		StartedAt:    now,
		LastActiveAt: now,
	}
	t.mu.Lock()
	if existing, ok := t.sessions[sessionID]; ok { // TOCTOU guard
		t.mu.Unlock()
		return ownedBy(existing, workspaceID)
	}
	t.sessions[sessionID] = s
	t.mu.Unlock()

	if t.pool != nil {
		if _, err := t.pool.Exec(ctx, insertSessionSQL, s.ID, s.WorkspaceID, s.AgentName); err != nil {
			slog.Warn("session: insert sessions row failed", slog.String("err", err.Error()))
		}
	}
	return s
}

// ownedBy is s when it belongs to workspaceID, else nil. WorkspaceID never changes after creation.
func ownedBy(s *Session, workspaceID string) *Session {
	if s.WorkspaceID != workspaceID {
		return nil
	}
	return s
}

const selectSessionSQL = `
SELECT id, workspace_id, agent_name,
       COALESCE(turn_count, 0), COALESCE(total_input_tokens, 0), COALESCE(total_output_tokens, 0),
       COALESCE(total_cost_usd, 0.0), COALESCE(cache_hits, 0), COALESCE(cache_misses, 0),
       last_active_at, created_at
FROM sessions WHERE id = $1`

// loadFromDB reads an existing session row from Postgres. Returns (nil, nil)
// when no row exists — callers can check for nil to distinguish "not found"
// from an error.
func (t *SessionTracker) loadFromDB(ctx context.Context, sessionID string) (*Session, error) {
	if t.pool == nil {
		return nil, nil
	}
	var s Session
	err := t.pool.QueryRow(ctx, selectSessionSQL, sessionID).Scan(
		&s.ID, &s.WorkspaceID, &s.AgentName,
		&s.TurnCount, &s.TotalInputTokens, &s.TotalOutputTokens,
		&s.TotalCostUSD, &s.CacheHits, &s.CacheMisses,
		&s.LastActiveAt, &s.StartedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: load from db: %w", err)
	}
	return &s, nil
}

const insertTurnSQL = `INSERT INTO session_turns
  (session_id, turn_number, role, model, input_tokens, output_tokens, cost_usd, cached)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

const updateSessionTotalsSQL = `UPDATE sessions SET
  turn_count = $1,
  total_input_tokens = $2,
  total_output_tokens = $3,
  total_cost_usd = $4,
  cache_hits = $5,
  cache_misses = $6,
  last_active_at = NOW()
WHERE id = $7`

// RecordTurn appends a turn to the in-memory session and updates running
// totals (cost, tokens, cache stats). Persistence is best-effort: errors
// are logged but never propagated to the caller — session tracking must
// not break the main proxy path.
//
// NOTE: prompt and response text are populated on the in-memory turn but
// NEVER sent to the session_turns row. Memory-only carries the privacy
// constraint from the spec.
func (t *SessionTracker) RecordTurn(ctx context.Context, sessionID string, turn Turn) error {
	t.mu.Lock()
	s, ok := t.sessions[sessionID]
	if !ok {
		t.mu.Unlock()
		return fmt.Errorf("session: unknown session %q", sessionID)
	}
	s.TurnCount++
	turn.TurnNumber = s.TurnCount
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}
	s.Turns = append(s.Turns, turn)
	s.TotalInputTokens += turn.InputTokens
	s.TotalOutputTokens += turn.OutputTokens
	s.TotalCostUSD += turn.CostUSD
	if turn.Cached {
		s.CacheHits++
	} else {
		s.CacheMisses++
	}
	s.LastActiveAt = time.Now().UTC()

	// Snapshot values needed by the DB writes before dropping the lock —
	// no further reads of `s` after this point.
	snap := struct {
		turnNumber                int
		role, model               string
		inputTokens, outputTokens int
		costUSD                   float64
		cached                    bool
		turnCount                 int
		totalIn, totalOut         int
		totalCost                 float64
		hits, misses              int
	}{
		turnNumber:   turn.TurnNumber,
		role:         turn.Role,
		model:        turn.Model,
		inputTokens:  turn.InputTokens,
		outputTokens: turn.OutputTokens,
		costUSD:      turn.CostUSD,
		cached:       turn.Cached,
		turnCount:    s.TurnCount,
		totalIn:      s.TotalInputTokens,
		totalOut:     s.TotalOutputTokens,
		totalCost:    s.TotalCostUSD,
		hits:         s.CacheHits,
		misses:       s.CacheMisses,
	}
	t.mu.Unlock()

	if t.pool != nil {
		if _, err := t.pool.Exec(ctx, insertTurnSQL,
			sessionID, snap.turnNumber, snap.role, snap.model,
			snap.inputTokens, snap.outputTokens, snap.costUSD, snap.cached,
		); err != nil {
			slog.Warn("session: insert turn failed",
				slog.String("session_id", sessionID),
				slog.String("err", err.Error()),
			)
		}
		if _, err := t.pool.Exec(ctx, updateSessionTotalsSQL,
			snap.turnCount, snap.totalIn, snap.totalOut, snap.totalCost,
			snap.hits, snap.misses, sessionID,
		); err != nil {
			slog.Warn("session: update totals failed",
				slog.String("session_id", sessionID),
				slog.String("err", err.Error()),
			)
		}
	}
	return nil
}

// GetSession returns a copy of the session — callers can mutate freely
// without affecting tracker state.
func (t *SessionTracker) GetSession(sessionID string) (*Session, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.sessions[sessionID]
	if !ok {
		return nil, false
	}
	copied := *s
	copied.Turns = append([]Turn(nil), s.Turns...)
	return &copied, true
}

func (t *SessionTracker) GetSessionCost(sessionID string) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if s, ok := t.sessions[sessionID]; ok {
		return s.TotalCostUSD
	}
	return 0
}

// SummariseSession returns a compact view: hit rate, average cost, most
// used model, and uptime. Returns a zero-value summary if the session is
// unknown so callers can render a "not found" page without nil checks.
func (t *SessionTracker) SummariseSession(_ context.Context, sessionID string) SessionSummary {
	t.mu.RLock()
	defer t.mu.RUnlock()

	s, ok := t.sessions[sessionID]
	if !ok {
		return SessionSummary{SessionID: sessionID}
	}

	total := s.CacheHits + s.CacheMisses
	var hitRate float64
	if total > 0 {
		hitRate = float64(s.CacheHits) / float64(total)
	}
	avgCost := 0.0
	if s.TurnCount > 0 {
		avgCost = s.TotalCostUSD / float64(s.TurnCount)
	}
	mostModel := mostUsedModel(s.Turns)

	return SessionSummary{
		SessionID:      s.ID,
		AgentName:      s.AgentName,
		TurnCount:      s.TurnCount,
		TotalCostUSD:   s.TotalCostUSD,
		CacheHitRate:   hitRate,
		AvgCostPerTurn: avgCost,
		MostUsedModel:  mostModel,
		StartedAt:      s.StartedAt,
		Duration:       time.Since(s.StartedAt),
	}
}

func mostUsedModel(turns []Turn) string {
	counts := make(map[string]int, len(turns))
	for _, tn := range turns {
		if tn.Model == "" {
			continue
		}
		counts[tn.Model]++
	}
	var mostModel string
	var mostCount int
	for m, c := range counts {
		if c > mostCount {
			mostCount = c
			mostModel = m
		}
	}
	return mostModel
}

// ListActiveByWorkspace returns sessions for a workspace that have been
// active within the last hour. Used by /v1/sessions to power dashboards.
func (t *SessionTracker) ListActiveByWorkspace(workspaceID string) []*Session {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cutoff := time.Now().Add(-activeWindow)
	var out []*Session
	for _, s := range t.sessions {
		if s.WorkspaceID != workspaceID {
			continue
		}
		if s.LastActiveAt.Before(cutoff) {
			continue
		}
		copied := *s
		// Don't return Turns from the list endpoint — they include
		// prompt/response strings the caller may not want over JSON.
		copied.Turns = nil
		out = append(out, &copied)
	}
	return out
}

// StartCleanup spawns the background eviction goroutine that drops
// sessions inactive for sessionMaxAge. Exits on ctx.Done().
func (t *SessionTracker) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.evictStale(sessionMaxAge)
		}
	}
}

func (t *SessionTracker) evictStale(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, s := range t.sessions {
		if s.LastActiveAt.Before(cutoff) {
			delete(t.sessions, id)
		}
	}
}
