package poolroyalty

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// IdentityDB is what LoadIdentityGraph reads the identity edges from: a pool, a connection or a transaction.
type IdentityDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadIdentityGraph reads the identity edges the ring detector judges royalties by — a shared card fingerprint or
// a shared owner key — into a graph, so another measure (B63.1, the payments pool) rules out the same operators.
func LoadIdentityGraph(ctx context.Context, db IdentityDB) (*IdentityGraph, error) {
	return (&RingDetector{db: db}).loadIdentityGraph(ctx)
}
