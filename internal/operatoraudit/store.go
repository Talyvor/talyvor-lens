// Package operatoraudit is B27.28's operator audit trail: one row per action an operator took in the
// web app — who, what, on which target, and when — appended through POST /v1/admin/operator-audit/record
// and read back by operators, filtered, as JSON or as a CSV export.
//
// The table is append-only at the database level (migration 0182): 0055's audit_block_mutation trigger
// refuses every UPDATE, DELETE and TRUNCATE. Nothing in this package updates or deletes a row.
package operatoraudit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/redact"
)

const (
	// defaultLimit is how many entries a list returns when it does not say.
	defaultLimit = 200
	// MaxLimit caps a list, and is the size of the CSV export.
	MaxLimit = 50000

	maxNameLen   = 200
	maxTargetLen = 500
	maxDetailLen = 4000

	// An entry's own time may trail the clock by a day (a retried write) and lead it by a minute of
	// skew, no more: a trail whose times a writer could set freely would let an action be backdated.
	// recorded_at is always the database's clock.
	maxBackdate = 24 * time.Hour
	maxSkew     = time.Minute
)

// ErrInvalid is an entry or filter the trail will not take; the message says why.
var ErrInvalid = errors.New("operatoraudit: invalid")

// Entry is one recorded operator action.
type Entry struct {
	ID         int64     `json:"id"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	Detail     string    `json:"detail"`
	OccurredAt time.Time `json:"occurred_at"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Filter narrows a read. Empty strings and zero times do not filter; Until is exclusive.
type Filter struct {
	Actor  string
	Action string
	Target string
	Since  time.Time
	Until  time.Time
	Limit  int
}

// Store appends to and reads operator_audit.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Record appends e and returns it as stored. Actor and action are required; a zero OccurredAt means now,
// and a set one must fall within the last 24 hours.
func (s *Store) Record(ctx context.Context, e Entry) (Entry, error) {
	e, err := validate(e)
	if err != nil {
		return Entry{}, err
	}
	if s.pool == nil {
		return Entry{}, errors.New("operatoraudit: no database configured")
	}
	return insert(ctx, s.pool, e)
}

// validate trims e and refuses an entry the trail will not take.
func validate(e Entry) (Entry, error) {
	e.Actor = strings.TrimSpace(e.Actor)
	e.Action = strings.TrimSpace(e.Action)
	e.Target = strings.TrimSpace(e.Target)
	e.Detail = strings.TrimSpace(e.Detail)
	switch {
	case e.Actor == "":
		return Entry{}, invalid("actor is required: the operator who acted")
	case e.Action == "":
		return Entry{}, invalid("action is required: what the operator did")
	case len(e.Actor) > maxNameLen:
		return Entry{}, invalid("actor is longer than %d bytes", maxNameLen)
	case len(e.Action) > maxNameLen:
		return Entry{}, invalid("action is longer than %d bytes", maxNameLen)
	case len(e.Target) > maxTargetLen:
		return Entry{}, invalid("target is longer than %d bytes", maxTargetLen)
	case len(e.Detail) > maxDetailLen:
		return Entry{}, invalid("detail is longer than %d bytes", maxDetailLen)
	case !e.OccurredAt.IsZero() && e.OccurredAt.After(time.Now().Add(maxSkew)):
		return Entry{}, invalid("at is in the future")
	case !e.OccurredAt.IsZero() && e.OccurredAt.Before(time.Now().Add(-maxBackdate)):
		return Entry{}, invalid("at is more than 24 hours ago; record an action when it happens")
	}
	return e, nil
}

// queryRower is a pool or a transaction.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RecordIn is Record inside the caller's transaction (B32.10): an operator's change and its row in the trail
// commit together, or neither does.
func RecordIn(ctx context.Context, tx pgx.Tx, e Entry) (Entry, error) {
	e.OccurredAt = time.Time{}
	e, err := validate(e)
	if err != nil {
		return Entry{}, err
	}
	return insert(ctx, tx, e)
}

func insert(ctx context.Context, q queryRower, e Entry) (Entry, error) {
	var at any
	if !e.OccurredAt.IsZero() {
		at = e.OccurredAt
	}
	err := q.QueryRow(ctx, `
		INSERT INTO operator_audit (actor, action, target, detail, occurred_at)
		VALUES ($1, $2, $3, $4, COALESCE($5::timestamptz, now()))
		RETURNING id, occurred_at, recorded_at`,
		e.Actor, e.Action, redact.Secrets(e.Target), redact.Secrets(e.Detail), at,
	).Scan(&e.ID, &e.OccurredAt, &e.RecordedAt)
	if err != nil {
		return Entry{}, fmt.Errorf("operatoraudit: record: %w", err)
	}
	return e, nil
}

// List returns the entries f selects, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Entry, error) {
	if f.Limit <= 0 {
		f.Limit = defaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if s.pool == nil {
		return nil, errors.New("operatoraudit: no database configured")
	}
	var since, until any
	if !f.Since.IsZero() {
		since = f.Since
	}
	if !f.Until.IsZero() {
		until = f.Until
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, actor, action, target, detail, occurred_at, recorded_at
		FROM operator_audit
		WHERE ($1 = '' OR actor = $1)
		  AND ($2 = '' OR action = $2)
		  AND ($3 = '' OR target = $3)
		  AND ($4::timestamptz IS NULL OR occurred_at >= $4)
		  AND ($5::timestamptz IS NULL OR occurred_at < $5)
		ORDER BY occurred_at DESC, id DESC
		LIMIT $6`,
		strings.TrimSpace(f.Actor), strings.TrimSpace(f.Action), strings.TrimSpace(f.Target), since, until, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("operatoraudit: list: %w", err)
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.OccurredAt, &e.RecordedAt); err != nil {
			return nil, fmt.Errorf("operatoraudit: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operatoraudit: list: %w", err)
	}
	return out, nil
}
