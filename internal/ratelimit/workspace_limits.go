package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// workspace_limits.go — B39.9: an operator's request limits for one workspace, stored in
// workspace_rate_limits (0255) and set through PUT /v1/admin/workspaces/{wsID}/rate-limits. Check
// reads them through a short cache; a workspace with no row keeps the default rules.

// workspaceLimitsTTL bounds how long another instance keeps serving a changed limit.
const workspaceLimitsTTL = 30 * time.Second

// ErrInvalidLimits is a limit that is not a positive request count.
var ErrInvalidLimits = errors.New("each limit must be a positive number of requests")

// WorkspaceRuleSource answers a workspace's own limits, ok=false when it has none.
type WorkspaceRuleSource interface {
	WorkspaceRule(ctx context.Context, wsID string) (RateRule, bool)
}

type cachedRule struct {
	rule RateRule
	ok   bool
	at   time.Time
}

type WorkspaceLimits struct {
	pool  *pgxpool.Pool
	mu    sync.Mutex
	cache map[string]cachedRule
}

func NewWorkspaceLimits(pool *pgxpool.Pool) *WorkspaceLimits {
	return &WorkspaceLimits{pool: pool, cache: map[string]cachedRule{}}
}

// Get reads the workspace's limits from the database, ok=false when it has none.
func (s *WorkspaceLimits) Get(ctx context.Context, wsID string) (RateRule, bool, error) {
	r := RateRule{WorkspaceID: wsID}
	err := s.pool.QueryRow(ctx, `SELECT requests_per_second, requests_per_minute, requests_per_hour
		FROM workspace_rate_limits WHERE workspace_id = $1`, wsID).
		Scan(&r.RequestsPerSecond, &r.RequestsPerMinute, &r.RequestsPerHour)
	if errors.Is(err, pgx.ErrNoRows) {
		return RateRule{}, false, nil
	}
	if err != nil {
		return RateRule{}, false, err
	}
	return r, true, nil
}

// Set stores the workspace's limits, replacing any it had.
func (s *WorkspaceLimits) Set(ctx context.Context, r RateRule) error {
	if r.RequestsPerSecond <= 0 || r.RequestsPerMinute <= 0 || r.RequestsPerHour <= 0 {
		return ErrInvalidLimits
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO workspace_rate_limits
		(workspace_id, requests_per_second, requests_per_minute, requests_per_hour, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (workspace_id) DO UPDATE SET requests_per_second = EXCLUDED.requests_per_second,
			requests_per_minute = EXCLUDED.requests_per_minute, requests_per_hour = EXCLUDED.requests_per_hour,
			updated_at = now()`,
		r.WorkspaceID, r.RequestsPerSecond, r.RequestsPerMinute, r.RequestsPerHour)
	s.forget(r.WorkspaceID)
	return err
}

// Delete puts the workspace back on the default limits.
func (s *WorkspaceLimits) Delete(ctx context.Context, wsID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM workspace_rate_limits WHERE workspace_id = $1`, wsID)
	s.forget(wsID)
	return err
}

func (s *WorkspaceLimits) forget(wsID string) {
	s.mu.Lock()
	delete(s.cache, wsID)
	s.mu.Unlock()
}

// WorkspaceRule is Get through the cache. A database error keeps the default limits (and is not cached),
// as a Redis error fails open in Check.
func (s *WorkspaceLimits) WorkspaceRule(ctx context.Context, wsID string) (RateRule, bool) {
	s.mu.Lock()
	c, hit := s.cache[wsID]
	s.mu.Unlock()
	if hit && time.Since(c.at) < workspaceLimitsTTL {
		return c.rule, c.ok
	}
	r, ok, err := s.Get(ctx, wsID)
	if err != nil {
		slog.Warn("ratelimit: workspace limits read failed; using defaults",
			slog.String("workspace_id", wsID), slog.String("err", err.Error()))
		return RateRule{}, false
	}
	s.mu.Lock()
	s.cache[wsID] = cachedRule{rule: r, ok: ok, at: time.Now()}
	s.mu.Unlock()
	return r, ok
}
