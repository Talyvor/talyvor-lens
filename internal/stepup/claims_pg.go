package stepup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresClaims keeps the used steps in operator_step_up_codes (0234), so a code used on one replica is refused on
// every other, and is the Guesses too: the wrong-code count in operator_step_up_guesses.
type PostgresClaims struct{ pool *pgxpool.Pool }

// NewPostgresClaims keeps the used steps in pool.
func NewPostgresClaims(pool *pgxpool.Pool) *PostgresClaims { return &PostgresClaims{pool: pool} }

// Claim records step as used by operator, and answers whether it was the first use.
func (p *PostgresClaims) Claim(ctx context.Context, step int64, operator string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `INSERT INTO operator_step_up_codes (step, operator) VALUES ($1, $2) ON CONFLICT (step) DO NOTHING`,
		step, operator)
	if err != nil {
		return false, fmt.Errorf("stepup: claim: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Update runs fn on the wrong-code count in operator_step_up_guesses (B28.461) inside one transaction, the row locked,
// so replicas count one after another.
func (p *PostgresClaims) Update(ctx context.Context, fn func(State) State) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("stepup: guesses: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO operator_step_up_guesses (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING`); err != nil {
		return fmt.Errorf("stepup: guesses: %w", err)
	}
	var (
		s                    State
		windowStart, lockEnd *time.Time
	)
	if err := tx.QueryRow(ctx, `SELECT wrong, window_start, locked_until FROM operator_step_up_guesses WHERE id FOR UPDATE`).
		Scan(&s.Wrong, &windowStart, &lockEnd); err != nil {
		return fmt.Errorf("stepup: guesses: %w", err)
	}
	if windowStart != nil {
		s.WindowStart = *windowStart
	}
	if lockEnd != nil {
		s.LockedUntil = *lockEnd
	}
	n := fn(s)
	if _, err := tx.Exec(ctx, `UPDATE operator_step_up_guesses SET wrong = $1, window_start = $2, locked_until = $3 WHERE id`,
		n.Wrong, nullTime(n.WindowStart), nullTime(n.LockedUntil)); err != nil {
		return fmt.Errorf("stepup: guesses: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("stepup: guesses: %w", err)
	}
	return nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
