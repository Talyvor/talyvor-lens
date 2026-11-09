package mining

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/metrics"
)

// Phase-1 / Phase-3 Item 1 — the holdback window for the CreditOnce traffic mints.
//
// The cache / compute / embedding mints previously landed DIRECTLY in spendable
// balance (no held state ⇒ no clawback surface). They now ALL route through the
// SAME proven held machinery pool-royalty uses: land HELD (uncounted) with a
// finalize window, settle held→spendable via a sweeper, and expose RevokeHeldTxAs
// for reversal before settlement (the clawback surface the anti-gaming layer needs).
// Cache was routed in Phase-1; compute + embedding — the NODE mints — in Phase-3
// (RecordServedRequest / RecordEmbeddingsServed → CreditOnceHeld), so when the node
// network lands, a node mint can never reach spendable un-adjudicated.

// heldTypeFor is the UNCOUNTED ledger type written at the held mint moment for a
// traffic mint whose COUNTED (final) type is mintType — e.g. "cache_mine" →
// "cache_mine_held". The _held type is in mintTypeList (gated + rate-capped at
// mint); the base type is written at finalize and counted in GetTotalSupply.
func heldTypeFor(mintType string) string { return mintType + "_held" }

// The held (mint-moment, uncounted) types for the CreditOnce traffic mints. Kept as
// constants so mintTypeList references them and TestMintTypeList pins the set.
const (
	TypeCacheMineHeld     = "cache_mine_held"
	TypeComputeMineHeld   = "compute_mine_held"
	TypeEmbeddingMineHeld = "embedding_mine_held"
	// TypePatternMineHeld is the held mint moment for the pattern mint (Phase-4a
	// Item 1: pattern was rerouted CreditTx→CreditHeldTx into traffic_mint_holds).
	// heldTypeFor(TypePatternMine) == this; the counted TypePatternMine is written
	// at finalize.
	TypePatternMineHeld = "pattern_mine_held"
	// TypeComputeFeeHeld is Talyvor's compute fee while the node mint it was taken from is held (B56.20). Not
	// a mint moment: its value is part of the gated compute_mine_held mint written in the same transaction.
	TypeComputeFeeHeld = "compute_fee_held"
)

const insertTrafficHoldSQL = `
    INSERT INTO traffic_mint_holds (request_id, workspace_id, mint_type, minted_amount, status, finalize_after)
    VALUES ($1, $2, $3, $4, 'held', now() + ($5::bigint * interval '1 microsecond'))
    ON CONFLICT (request_id, workspace_id, mint_type) DO NOTHING`

// CreditOnceHeld is the HELD, idempotent traffic mint (Phase-1 Item 1). Like
// CreditOnce it is exactly-once on (requestID, workspaceID, mintType) and
// fail-closed on an empty requestID — but the credit lands in HELD balance (not
// spendable): it claims a traffic_mint_holds row with a finalize_after window and
// writes the UNCOUNTED <mintType>_held ledger row. It becomes spendable only when
// the sweeper settles it. Claim + verified-earn gate + rate cap + held credit are
// ONE tx, so a duplicate or a gate-blocked mint leaves no partial state and does
// not consume the idempotency key.
func (s *LedgerStore) CreditOnceHeld(ctx context.Context, requestID, workspaceID string, amount int64, mintType, description string, window time.Duration, metadata map[string]interface{}) (alreadyMinted bool, err error) {
	return s.CreditOnceHeldWithFee(ctx, requestID, workspaceID, amount, mintType, description, window, metadata, "", 0, "", nil)
}

// CreditOnceHeldWithFee is CreditOnceHeld with Talyvor's fee taken out of the earning (B56.20): amount is what
// the earner is paid, and fee µLENS land held for feeWorkspace as feeType in the same transaction, under the
// same request and window, so the sweeper settles both and a gate-blocked mint books neither. fee 0 is
// CreditOnceHeld exactly.
func (s *LedgerStore) CreditOnceHeldWithFee(ctx context.Context, requestID, workspaceID string, amount int64, mintType, description string, window time.Duration, metadata map[string]interface{},
	feeWorkspace string, fee int64, feeType string, feeMetadata map[string]interface{}) (alreadyMinted bool, err error) {
	if amount <= 0 || fee < 0 {
		return false, fmt.Errorf("mining: credit amount must be positive")
	}
	if requestID == "" {
		return false, ErrNoMintRequestID
	}
	if s.pool == nil {
		return false, nil
	}
	if window <= 0 {
		window = 72 * time.Hour
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("mining: begin credit-once-held tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, insertTrafficHoldSQL, requestID, workspaceID, mintType, amount, window.Microseconds())
	if err != nil {
		return false, fmt.Errorf("mining: insert traffic hold claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return true, nil // already claimed — exactly-once suppression, nothing minted
	}
	// CreditHeldTx → heldInner runs the verified-earn gate + rate cap on the HELD
	// mint type (in mintTypeList). A gate block rolls the claim back with it.
	if err := s.CreditHeldTx(ctx, tx, workspaceID, amount, heldTypeFor(mintType), description, metadata); err != nil {
		return false, err
	}
	if fee > 0 {
		tag, err := tx.Exec(ctx, insertTrafficHoldSQL, requestID, feeWorkspace, feeType, fee, window.Microseconds())
		if err != nil {
			return false, fmt.Errorf("mining: insert fee hold claim: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return true, nil // the fee was already claimed for this request; the rollback undoes the earner's claim too
		}
		if err := s.CreditHeldTx(ctx, tx, feeWorkspace, fee, heldTypeFor(feeType), "Talyvor fee: "+description, feeMetadata); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("mining: commit credit-once-held: %w", err)
	}
	// No MintedTokens metric here — a held mint hasn't entered circulation; the
	// finalize sweeper records it at settlement (mirrors the pool-royalty held mint).
	return false, nil
}

// ─── finalize sweeper over traffic_mint_holds ─────────────────────────────────

// settleStatus is the status the sweeper settles FROM: 'held' by default (today's
// behavior, byte-identical) or 'cleared' when the Phase-4a fail-closed layer is
// armed (SetSettleStatus) — then an un-examined 'held' traffic row never settles.
// Both `status` literals are trusted internal constants, so the fmt.Sprintf is
// injection-safe (mirrors poolroyalty.sweeper).
func trafficSweepSelectSQLFor(settleStatus string) string {
	return fmt.Sprintf(`SELECT request_id, workspace_id, mint_type, minted_amount
FROM traffic_mint_holds
WHERE status = '%s' AND finalize_after < now()
ORDER BY finalize_after
LIMIT 500`, settleStatus)
}

func trafficSweepCASSQLFor(settleStatus string) string {
	return fmt.Sprintf(`UPDATE traffic_mint_holds SET status = 'final'
WHERE request_id = $1 AND workspace_id = $2 AND mint_type = $3 AND status = '%s'`, settleStatus)
}

// TrafficMintSweeper settles due held traffic mints (held → spendable), finalizing
// each to its OWN counted mint_type via FinalizeHeldTxAs. The CAS held→final makes
// concurrent sweeps settle each row exactly once — the same shape as the
// pool-royalty FinalizeSweeper, over traffic_mint_holds. A nil pool is a no-op.
type TrafficMintSweeper struct {
	pool         pgxDB
	ledger       *LedgerStore
	settleStatus string // 'held' (default) or 'cleared' (Phase-4a fail-closed)
}

func NewTrafficMintSweeper(pool pgxDB, ledger *LedgerStore) *TrafficMintSweeper {
	return &TrafficMintSweeper{pool: pool, ledger: ledger, settleStatus: "held"}
}

// SetSettleStatus arms the Phase-4a fail-closed layer for traffic mints: status
// 'cleared' makes the sweeper settle ONLY rows the single-party clearer examined
// and promoted; an un-examined held row never finalizes. 'held' (default) is
// byte-identical to pre-Phase-4a. Any other value is ignored (defense in depth).
func (s *TrafficMintSweeper) SetSettleStatus(status string) {
	if s == nil || (status != "held" && status != "cleared") {
		return
	}
	s.settleStatus = status
}

// RunOnce settles every currently-due held row and returns the count finalized.
func (s *TrafficMintSweeper) RunOnce(ctx context.Context) (int, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, trafficSweepSelectSQLFor(s.settleStatus))
	if err != nil {
		return 0, fmt.Errorf("mining: traffic sweep select: %w", err)
	}
	type due struct {
		requestID, workspaceID, mintType string
		amount                           int64
	}
	var dues []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.requestID, &d.workspaceID, &d.mintType, &d.amount); err != nil {
			rows.Close()
			return 0, fmt.Errorf("mining: traffic sweep scan: %w", err)
		}
		dues = append(dues, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, d := range dues {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return n, fmt.Errorf("mining: traffic finalize begin: %w", err)
		}
		tag, err := tx.Exec(ctx, trafficSweepCASSQLFor(s.settleStatus), d.requestID, d.workspaceID, d.mintType)
		if err != nil {
			_ = tx.Rollback(ctx)
			return n, fmt.Errorf("mining: traffic finalize CAS: %w", err)
		}
		if tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx) // another sweeper won it
			continue
		}
		meta := map[string]interface{}{"request_id": d.requestID, "traffic_hold": true}
		if err := s.ledger.FinalizeHeldTxAs(ctx, tx, d.workspaceID, d.amount, d.mintType,
			"traffic mint finalized (held → spendable)", meta); err != nil {
			_ = tx.Rollback(ctx)
			slog.Warn("mining: traffic finalize failed (row stays held; retries next tick)",
				slog.String("request_id", d.requestID), slog.String("err", err.Error()))
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			return n, fmt.Errorf("mining: traffic finalize commit: %w", err)
		}
		// The mint enters circulation NOW (the counted mint_type row was committed) —
		// emit MintedTokens here, not at the held mint moment (Phase-4a Item 1).
		metrics.MintedTokens(MicroToFloat(d.amount))
		slog.Info("mining: traffic mint finalized (held → spendable)",
			slog.String("workspace", d.workspaceID), slog.String("type", d.mintType), slog.Int64("amount_ulens", d.amount))
		n++
	}
	return n, nil
}

// StartScheduler runs RunOnce on an interval until ctx is done (leader-elected by
// the caller, like the pool-royalty finalize loop).
func (s *TrafficMintSweeper) StartScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.RunOnce(ctx); err != nil {
				slog.Warn("mining: traffic finalize sweep failed", slog.String("err", err.Error()))
			}
		}
	}
}
