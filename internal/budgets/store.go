package budgets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgxDB is the subset of *pgxpool.Pool the Store needs. Tests substitute
// pgxmock; a nil pool short-circuits reads to empty and drops writes so the
// pure in-memory service logic can be exercised without Postgres.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store owns the budgets table and the token_events reconciliation reads.
type Store struct {
	pool pgxDB
}

func NewStore(pool *pgxpool.Pool) *Store {
	// Guard the typed-nil interface trap (a (*pgxpool.Pool)(nil) stored in
	// an interface is != nil but panics on call).
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Store{pool: db}
}

// newStore is the test seam — accepts any pgxDB (e.g. pgxmock).
func newStore(pool pgxDB) *Store { return &Store{pool: pool} }

// Sentinel + validation errors.
var (
	ErrInvalidScope       = errors.New("budgets: scope must be one of workspace, team, sprint")
	ErrInvalidEnforcement = errors.New("budgets: enforcement must be one of off, alert, hard_block")
	ErrInvalidPeriod      = errors.New("budgets: period must be one of monthly, weekly, total")
	ErrScopeIDRequired    = errors.New("budgets: scope_id required for team/sprint scope")
	ErrInvalidThreshold   = errors.New("budgets: alert thresholds must be in (0, 1]")
	ErrWorkspaceRequired  = errors.New("budgets: workspace_id required")
	ErrNegativeLimit      = errors.New("budgets: limit_usd must be >= 0")
	ErrNotFound           = errors.New("budgets: not found")
	ErrScopeTaken         = errors.New("budgets: a budget for this scope already exists")
	ErrUnknownScope       = errors.New("budgets: unknown scope, so no spend is summed without a scope predicate")
)

// IsInvalid reports whether err is a budget the caller sent that the store
// refused — a message the caller can act on. Any other store error is the
// server's, and its text (the database's own words) is not for the caller.
func IsInvalid(err error) bool {
	for _, e := range []error{ErrInvalidScope, ErrInvalidEnforcement, ErrInvalidPeriod, ErrScopeIDRequired,
		ErrInvalidThreshold, ErrWorkspaceRequired, ErrNegativeLimit} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// pgCode is the SQLSTATE of a Postgres error, or "".
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func validScope(s Scope) bool {
	return s == ScopeWorkspace || s == ScopeTeam || s == ScopeSprint
}

func validEnforcement(e Enforcement) bool {
	return e == EnforcementOff || e == EnforcementAlert || e == EnforcementHardBlock
}

func validPeriod(p string) bool {
	return p == "monthly" || p == "weekly" || p == "total"
}

// Validate checks a budget for create/update. ScopeID for the workspace
// scope is normalized to the workspace id by the caller (Create), so it's
// only required for team/sprint here.
func (b *Budget) Validate() error {
	if strings.TrimSpace(b.WorkspaceID) == "" {
		return ErrWorkspaceRequired
	}
	if !validScope(b.Scope) {
		return ErrInvalidScope
	}
	if (b.Scope == ScopeTeam || b.Scope == ScopeSprint) && strings.TrimSpace(b.ScopeID) == "" {
		return ErrScopeIDRequired
	}
	if b.Enforcement == "" {
		b.Enforcement = EnforcementAlert
	}
	if !validEnforcement(b.Enforcement) {
		return ErrInvalidEnforcement
	}
	if b.Period == "" {
		b.Period = "monthly"
	}
	if !validPeriod(b.Period) {
		return ErrInvalidPeriod
	}
	if b.LimitUSD < 0 {
		return ErrNegativeLimit
	}
	for _, t := range b.AlertThresholds {
		if t <= 0 || t > 1 {
			return ErrInvalidThreshold
		}
	}
	return nil
}

const insertBudgetSQL = `
INSERT INTO budgets
  (workspace_id, scope, scope_id, period, limit_usd, alert_thresholds, enforcement, starts_at, ends_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, spent_usd, created_at, updated_at`

// Create validates + inserts a budget. For the workspace scope, scope_id is
// normalized to the workspace id so the (workspace_id, scope, scope_id)
// lookup key is well-defined.
func (s *Store) Create(ctx context.Context, b Budget) (*Budget, error) {
	if b.Scope == ScopeWorkspace {
		b.ScopeID = b.WorkspaceID
	}
	if len(b.AlertThresholds) == 0 {
		b.AlertThresholds = []float64{0.5, 0.8, 0.9}
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if s.pool == nil {
		return &b, nil
	}
	row := s.pool.QueryRow(ctx, insertBudgetSQL,
		b.WorkspaceID, string(b.Scope), b.ScopeID, b.Period, b.LimitUSD,
		b.AlertThresholds, string(b.Enforcement), b.StartsAt, b.EndsAt,
	)
	if err := row.Scan(&b.ID, &b.SpentUSD, &b.CreatedAt, &b.UpdatedAt); err != nil {
		if pgCode(err) == "23505" { // unique_violation on idx_budgets_scope
			return nil, ErrScopeTaken
		}
		return nil, fmt.Errorf("budgets: insert: %w", err)
	}
	return &b, nil
}

const budgetColumns = `id, workspace_id, scope, scope_id, period, limit_usd, spent_usd,
       alert_thresholds, enforcement, starts_at, ends_at, created_at, updated_at`

func scanBudget(row pgx.Row) (*Budget, error) {
	var b Budget
	var scope, enforcement string
	if err := row.Scan(
		&b.ID, &b.WorkspaceID, &scope, &b.ScopeID, &b.Period, &b.LimitUSD, &b.SpentUSD,
		&b.AlertThresholds, &enforcement, &b.StartsAt, &b.EndsAt, &b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return nil, err
	}
	b.Scope = Scope(scope)
	b.Enforcement = Enforcement(enforcement)
	return &b, nil
}

const getBudgetSQL = `SELECT ` + budgetColumns + ` FROM budgets WHERE id = $1 AND workspace_id = $2`

// Get returns the budget by id, scoped to workspace_id. Returns ErrNotFound
// when missing.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (*Budget, error) {
	if s.pool == nil {
		return nil, ErrNotFound
	}
	b, err := scanBudget(s.pool.QueryRow(ctx, getBudgetSQL, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("budgets: get: %w", err)
	}
	return b, nil
}

const listBudgetsSQL = `SELECT ` + budgetColumns + ` FROM budgets WHERE workspace_id = $1 ORDER BY scope, scope_id`

// List returns every budget for a workspace.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Budget, error) {
	if s.pool == nil {
		return nil, nil
	}
	return s.queryBudgets(ctx, listBudgetsSQL, workspaceID)
}

const activeBudgetsSQL = `SELECT ` + budgetColumns + ` FROM budgets
WHERE (ends_at IS NULL OR ends_at > NOW())
ORDER BY workspace_id, scope, scope_id`

// ActiveBudgets returns every budget that has not ended, across all
// workspaces — the set the service holds in memory.
func (s *Store) ActiveBudgets(ctx context.Context) ([]Budget, error) {
	if s.pool == nil {
		return nil, nil
	}
	return s.queryBudgets(ctx, activeBudgetsSQL)
}

func (s *Store) queryBudgets(ctx context.Context, sql string, args ...any) ([]Budget, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("budgets: query: %w", err)
	}
	defer rows.Close()
	var out []Budget
	for rows.Next() {
		b, err := scanBudget(rows)
		if err != nil {
			return nil, fmt.Errorf("budgets: scan: %w", err)
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// Patch is the change a PATCH asks for. A field left out (nil, or an unset
// EndsAt) keeps its stored value; an empty enforcement or period is treated as
// left out.
type Patch struct {
	LimitUSD        *float64     `json:"limit_usd"`
	AlertThresholds *[]float64   `json:"alert_thresholds"`
	Enforcement     *Enforcement `json:"enforcement"`
	Period          *string      `json:"period"`
	EndsAt          OptionalTime `json:"ends_at"`
}

// OptionalTime tells a time left out of a PATCH (Set false: keep the stored
// value) from an explicit null (Set true, Value nil: clear it).
type OptionalTime struct {
	Set   bool
	Value *time.Time
}

func (o *OptionalTime) UnmarshalJSON(data []byte) error {
	o.Set = true
	o.Value = nil
	if string(data) == "null" {
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(data, &t); err != nil {
		return err
	}
	o.Value = &t
	return nil
}

const updateBudgetSQL = `
UPDATE budgets SET
    limit_usd        = COALESCE($3::double precision, limit_usd),
    alert_thresholds = COALESCE($4::double precision[], alert_thresholds),
    enforcement      = COALESCE($5::text, enforcement),
    period           = COALESCE($6::text, period),
    ends_at          = CASE WHEN $7::boolean THEN $8::timestamptz ELSE ends_at END,
    updated_at       = NOW()
WHERE id = $1 AND workspace_id = $2
RETURNING ` + budgetColumns

// Update applies a Patch to a budget's mutable fields (limit, thresholds,
// enforcement, period, ends_at); a field the patch leaves out keeps its value.
// Scope/scope_id are immutable — create a new budget to re-scope. Returns
// ErrNotFound when the (id, workspace) pair is missing.
func (s *Store) Update(ctx context.Context, workspaceID, id string, p Patch) (*Budget, error) {
	var enforcement, period *string
	if p.Enforcement != nil && *p.Enforcement != "" {
		if !validEnforcement(*p.Enforcement) {
			return nil, ErrInvalidEnforcement
		}
		e := string(*p.Enforcement)
		enforcement = &e
	}
	if p.Period != nil && *p.Period != "" {
		if !validPeriod(*p.Period) {
			return nil, ErrInvalidPeriod
		}
		period = p.Period
	}
	if p.LimitUSD != nil && *p.LimitUSD < 0 {
		return nil, ErrNegativeLimit
	}
	var thresholds any // untyped nil → SQL NULL → keep
	if p.AlertThresholds != nil {
		for _, t := range *p.AlertThresholds {
			if t <= 0 || t > 1 {
				return nil, ErrInvalidThreshold
			}
		}
		th := *p.AlertThresholds
		if th == nil {
			th = []float64{}
		}
		thresholds = th
	}
	if s.pool == nil {
		return nil, ErrNotFound
	}
	out, err := scanBudget(s.pool.QueryRow(ctx, updateBudgetSQL,
		id, workspaceID, p.LimitUSD, thresholds, enforcement, period, p.EndsAt.Set, p.EndsAt.Value,
	))
	// A malformed id (invalid_text_representation for the uuid column) names
	// no budget, the same as a well-formed id that matches none.
	if errors.Is(err, pgx.ErrNoRows) || pgCode(err) == "22P02" {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("budgets: update: %w", err)
	}
	return out, nil
}

const deleteBudgetSQL = `DELETE FROM budgets WHERE id = $1 AND workspace_id = $2`

// Delete removes a budget. Idempotent: missing rows are not an error.
func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, deleteBudgetSQL, id, workspaceID); err != nil {
		return fmt.Errorf("budgets: delete: %w", err)
	}
	return nil
}

const updateSpentSQL = `UPDATE budgets SET spent_usd = $2, updated_at = NOW() WHERE id = $1 AND workspace_id = $3`

// UpdateSpent persists the reconciled running total for a budget, CONFINED to its owning
// workspace. Called during refresh, never on the request hot path. Keying the write on
// (id, workspace_id) — not id alone — makes it a money-path identity guard: a wrong or foreign
// budget id can never move another workspace's spend (SEC-11: a scoping key must hold every
// identity it protects). spent_usd is advisory USD, not conserved µLENS — no amount arithmetic
// changes here; only which row the write is allowed to touch.
func (s *Store) UpdateSpent(ctx context.Context, workspaceID, id string, spent float64) error {
	if s.pool == nil || id == "" {
		return nil
	}
	if _, err := s.pool.Exec(ctx, updateSpentSQL, id, spent, workspaceID); err != nil {
		return fmt.Errorf("budgets: update spent: %w", err)
	}
	return nil
}

// ScopeColumn returns the token_events column that identifies a scope for
// spend aggregation, and whether the scope is one this mapping KNOWS.
// Centralized so reconciliation here and the forecasting reads
// (internal/forecast) agree on the mapping rather than each hard-coding the
// switch.
//
// ⚠ THE TWO RETURN VALUES ARE NOT REDUNDANT. The workspace scope is known and
// deliberately has NO column — workspace_id is already the predicate — so it
// answers ("", true). An UNKNOWN scope answers ("", false). Before this split
// both cases were the single answer "", and a caller writing `if col != ""`
// silently dropped the predicate for an unknown scope: the query then summed
// the whole workspace and returned it as that scope's spend. Callers MUST
// branch on ok, never on col being empty.
func ScopeColumn(scope Scope) (col string, ok bool) {
	switch scope {
	case ScopeWorkspace:
		return "", true
	case ScopeTeam:
		return "team", true
	case ScopeSprint:
		return "sprint_id", true
	default:
		return "", false
	}
}

// periodWindow returns the SQL predicate restricting token_events to the
// budget's period. monthly/weekly use calendar boundaries; total uses the
// budget's [starts_at, ends_at) window when set, otherwise all time.
//
// The calendar definitions here (monthly = date_trunc('month'), weekly =
// date_trunc('week') = Monday) are mirrored in Go by PeriodBounds; keep the
// two in sync.
func periodWindow(b Budget) string {
	switch b.Period {
	case "weekly":
		return "created_at >= date_trunc('week', NOW())"
	case "total":
		return "TRUE"
	default: // monthly
		return "created_at >= date_trunc('month', NOW())"
	}
}

// ReconcileSpent sums realized spend for a budget's scope + period directly
// from token_events — the single billing source of truth. Workspace-scope
// budgets sum by workspace_id (now correctly populated; see migration
// 0028), team by team, sprint by sprint_id.
func (s *Store) ReconcileSpent(ctx context.Context, b Budget) (float64, error) {
	// Fail CLOSED on a scope this mapping does not know, BEFORE the no-database
	// short circuit — an unknown scope is a caller error either way, and a
	// refusal that only happens when a pool is configured is a refusal that
	// tests cannot reach. Falling through to "no predicate" would sum the
	// ENTIRE workspace and return it as this scope's spend, silently and with a
	// nil error, which on a hard_block budget rejects every request in the
	// workspace.
	col, known := ScopeColumn(b.Scope)
	if !known {
		return 0, fmt.Errorf("%w: %q", ErrUnknownScope, b.Scope)
	}
	if s.pool == nil {
		return b.SpentUSD, nil
	}
	where := "workspace_id = $1"
	args := []any{b.WorkspaceID}
	if col != "" {
		where += fmt.Sprintf(" AND %s = $%d", col, len(args)+1)
		args = append(args, b.ScopeID)
	}
	if b.Period == "total" && b.StartsAt != nil {
		where += fmt.Sprintf(" AND created_at >= $%d", len(args)+1)
		args = append(args, *b.StartsAt)
		if b.EndsAt != nil {
			where += fmt.Sprintf(" AND created_at < $%d", len(args)+1)
			args = append(args, *b.EndsAt)
		}
	} else {
		where += " AND " + periodWindow(b)
	}
	q := "SELECT COALESCE(SUM(cost_usd), 0) FROM token_events WHERE " + where
	var spent float64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&spent); err != nil {
		return 0, fmt.Errorf("budgets: reconcile: %w", err)
	}
	return spent, nil
}
