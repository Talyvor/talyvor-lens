package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// B25.1 — THE WALL BETWEEN TEST MONEY AND REAL MONEY.
//
// A synthetic (test) workspace's money moves only to and from other synthetic workspaces, and a real
// workspace's only to and from real ones — never across. Every path that moves money between two
// workspaces calls CheckMoneyWall with both sides, inside its own transaction, before it writes a row:
// transfers and requests, loans, escrow, recurring transfers, marketplace purchases, LENS transfers and
// royalties. Every row test money writes carries the test mark (migration 0173), and the real figures leave
// it out: LENS supply (the backing value) and what a live payout run pays.

// ErrMoneyWall is the refusal. Its text names the rule, so whoever meets it knows why.
var ErrMoneyWall = errors.New("a test workspace and a real one never pay each other: a test workspace transacts only with other test workspaces, and a real workspace only with real ones")

// RowQuerier is a transaction, a connection or a pool.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// moneyWallSQL is true when exactly one of the two workspaces is synthetic. A workspace with no
// workspaces row (the legacy 'default' id) counts as real, as it does in Audience.SQL.
const moneyWallSQL = `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)
	<> COALESCE((SELECT synthetic FROM workspaces WHERE id = $2), false)`

// CheckMoneyWall returns ErrMoneyWall when money would cross between a test workspace and a real one,
// and nil when both are test or both are real. The flag is read from the database in the caller's
// transaction; it is never cleared, so the answer cannot change under it.
func CheckMoneyWall(ctx context.Context, q RowQuerier, a, b string) error {
	if a == b {
		return nil
	}
	var across bool
	if err := q.QueryRow(ctx, moneyWallSQL, a, b).Scan(&across); err != nil {
		return fmt.Errorf("workspace: check the test-workspace wall: %w", err)
	}
	if across {
		return ErrMoneyWall
	}
	return nil
}
