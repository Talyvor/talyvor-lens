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
// reads them from an in-memory copy of the whole table (it is small), so no request reaches the database
// — the public routes rate-limit on an unauthenticated workspace header. A workspace with no row keeps the
// default rules.

// workspaceLimitsTTL bounds how long another instance keeps serving a changed limit.
const workspaceLimitsTTL = 30 * time.Second

// ErrInvalidLimits is a limit that is not a positive request count.
var ErrInvalidLimits = errors.New("each limit must be a positive number of requests")

// WorkspaceRuleSource answers a workspace's own limits, ok=false when it has none.
type WorkspaceRuleSource interface {
	WorkspaceRule(ctx context.Context, wsID string) (RateRule, bool)
}

type WorkspaceLimits struct {
	pool  *pgxpool.Pool
	mu    sync.RWMutex
	rules map[string]RateRule
}

func NewWorkspaceLimits(pool *pgxpool.Pool) *WorkspaceLimits {
	return &WorkspaceLimits{pool: pool, rules: map[string]RateRule{}}
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
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_rate_limits
		(workspace_id, requests_per_second, requests_per_minute, requests_per_hour, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (workspace_id) DO UPDATE SET requests_per_second = EXCLUDED.requests_per_second,
			requests_per_minute = EXCLUDED.requests_per_minute, requests_per_hour = EXCLUDED.requests_per_hour,
			updated_at = now()`,
		r.WorkspaceID, r.RequestsPerSecond, r.RequestsPerMinute, r.RequestsPerHour); err != nil {
		return err
	}
	s.mu.Lock()
	s.rules[r.WorkspaceID] = r
	s.mu.Unlock()
	return nil
}

// Delete puts the workspace back on the default limits.
func (s *WorkspaceLimits) Delete(ctx context.Context, wsID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM workspace_rate_limits WHERE workspace_id = $1`, wsID); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.rules, wsID)
	s.mu.Unlock()
	return nil
}

// Reload replaces the in-memory copy with the table. On error the old copy is kept.
func (s *WorkspaceLimits) Reload(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id, requests_per_second, requests_per_minute, requests_per_hour
		FROM workspace_rate_limits`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := map[string]RateRule{}
	for rows.Next() {
		var r RateRule
		if err := rows.Scan(&r.WorkspaceID, &r.RequestsPerSecond, &r.RequestsPerMinute, &r.RequestsPerHour); err != nil {
			return err
		}
		next[r.WorkspaceID] = r
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.rules = next
	s.mu.Unlock()
	return nil
}

// StartRefresh reloads the table every workspaceLimitsTTL until ctx ends, so a limit set on another
// instance is enforced here within that.
func (s *WorkspaceLimits) StartRefresh(ctx context.Context) {
	go func() {
		t := time.NewTicker(workspaceLimitsTTL)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.Reload(ctx); err != nil {
					slog.Warn("ratelimit: workspace limits reload failed (old copy kept)", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// WorkspaceRule answers from the in-memory copy; it never reaches the database.
func (s *WorkspaceLimits) WorkspaceRule(_ context.Context, wsID string) (RateRule, bool) {
	s.mu.RLock()
	r, ok := s.rules[wsID]
	s.mu.RUnlock()
	return r, ok
}
