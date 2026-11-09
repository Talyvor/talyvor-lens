// sweeper.go — the Stage-2.3a finalize sweeper: settles due held mints
// (held → spendable) after the holdback window.
//
// Mirrors the povi-challenge scheduler exactly: registered in main under
// haComps.leader.Run("pool-royalty-finalize", ...) — leader-elected via
// Redis SetNX when HA is on, direct-run when off — ticking on a minute
// scale. Registered UNCONDITIONALLY (never gated on the minting flag):
// committed held rows must finalize on schedule even if minting is later
// disabled, or contributor LENS strands in held forever.
//
// Each due row settles in its OWN single transaction, claim-first:
//
//	(1) CAS: UPDATE pool_royalty_mints SET status='final'
//	         WHERE request_id=$1 AND status='held'
//	    RowsAffected()==0 → another sweeper already settled it (HA failover
//	    overlap) → roll back and skip BEFORE touching any balance. Double-
//	    finalize is impossible by this guard — the same claim/RowsAffected
//	    discipline as povi_challenges, the 2.1 mint claim, and the 2.2
//	    RecordReceipt fix.
//	(2) FinalizeHeldTxAs: the single-row FOR UPDATE held→spendable move, which
//	    writes the counted ledger row for THIS table's mint (finalTypeForTable) —
//	    the moment the mint enters supply.
//
// Two single-row writes in one tx, no cross-row ordering surface (each claim
// row is only ever touched by its own request_id-keyed transition; distinct
// claim rows mean no lock cycle is constructible). Plain parameterized SQL —
// PgBouncer transaction-pooling safe.
//
// The settlement TRIGGER is decoupled from the ledger ops by design: this
// timed sweeper is the initial trigger; billing settlement can replace it
// later by calling the same per-row settle without schema or kernel changes.
package poolroyalty

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
)

// sweepBatchLimit bounds one tick's settle work. NOT a silent cap: rows past
// the limit are simply settled on the next minute tick, and RunOnce logs
// when a full batch suggests more are waiting.
const sweepBatchLimit = 500

// sweepSelectSQLFor / finalizeCASSQLFor build the table-scoped settlement SQL.
// The table is a TRUSTED internal constant (NewFinalizeSweeper's caller passes a
// hardcoded name — "pool_royalty_mints" or "distill_royalty_mints" — never user
// input), so the fmt.Sprintf interpolation is injection-safe. Both tables expose
// the generic (request_id, contributor_workspace_id, minted_amount, status,
// finalize_after) finalize columns the kernel reads; the partial index
// idx_<table>_finalize (finalize_after) WHERE status='held' backs the SELECT.
// settleStatus is the row status the sweeper settles FROM: 'held' by default
// (today's behavior, byte-identical) or 'cleared' when the Phase-3 fail-closed
// layer is armed (only adjudicated-clean rows settle). Both table and status are
// TRUSTED internal literals (never user input) — see SetSettleStatus's whitelist.
func sweepSelectSQLFor(table, settleStatus string) string {
	return fmt.Sprintf(`SELECT request_id, contributor_workspace_id, minted_amount
FROM %s
WHERE status = '%s' AND finalize_after < now()
LIMIT %d`, table, settleStatus, sweepBatchLimit)
}

func finalizeCASSQLFor(table, settleStatus string) string {
	return fmt.Sprintf(`UPDATE %s SET status = 'final' WHERE request_id = $1 AND status = '%s'`, table, settleStatus)
}

type sweeperDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// heldFinalizer is the minimal settle surface; *mining.LedgerStore's
// FinalizeHeldTxAs satisfies it exactly.
//
// Deliberately FinalizeHeldTxAs, not FinalizeHeldTx: the latter hardcodes
// mining.TypePoolRoyalty, and this sweeper settles SIX different claim tables. Taking the
// type-less overload is what let every settled mint land labelled pool_royalty regardless
// of what it actually was. The finalType must be supplied per table — see finalTypeForTable.
type heldFinalizer interface {
	FinalizeHeldTxAs(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, finalType, description string, metadata map[string]interface{}) error
}

// finalTypeForTable maps each claim TABLE this sweeper settles to the COUNTED ledger type
// its settled row must carry. The sweeper is parameterized BY TABLE, so the table IS the
// mint's identity — this map is the ONLY place that identity becomes a ledger label.
//
// Each entry pairs with the held type its minter writes at the mint moment (final + "_held"
// == held), and every value is counted in mining.GetTotalSupply — so settling attributes a
// mint honestly WITHOUT moving a µLENS in or out of supply.
//
//	pool_royalty_mints         pool_royalty_held              → pool_royalty
//	distill_royalty_mints      pool_royalty_held              → pool_royalty  (see below)
//	eval_contribution_mints    eval_contribution_held         → eval_contribution
//	routing_prediction_mints   eval_routing_prediction_held   → eval_routing_prediction
//	node_latency_mints         eval_latency_locality_held     → eval_latency_locality
//	confidential_compute_mints eval_confidential_compute_held → eval_confidential_compute
//
// distill shares pool_royalty by DESIGN, not by the old bug: NewDistillMinter reuses the
// Pool-B held kernel and mints TypePoolRoyaltyHeld, so its held row and settled row agree.
// Giving distill its own final type would need its HELD type changed too — the mint moment,
// i.e. the money path.
//
// A table absent from this map is a mint whose settled label nobody decided. That is
// FAIL-CLOSED here (settleOne refuses; the row stays held and is retried) rather than
// silently mislabelled — stranding is visible and recoverable, mislabelled money is neither.
// TestFinalTypeForTable_CoversEveryWiredSweeper pins main.go's tables against these keys.
var finalTypeForTable = map[string]string{
	"pool_royalty_mints":         mining.TypePoolRoyalty,
	"distill_royalty_mints":      mining.TypePoolRoyalty,
	"eval_contribution_mints":    mining.TypeEvalContribution,
	"routing_prediction_mints":   mining.TypeRoutingPrediction,
	"node_latency_mints":         mining.TypeLatencyLocality,
	"confidential_compute_mints": mining.TypeConfidentialCompute,
}

// FinalizeSweeper settles due held mints (Stage 2.3a) for ONE claim table. The
// zero/nil sweeper is inert. Parameterized by table so the SAME kernel finalizes
// both pool_royalty_mints (cache royalty) and distill_royalty_mints (L2/S4) — the
// finalize logic reads only generic columns, so one implementation serves both.
type FinalizeSweeper struct {
	db           sweeperDB
	ledger       heldFinalizer
	table        string
	finalType    string // the COUNTED ledger type this table's settled rows carry
	settleStatus string // 'held' (default) or 'cleared' (Phase-3 fail-closed)
	selectSQL    string
	casSQL       string
}

// NewFinalizeSweeper wires the pool, the held ledger, and the claim TABLE to
// settle (a trusted internal constant — e.g. "pool_royalty_mints" or
// "distill_royalty_mints"; empty defaults to pool_royalty_mints).
//
// The table also selects the settled row's ledger TYPE via finalTypeForTable: the sweeper
// settles six different mints, so its own table is the only thing that knows which one this
// is. An unmapped table leaves finalType empty and settleOne refuses (fail-closed).
func NewFinalizeSweeper(db sweeperDB, ledger heldFinalizer, table string) *FinalizeSweeper {
	if table == "" {
		table = "pool_royalty_mints"
	}
	return &FinalizeSweeper{
		db:           db,
		ledger:       ledger,
		table:        table,
		finalType:    finalTypeForTable[table],
		settleStatus: "held",
		selectSQL:    sweepSelectSQLFor(table, "held"),
		casSQL:       finalizeCASSQLFor(table, "held"),
	}
}

// SetSettleStatus arms the Phase-3 Item 3 fail-closed layer. status='cleared'
// makes the sweeper settle ONLY adjudicated-clean rows (an un-adjudicated 'held'
// row never finalizes → fail-closed); status='held' is the default (byte-identical
// to pre-Phase-3). Both values are interpolated into SQL, so this WHITELIST is
// load-bearing: any other value is ignored (the status is a trusted internal
// literal from main.go, never user input — the whitelist is defense-in-depth).
func (s *FinalizeSweeper) SetSettleStatus(status string) {
	if s == nil || (status != "held" && status != "cleared") {
		return
	}
	s.settleStatus = status
	s.selectSQL = sweepSelectSQLFor(s.table, status)
	s.casSQL = finalizeCASSQLFor(s.table, status)
}

type dueMint struct {
	requestID   string
	contributor string
	amount      int64 // µLENS (SEC-2: minted_amount is BIGINT)
}

// RunOnce sweeps due held rows and settles each in its own CAS-guarded tx.
// Returns the number finalized. Per-row failures are logged and skipped (the
// row stays 'held' and retries next tick); only the sweep SELECT itself can
// error the call.
func (s *FinalizeSweeper) RunOnce(ctx context.Context) (int, error) {
	if s == nil || s.db == nil || s.ledger == nil {
		return 0, nil
	}
	rows, err := s.db.Query(ctx, s.selectSQL)
	if err != nil {
		return 0, err
	}
	due := make([]dueMint, 0, 16)
	for rows.Next() {
		var d dueMint
		if err := rows.Scan(&d.requestID, &d.contributor, &d.amount); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	finalized := 0
	for _, d := range due {
		if err := s.settleOne(ctx, d); err != nil {
			if _, lost := err.(casLost); lost {
				continue // normal HA overlap — the other sweeper settled it
			}
			slog.Warn("poolroyalty: finalize failed (row stays held; retries next tick)",
				slog.String("request_id", d.requestID),
				slog.String("contributor", d.contributor),
				slog.String("error", err.Error()),
			)
			continue
		}
		finalized++
	}
	if len(due) == sweepBatchLimit {
		slog.Info("poolroyalty: finalize sweep hit batch limit — more due rows settle next tick",
			slog.Int("batch", sweepBatchLimit))
	}
	return finalized, nil
}

// errCASLost is internal: another sweeper settled the row first.
type casLost struct{}

func (casLost) Error() string { return "finalize CAS lost (already settled)" }

// settleOne settles a single due mint: CAS-claim the row, then move
// held→spendable, in one transaction. A lost CAS is a silent skip (not an
// error and not a finalize).
func (s *FinalizeSweeper) settleOne(ctx context.Context, d dueMint) error {
	// Fail-closed: without a decided settled type this row could only be labelled by a
	// guess, and a mislabelled settle is silent and permanent. Refuse BEFORE the CAS, so
	// the row stays held (visible, retried, recoverable) instead of settling as a lie.
	if s.finalType == "" {
		return fmt.Errorf("poolroyalty: no settled ledger type mapped for table %q, so it refuses to finalize (add it to finalTypeForTable)", s.table)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, s.casSQL, d.requestID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Another sweeper (HA overlap) already settled it — skip before
		// touching any balance. The deferred rollback ends the tx.
		return casLost{}
	}
	meta := map[string]interface{}{"request_id": d.requestID}
	// FinalizeHeldTxAs with THIS table's own type — the settled row is attributed to the
	// mint it actually came from. Amount, timing and the held→spendable move are untouched:
	// only the label is parameterized.
	if err := s.ledger.FinalizeHeldTxAs(ctx, tx, d.contributor, d.amount, s.finalType,
		s.finalType+" finalized (holdback window elapsed)", meta); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// The mint enters circulation NOW (this table's counted row was just committed) —
	// this is where the supply counter agrees with SQL.
	metrics.MintedTokens(microToFloatLENS(d.amount))
	slog.Info("poolroyalty: held mint finalized (held → spendable)",
		slog.String("request_id", d.requestID),
		slog.String("contributor", d.contributor),
		slog.String("type", s.finalType),
		slog.Int64("amount_ulens", d.amount),
	)
	return nil
}

// StartScheduler ticks RunOnce until ctx ends — mirrors
// povi.ChallengeScheduler.StartScheduler.
func (s *FinalizeSweeper) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.RunOnce(ctx); err != nil {
				slog.Warn("poolroyalty: finalize sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
