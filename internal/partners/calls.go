package partners

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// B37.5 — every call through a partner rail leaves one audit row: which service and method, for which workspace,
// under which of the caller's ids, how it ended and how long it took.

// Call is one call through a partner rail, as the audit keeps it. It holds no partner reference, account number,
// name or error text: Outcome is only ok, refused or failed.
type Call struct {
	At        time.Time
	Service   Service
	Method    string
	Workspace string // "" when the caller named none: an operator's or Lens's own call
	ID        string // the caller's idempotency id; "" for a read, whose argument is a partner's reference
	Outcome   string
	Duration  time.Duration
}

// The outcomes a Call records.
const (
	OutcomeOK      = "ok"
	OutcomeRefused = "refused" // the partner answered: the request was invalid, unknown or reused an id
	OutcomeFailed  = "failed"  // the partner did not answer
)

// outcome is how a call that returned err ended.
func outcome(err error) string {
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrNotFound), errors.Is(err, ErrIDReused):
		return OutcomeRefused
	}
	return OutcomeFailed
}

// CallLog keeps each Call: a CallStore.
type CallLog interface {
	LogCall(ctx context.Context, c Call)
}

type workspaceKey struct{}

// WithWorkspace is ctx naming the workspace a partner call is made for, as its audit row records it.
func WithWorkspace(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, workspaceKey{}, workspaceID)
}

// WorkspaceOf is the workspace ctx names (WithWorkspace): "" when it names none.
func WorkspaceOf(ctx context.Context) string {
	ws, _ := ctx.Value(workspaceKey{}).(string)
	return ws
}

// CallStore keeps each call in partner_calls (0238).
type CallStore struct{ pool *pgxpool.Pool }

// NewCallStore is the partner-call audit in pool.
func NewCallStore(pool *pgxpool.Pool) *CallStore { return &CallStore{pool: pool} }

// LogCall writes c. The call has happened by the time it is logged, so a write that fails is warned about and the
// call's answer stands.
func (s *CallStore) LogCall(ctx context.Context, c Call) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO partner_calls (at, service, method, workspace_id, idempotency_id, outcome, duration_us)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		c.At, string(c.Service), c.Method, c.Workspace, c.ID, c.Outcome, c.Duration.Microseconds()); err != nil {
		slog.Warn("partners: a partner call's audit row was not written",
			slog.String("service", string(c.Service)), slog.String("method", c.Method), slog.String("err", err.Error()))
	}
}
