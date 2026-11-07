package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// retentionTable is HARDCODED. There is deliberately NO table knob: the supply
// ledgers (lens_token_ledger, lxc_ledger) must NEVER be retention-swept
// (GetTotalSupply sums their full history), so the only deletable audit table is
// token_events. A config can never point this sweeper at a ledger.
const retentionTable = "token_events"

// retentionBatch bounds the rows deleted per transaction, so a large purge on the
// highest-volume table can't take a long lock or bloat WAL/vacuum in one shot.
const retentionBatch = 5000

// Delete queries for the sweeper. The export-gated variant (U14 #187) additionally
// bounds the delete by the off-box export watermark: a row is removed only once its
// created_at is at/below last_exported_at — proof-of-export-before-delete.
//
// ⚠⚠ THE VICTIM SET IS KEYED ON THE PRIMARY KEY AND MUST NEVER GO BACK TO `ctid`.
// Both statements used to read `WHERE ctid IN (SELECT ctid FROM token_events …)`.
// ctid is unique within a PHYSICAL RELATION, and migration 0034 made this table 8
// HASH partitions on workspace_id — the first row of every partition is (0,1) — so
// the inner SELECT nominated ctids from one partition and the outer DELETE matched
// them in ALL of them. An aged row in p4 therefore deleted a BRAND-NEW row in p1,
// with no error and no log line, on the table the 0055 append-only triggers exist
// to make unrewritable, through the one door this sweeper holds the only key to.
// Under the export-gated variant it destroyed rows that had not been exported
// off-box, which is exactly what proof-of-export-before-delete promises to keep.
//
// (id, workspace_id) is 0034's declared PRIMARY KEY, so it is unique across the
// whole partitioned table by construction rather than by luck of layout — and
// because workspace_id is the partition key, the planner still prunes to one
// partition per candidate row. Measured, not reasoned about:
// TestRetention_DoesNotSweepAcrossPartitionsOnACollidingCtid builds the collision,
// asserts it really built it, and fails if a fresh row is swept.
const (
	deleteAgedSQL = `DELETE FROM ` + retentionTable +
		` WHERE (id, workspace_id) IN (SELECT id, workspace_id FROM ` + retentionTable +
		` WHERE created_at < $1 LIMIT $2)`
	deleteAgedExportedSQL = `DELETE FROM ` + retentionTable +
		` WHERE (id, workspace_id) IN (SELECT id, workspace_id FROM ` + retentionTable +
		` WHERE created_at < $1 AND created_at <= $2 LIMIT $3)`
)

// Retention is the token_events-only retention sweeper. It is the ONLY sanctioned
// deleter of audit rows: each batch runs `SET LOCAL lens.audit_retention = 'on'`
// so the 0055 trigger permits the DELETE (a stray DELETE without the flag is still
// blocked). UPDATE/TRUNCATE remain blocked even here.
//
// U14 #187: when requireExport is on, the delete is additionally bounded by the
// off-box export watermark so retention can never prune a row that has not yet been
// exported off-box.
type Retention struct {
	pool          *pgxpool.Pool
	window        time.Duration
	batch         int
	requireExport bool // U14 #187: prune only at/below the export watermark
	exportEnabled bool // whether off-box export is configured (LENS_AUDIT_EXPORT_URL set)
	log           *slog.Logger
}

// NewRetention builds the sweeper. window <= 0 disables it.
//
// requireExport (U14 #187, default false) gates each delete behind the off-box
// export watermark so no un-exported row is ever pruned. exportEnabled reports
// whether the off-box export loop is configured: when requireExport is on and
// export is OFF, the sweep is skipped entirely (rather than pruning rows that can
// never be proven exported). With requireExport off, the sweeper behaves exactly
// as before — age-only pruning, no watermark read.
func NewRetention(pool *pgxpool.Pool, window time.Duration, requireExport, exportEnabled bool) *Retention {
	return &Retention{
		pool:          pool,
		window:        window,
		batch:         retentionBatch,
		requireExport: requireExport,
		exportEnabled: exportEnabled,
		log:           slog.Default(),
	}
}

// SweepOnce deletes token_events rows older than the window in batches, returning
// the total deleted. window <= 0 is a no-op (disabled).
//
// When requireExport is on (U14 #187), the delete is additionally bounded by the
// off-box export watermark, read ONCE here and applied to every batch: a row is
// pruned only when created_at <= last_exported_at, so a row that has not yet been
// exported is KEPT even if it is already past the retention window. Reading the
// watermark once is safe by direction — it only advances, so a slightly-stale value
// prunes LESS, never more. If export is disabled, the sweep is skipped entirely.
func (r *Retention) SweepOnce(ctx context.Context) (int64, error) {
	if r.window <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-r.window)

	// nil ⇒ age-only (default, unchanged behaviour); non-nil ⇒ also bound the delete
	// by the export watermark (proof-of-export-before-delete).
	var upper *time.Time
	if r.requireExport {
		if !r.exportEnabled {
			// Proof-of-export is demanded but nothing exports off-box: pruning now could
			// destroy un-exported audit history. Skip (and warn) rather than prune.
			r.log.Warn("audit retention SKIPPED: require-export-before-prune is on but off-box export is disabled " +
				"(set LENS_AUDIT_EXPORT_URL, or unset LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE to allow age-only pruning)")
			return 0, nil
		}
		wm, err := r.readWatermark(ctx)
		if err != nil {
			return 0, fmt.Errorf("audit retention: read export watermark: %w", err)
		}
		upper = &wm
	}

	var total int64
	for {
		n, err := r.deleteBatch(ctx, cutoff, upper)
		total += n
		if err != nil {
			return total, err
		}
		if n < int64(r.batch) {
			return total, nil // last (partial) batch drained the backlog
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// readWatermark returns the off-box export high-water mark — the created_at upper
// bound of the last successfully-exported window (audit_export_state, migration
// 0056). The singleton row is seeded at epoch, so before any successful export this
// is 1970, which prunes nothing (the safe default for an enabled-but-not-yet-caught-
// up export loop).
func (r *Retention) readWatermark(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := r.pool.QueryRow(ctx, `SELECT last_exported_at FROM audit_export_state WHERE id = true`).Scan(&t)
	return t, err
}

func (r *Retention) deleteBatch(ctx context.Context, cutoff time.Time, upper *time.Time) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Scoped, transaction-local flag — the ONLY thing the trigger accepts as a
	// sanctioned delete. SET LOCAL does not leak past COMMIT/ROLLBACK.
	if _, err := tx.Exec(ctx, `SET LOCAL lens.audit_retention = 'on'`); err != nil {
		return 0, fmt.Errorf("audit retention: set flag: %w", err)
	}
	query, args := deleteAgedSQL, []any{cutoff, r.batch}
	if upper != nil {
		query, args = deleteAgedExportedSQL, []any{cutoff, *upper, r.batch}
	}
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("audit retention: delete batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("audit retention: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteWorkspaceEvents deletes every token_events row of workspaceID inside tx, under the same flag, set and then
// cleared within the statement's reach: the second sanctioned delete, for a person who deletes their account (B52.37).
func DeleteWorkspaceEvents(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	if _, err := tx.Exec(ctx, `SET LOCAL lens.audit_retention = 'on'`); err != nil {
		return fmt.Errorf("audit: set flag: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM token_events WHERE workspace_id = $1`, workspaceID); err != nil {
		return fmt.Errorf("audit: delete the workspace's events: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lens.audit_retention = 'off'`); err != nil {
		return fmt.Errorf("audit: clear flag: %w", err)
	}
	return nil
}

// StartSweeper runs SweepOnce on a fixed interval until ctx is cancelled. A
// non-positive window does NOT start the loop (default-off). BLOCKS until ctx is
// done — intended to run under the HA leader (so exactly one instance sweeps the
// highest-volume table). Mirrors the semantic-cache DeleteStale/StartSweeper pattern.
func (r *Retention) StartSweeper(ctx context.Context, interval time.Duration) {
	if r.window <= 0 {
		r.log.Info("audit retention sweeper disabled (LENS_AUDIT_RETENTION unset/<=0)")
		return
	}
	r.log.Info("audit retention sweeper started", "window", r.window.String(), "interval", interval.String(), "table", retentionTable)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := r.SweepOnce(ctx)
			switch {
			case err != nil:
				r.log.Warn("audit retention sweep failed", "err", err)
			case n > 0:
				r.log.Info("audit retention sweep", "deleted", n, "table", retentionTable)
			}
		}
	}
}
