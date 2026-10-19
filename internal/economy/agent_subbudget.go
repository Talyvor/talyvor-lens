package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- retry jitter only, never an id, token or secret
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/metrics"
)

// agent_subbudget.go — F4-capstone step A: the per-scoped-key LXC sub-budget + the EXACTLY-ONCE agent debit.
// This is the substrate the closed-loop allocator (a later step) sits on. It MINTS NOTHING — it bounds and
// records SPENDING of already-existing LXC (workspace↔Talyvor pool), crediting no ledger and adding no mint
// type. CENTRAL-COUNTERPARTY + CLOSED-LOOP: LXC is debited from the workspace's lxc_balances via the SAME
// internals SpendLXC uses (readLXCBalance/insertLXCLedger/writeLXCBalance) — this file NEVER touches
// LedgerStore.Transfer (LENS P2P) or the marketplace (asserted by agent_subbudget_noloop_test.go).
//
// THE LOCK ORDER (B35.2). Every transaction that takes these rows takes them in this order, and only this order:
//
//  1. the request's own row: its lxc_reservations row (a hold inserts it; a settle or a release locks it), its
//     lxc_spend_claims row (a debit) or its agent_debit_settlements row (the debit's settle);
//  2. the key's agent_lxc_subbudgets row;
//  3. the agent's agent_accounts row (lockAgent), and with it the agent's stored balance and its company's
//     credit line;
//  4. the workspace's lxc_balances row (readLXCBalance).
//
// A step a transaction does not need is skipped, never reordered: an agent's funding, a move back to its
// workspace, a payment and every other agent movement take 3 then 4. Before B35.2 a settle and a release took 4 before 2 and 3 while a hold took 2, 3, 4,
// so a hold and a settle of one agent at the same moment deadlocked: Postgres cancelled one, a cancelled hold
// was refused with money in the wallet and a cancelled settle left the answer unbilled. The order makes that
// cycle impossible; retryLocks below runs again whatever Postgres still cancels.

// lockRetries is how many more times a hold, debit, settle or release runs after Postgres cancels it as a
// deadlock or a serialization failure. Each is idempotent under its reservation or request id, so running it
// again cannot hold or bill twice.
const lockRetries = 5

// ErrLockContention is a hold, debit, settle or release Postgres cancelled as a deadlock or a serialization
// failure on every one of its attempts. It is never a shortfall: nothing was held, debited or settled, and the
// same call may be made again.
var ErrLockContention = errors.New("economy: Lens could not take the locks for this movement")

// lockFailure reports whether Postgres cancelled the transaction as a deadlock (40P01) or a serialization
// failure (40001), which running it again resolves.
func lockFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// retryLocks runs one money transaction, and runs it again, after a short jittered wait, each time Postgres
// cancels it as a deadlock or a serialization failure, up to lockRetries more times. Any other outcome is
// returned as it is; the last cancellation is returned as ErrLockContention.
func retryLocks(ctx context.Context, run func() error) error {
	for attempt := 0; ; attempt++ {
		err := run()
		if !lockFailure(err) {
			return err
		}
		if attempt == lockRetries {
			return fmt.Errorf("%w after %d attempts: %w", ErrLockContention, attempt+1, err)
		}
		wait := time.Duration(5<<attempt) * time.Millisecond
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrLockContention, err)
		case <-time.After(wait + rand.N(wait)):
		}
	}
}

// lockKeyAndAgent takes steps 2 and 3 of the lock order for a settle or a release of scopedKeyID's hold: the key's
// sub-budget row, then its agent's row (a key attached to no agent has none). Both are taken before the
// workspace's balance, as the hold took them.
func lockKeyAndAgent(ctx context.Context, tx pgx.Tx, scopedKeyID string) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_lxc_subbudgets WHERE scoped_key_id = $1 FOR UPDATE`, scopedKeyID); err != nil {
		return fmt.Errorf("economy: lock sub-budget: %w", err)
	}
	var agentID, workspaceID string
	err := tx.QueryRow(ctx,
		`SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		  WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: agent of key: %w", err)
	}
	return lockAgent(ctx, tx, workspaceID, agentID)
}

// ErrSubBudgetExceeded is returned when a debit would push the agent's spent_lxc past its ceiling. The whole
// transaction rolls back — no claim, no debit — so the request_id stays retriable (e.g. after a ceiling raise).
var ErrSubBudgetExceeded = errors.New("economy: agent LXC sub-budget ceiling exceeded")

// DefaultAgentCeilingLXC is the ceiling applied to a scoped key with no explicit ceiling — 50 LXC ($5 at the
// 1 LXC = $0.10 peg), in µLXC (SEC-2). Baked here per the owner's step-A instruction (the capstone ships
// armed with a test ceiling). A funded agent spends up to this; an unfunded one (zero LXC balance) spends nothing.
const DefaultAgentCeilingLXC int64 = 50_000_000

// SetAgentCeiling upserts a scoped key's LXC ceiling in µLXC (preserving spent_lxc). This is how an operator
// sets a per-agent cap other than the default.
func (s *DualTokenStore) SetAgentCeiling(ctx context.Context, scopedKeyID, workspaceID string, ceilingLXC int64) error {
	if s == nil || s.pool == nil {
		return nil
	}
	if scopedKeyID == "" || workspaceID == "" {
		return errors.New("economy: SetAgentCeiling requires scoped_key_id + workspace_id")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0)
		 ON CONFLICT (scoped_key_id) DO UPDATE SET ceiling_lxc = EXCLUDED.ceiling_lxc, updated_at = now()`,
		scopedKeyID, workspaceID, ceilingLXC)
	if err != nil {
		return fmt.Errorf("economy: set agent ceiling: %w", err)
	}
	return nil
}

// AgentDebitMeta is the NON-CONTENT metadata stamped on an agent-allocator lxc_ledger debit row. It
// exists so the ledger is READABLE (per-model spend derivable) and a money row JOINS to its usage row —
// without lxc_ledger, an append-only + immutable financial record (migration 0055), ever becoming a
// content record. It carries EXACTLY three model/id SCALARS and structurally cannot carry prompt text, a
// hash, or an embedding (there is no field for content):
//
//   - RequestedModel: the model the charge was ESTIMATED on. The hold is PRE-SERVE / PRE-ROUTING, so this
//     is the REQUESTED model, NOT necessarily the one that served. Named "requested_model" on the row so a
//     UI cannot imply it was the serving model.
//   - ServedModel: the model that ACTUALLY served, known only post-route at settle time (empty on the
//     pre-serve hold and on a full refund/release, where nothing served). Stamped as "served_model" on the
//     delivered-charge SPEND row so that row is self-describing — "requested X, served Y, charged Z" —
//     without a token_events join. A model name, never content.
//   - RequestID: the token_events request_id, so the money row still joins to its usage row (real token
//     counts) instead of duplicating any of that here.
type AgentDebitMeta struct {
	RequestedModel string
	ServedModel    string
	RequestID      string
	// PriceBasis marks a charge that was priced on a DERIVED FALLBACK rate because the served model is
	// not in the catalog ("fallback"). Empty for exact pricing, so an ordinary row is unchanged and only
	// a guessed charge carries the marking — a bill built from this ledger can then separate measured
	// charges from estimated ones instead of presenting a guess as a price.
	PriceBasis string

	// PoolListULXC is the LIST price of a CROSS-TENANT POOLED cache hit: what this request would
	// have cost the consumer had it gone upstream, in µLXC, priced exactly the way a real charge is
	// (ceil). Zero on every other kind of charge, and zero leaves the row untouched — an ordinary
	// provider call must not carry a pooled-saving claim.
	//
	// ⚠ THE SAVING IS NOT A FIELD HERE, DELIBERATELY. It is derived at the insert from this list
	// price and the amount ACTUALLY debited, because the settle CLAMPS the charge to the hold: a
	// saving computed by the caller can disagree with what the customer really paid, and then one
	// row states two different prices. Derived, the three numbers reconcile on every row —
	// charged + saved = list — including clamped and partially-settled ones.
	PoolListULXC int64

	// PoolDiscountRate is the consumer discount rate in force WHEN THIS CHARGE WAS MADE. It rides on
	// the row so a bill can be audited against the rate that actually applied rather than the rate
	// configured today: the rate is tunable at boot, so reading it back from config would silently
	// re-price history.
	PoolDiscountRate float64

	// WrittenOffULXC is the part of the delivered cost a settle did not charge because it was above the hold
	// (B35.3). Set only by that settle, on its spend row; zero leaves every other row untouched.
	WrittenOffULXC int64
}

// toMap renders the scalars as the lxc_ledger metadata document, OMITTING an empty scalar so a row carries
// no empty-string noise. The economy layer owns this shape: a caller supplies only these typed model/id
// strings, never a free-form map, so no content can be injected into a money row.
func (m AgentDebitMeta) toMap() map[string]interface{} {
	out := map[string]interface{}{}
	if m.RequestedModel != "" {
		out["requested_model"] = m.RequestedModel
	}
	if m.ServedModel != "" {
		out["served_model"] = m.ServedModel
	}
	if m.RequestID != "" {
		out["request_id"] = m.RequestID
	}
	if m.PriceBasis != "" {
		out["price_basis"] = m.PriceBasis
	}
	if m.WrittenOffULXC > 0 {
		out["written_off_ulxc"] = m.WrittenOffULXC
	}
	return out
}

// toSpendMap is toMap plus the POOLED-DISCOUNT DISCLOSURE, and the only place that disclosure is
// assembled.
//
// chargedULXC is what was ACTUALLY debited — post-clamp — so pool_saved_ulxc is derived from the
// real charge rather than from what the caller intended to charge. That is the whole reason this
// takes an argument instead of being a field: the settle never bills above the hold, so it may bill
// less than the discounted price, and a saving passed in alongside would keep claiming the intended
// discount while the customer was charged something else.
//
// Emits NOTHING when PoolListULXC is 0, so an ordinary upstream charge is byte-for-byte the row it
// was before. A saving is clamped at 0: it must never read negative, which would present a pooled
// hit as having cost MORE than the live call and, read by any aggregator, would subtract from the
// total saved.
func (m AgentDebitMeta) toSpendMap(chargedULXC int64) map[string]interface{} {
	out := m.toMap()
	if m.PoolListULXC <= 0 {
		return out
	}
	saved := m.PoolListULXC - chargedULXC
	if saved < 0 {
		saved = 0
	}
	out["pool_list_ulxc"] = m.PoolListULXC
	out["pool_saved_ulxc"] = saved
	out["pool_discount_rate"] = m.PoolDiscountRate
	return out
}

// SpendLXCForAgent debits lxcAmount from the workspace's LXC balance on behalf of a scoped key (the "agent"),
// EXACTLY ONCE per requestID and only within the agent's remaining sub-budget. All of {claim, ceiling check,
// balance debit, spent bump} happen in ONE transaction — a claim without a debit, or a debit without a
// claim, is the double-spend bug this method exists to prevent. `meta` stamps the debit row with the
// requested model + token_events request_id (non-content; see AgentDebitMeta) so the ledger is readable.
//
// Returns nil on a fresh successful debit AND on an idempotent replay (a requestID already claimed ⇒ nothing
// debited). Returns ErrSubBudgetExceeded (ceiling) or ErrInsufficientLXC (balance) on a rejected debit —
// both roll the whole tx back (no orphan claim, retriable).
func (s *DualTokenStore) SpendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount int64, description string, meta AgentDebitMeta) error {
	return retryLocks(ctx, func() error {
		return s.spendLXCForAgent(ctx, scopedKeyID, workspaceID, requestID, lxcAmount, description, meta)
	})
}

// spendLXCForAgent is one attempt of SpendLXCForAgent, in one transaction.
func (s *DualTokenStore) spendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount int64, description string, meta AgentDebitMeta) error {
	if lxcAmount <= 0 {
		return errors.New("economy: agent spend amount must be positive")
	}
	if scopedKeyID == "" || workspaceID == "" || requestID == "" {
		return errors.New("economy: agent spend requires scoped_key_id, workspace_id, request_id")
	}
	if s == nil || s.pool == nil {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin agent spend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// B32.11: the debit carries the plan's platform fee, at the rate its settle will charge it at (the claim's).
	bps, err := s.platformFeeBPS(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	fee := PlatformFee(lxcAmount, bps)

	// (1) EXACTLY-ONCE claim. ON CONFLICT DO NOTHING ⇒ 0 rows means this requestID already succeeded — an
	// idempotent replay: debit NOTHING, return nil. The claim is committed in THIS tx (below) only on success.
	tag, err := tx.Exec(ctx,
		`INSERT INTO lxc_spend_claims (request_id, scoped_key_id, lxc_amount, platform_fee_bps) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (request_id) DO NOTHING`, requestID, scopedKeyID, lxcAmount, bps)
	if err != nil {
		return fmt.Errorf("economy: spend claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // idempotent replay — already debited under this request_id
	}

	// (2) Ensure + LOCK the sub-budget row (FOR UPDATE serializes concurrent debits for this agent).
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0) ON CONFLICT (scoped_key_id) DO NOTHING`,
		scopedKeyID, workspaceID, DefaultAgentCeilingLXC); err != nil {
		return fmt.Errorf("economy: ensure sub-budget: %w", err)
	}
	var ceiling, spent int64 // µLXC
	if err := tx.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1 FOR UPDATE`,
		scopedKeyID).Scan(&ceiling, &spent); err != nil {
		return fmt.Errorf("economy: read sub-budget: %w", err)
	}

	// (3) CEILING check — reject (rollback ⇒ no orphan claim) if this debit would exceed remaining.
	// B19.1: a key attached to an agent account spends the agent's balance, posted in this transaction;
	// the per-key ceiling binds only a key with no agent.
	isAgent, err := agentMovementFee(ctx, tx, scopedKeyID, lxcAmount, fee, bps, "spend", requestID, meta.RequestedModel)
	if err != nil {
		return s.refusedMovement(ctx, tx, err)
	}
	if !isAgent && ceiling-spent < lxcAmount+fee {
		return ErrSubBudgetExceeded
	}

	// (4) Debit the workspace LXC via the SAME internals SpendLXC uses (no duplication of the balance path).
	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID) // FOR UPDATE
	if err != nil {
		return err
	}
	if bal < lxcAmount+fee {
		return ErrInsufficientLXC // rollback ⇒ no orphan claim; retriable after funding
	}
	if !isAgent { // B19.13: a key attached to no agent is the workspace's own spending
		if err := requireUnallocated(ctx, tx, workspaceID, bal, lxcAmount+fee); err != nil {
			return err
		}
	}
	newBal := bal - lxcAmount // exact integer µLXC
	// Stamp the non-content metadata (requested model + token_events request_id) so the ledger is readable
	// and joins to token_events. meta.toMap() carries ONLY those two scalars — never content (0055 immutable).
	if err := insertLXCLedger(ctx, tx, workspaceID, -lxcAmount, newBal, LXCTypeSpend, description, meta.toMap()); err != nil {
		return err
	}
	if err := insertPlatformFee(ctx, tx, workspaceID, fee, newBal-fee, bps, lxcAmount, meta.RequestID); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal-fee, minted, wsSpent+lxcAmount+fee); err != nil {
		return err
	}

	// (5) Bump the agent's spent_lxc (monotonic) — atomic with the debit + the claim.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc + $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, lxcAmount+fee); err != nil {
		return fmt.Errorf("economy: bump spent: %w", err)
	}

	return tx.Commit(ctx)
}

// ─── RESERVATION lifecycle (billing redesign) ───────────────────────────────
//
// The permanent pre-serve debit (SpendLXCForAgent, above) is replaced by a HOLD → SETTLE/RELEASE pair so
// the customer is billed what was actually DELIVERED, not a pre-serve estimate — while the CEILING stays
// enforced pre-serve against a CONSERVATIVE (output-aware) hold. Every balance move is a NEW lxc_ledger
// row (0055 forbids UPDATE/DELETE on the ledger); only the mutable lifecycle status lives in
// lxc_reservations. Exactly-once: the HOLD is gated by the reservation_id PRIMARY KEY (ON CONFLICT DO
// NOTHING); the resolution is a SELECT ... FOR UPDATE status-CAS (only the first caller to find 'held'
// resolves it, so a settle+release race or a replay is a no-op).

const (
	// LXCTypeReservationHold marks the pre-serve HOLD debit — a bound, NOT a bill. Revenue readers
	// (sum type='spend') MUST exclude it; it nets to zero against its release.
	LXCTypeReservationHold = "reservation_hold"
	// LXCTypeReservationRelease marks the compensating CREDIT that undoes a hold (on settle: refund the
	// unused reservation; on release: refund the whole hold). Append-only-safe (a new row, never an edit).
	LXCTypeReservationRelease = "reservation_release"
)

// ReserveLXCForAgent HOLDS heldLXC of the workspace's LXC against the agent's sub-budget, EXACTLY ONCE per
// reservationID, only within the remaining ceiling. It is the pre-serve gate: the caller BLOCKS the request
// unless this returns nil (mirrors the old SpendLXCForAgent block). heldLXC must be the CONSERVATIVE
// (output-aware) estimate so it is an upper bound on the delivered cost — the ceiling stays airtight. The
// hold is later reconciled by SettleLXCReservation (bill the delivered cost, refund the rest) or refunded in
// full by ReleaseLXCReservation. Returns ErrSubBudgetExceeded / ErrInsufficientLXC on a rejected hold (whole
// tx rolls back — no orphan reservation, retriable); nil on a fresh hold AND on an idempotent replay.
func (s *DualTokenStore) ReserveLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, reservationID string, heldLXC int64, meta AgentDebitMeta) error {
	return retryLocks(ctx, func() error {
		return s.reserveLXCForAgent(ctx, scopedKeyID, workspaceID, reservationID, heldLXC, meta)
	})
}

// reserveLXCForAgent is one attempt of ReserveLXCForAgent, in one transaction.
func (s *DualTokenStore) reserveLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, reservationID string, heldLXC int64, meta AgentDebitMeta) error {
	if heldLXC <= 0 {
		return errors.New("economy: reservation hold amount must be positive")
	}
	if scopedKeyID == "" || workspaceID == "" || reservationID == "" {
		return errors.New("economy: reserve requires scoped_key_id, workspace_id, reservation_id")
	}
	if s == nil || s.pool == nil {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// B32.11: the hold is the estimate plus its platform fee, so the agent's balance and every limit judge the
	// whole price; the rate rides the reservation to its settle.
	bps, err := s.platformFeeBPS(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	heldLXC += PlatformFee(heldLXC, bps)

	// (1) EXACTLY-ONCE hold claim — reservation_id PK. 0 rows ⇒ this id already holds ⇒ idempotent replay.
	tag, err := tx.Exec(ctx,
		`INSERT INTO lxc_reservations (reservation_id, scoped_key_id, workspace_id, held_ulxc, status, requested_model, request_id, platform_fee_bps)
		 VALUES ($1, $2, $3, $4, 'held', $5, $6, $7) ON CONFLICT (reservation_id) DO NOTHING`,
		reservationID, scopedKeyID, workspaceID, heldLXC, nullIfEmpty(meta.RequestedModel), nullIfEmpty(meta.RequestID), bps)
	if err != nil {
		return fmt.Errorf("economy: reservation claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // idempotent replay — already held under this reservation_id
	}

	// (2) Ceiling — ensure + LOCK the sub-budget (FOR UPDATE serializes concurrent holds for this agent).
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0) ON CONFLICT (scoped_key_id) DO NOTHING`,
		scopedKeyID, workspaceID, DefaultAgentCeilingLXC); err != nil {
		return fmt.Errorf("economy: ensure sub-budget: %w", err)
	}
	var ceiling, spent int64
	if err := tx.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1 FOR UPDATE`,
		scopedKeyID).Scan(&ceiling, &spent); err != nil {
		return fmt.Errorf("economy: read sub-budget: %w", err)
	}
	// B19.1: an agent's key holds against the agent's balance (posted here), not the per-key ceiling.
	isAgent, err := agentMovement(ctx, tx, scopedKeyID, heldLXC, "hold", reservationID, meta.RequestedModel)
	if err != nil {
		return s.refusedMovement(ctx, tx, err) // rollback ⇒ no orphan reservation
	}
	if !isAgent && ceiling-spent < heldLXC {
		return ErrSubBudgetExceeded // rollback ⇒ no orphan reservation
	}

	// (3) Debit the workspace LXC for the hold (an immutable lxc_ledger row + balance decrement). The row
	// type is LXCTypeReservationHold — a BOUND, excluded from revenue; the metadata joins to token_events.
	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if bal < heldLXC {
		return ErrInsufficientLXC // rollback ⇒ no orphan reservation; retriable after funding
	}
	if !isAgent { // B19.13: a key attached to no agent is the workspace's own spending
		if err := requireUnallocated(ctx, tx, workspaceID, bal, heldLXC); err != nil {
			return err
		}
	}
	newBal := bal - heldLXC
	if err := insertLXCLedger(ctx, tx, workspaceID, -heldLXC, newBal, LXCTypeReservationHold, "reservation hold (pre-serve)", meta.toMap()); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal, minted, wsSpent+heldLXC); err != nil {
		return err
	}
	// (4) Bump spent_lxc by the HELD amount — the ceiling counts the conservative reservation. Settle
	// reclaims the unused difference back into the budget.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc + $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, heldLXC); err != nil {
		return fmt.Errorf("economy: bump spent (hold): %w", err)
	}
	return tx.Commit(ctx)
}

// SettleLXCReservation reconciles a held reservation to the DELIVERED charge finalLXC: it credits back the
// unused reservation (refund = held − final) and books final as the real bill. final is CLAMPED to [0, held]
// — the conservative hold is an upper bound, and the customer is NEVER charged more than was reserved (belt
// and braces: even a mis-estimated hold cannot over-bill). B35.3: a clamp is never silent — it is an ERROR
// log and lens_agent_hold_cuts_total, and the spend row records the written_off_ulxc. Two immutable ledger rows in one tx: a
// LXCTypeReservationRelease credit of +held (undo the hold) and a LXCTypeSpend debit of −final (THE bill,
// joined to token_events by request_id) — net balance move +refund. The agent's spent_lxc drops by refund so
// the reserved-but-unspent headroom returns to its budget. Idempotent via the status-CAS: a second settle, or
// a settle racing a release, finds status≠'held' and is a no-op. A settle of an unknown reservation is an
// error (a bug — you cannot bill what you never held).
//
// RETURNS the µLXC ACTUALLY charged (finalLXC after the [0, held] clamp) so the caller can tie a downstream
// action — a cross-tenant royalty mint — to what the consumer REALLY paid. The idempotent no-op and every
// error path return 0 (this call charged nothing new): a royalty funded on a 0 return mints nothing, which
// is the deflationary-safe direction. cashBackedULXC is the part that may fund that royalty (royaltyBacked).
func (s *DualTokenStore) SettleLXCReservation(ctx context.Context, reservationID string, finalLXC int64, meta AgentDebitMeta) (settledULXC, cashBackedULXC int64, err error) {
	var cut holdCut
	err = retryLocks(ctx, func() error {
		var err error
		cut = holdCut{}
		settledULXC, cashBackedULXC, err = s.settleLXCReservation(ctx, reservationID, finalLXC, meta, &cut)
		return err
	})
	if err == nil && cut.writtenOff > 0 {
		slog.Error("economy: a settle was cut to its hold — the answer cost more than was reserved, and the rest is written off",
			slog.String("reservation", reservationID), slog.String("model", cut.model),
			slog.Int64("delivered_ulxc", cut.delivered), slog.Int64("held_ulxc", cut.held),
			slog.Int64("charged_ulxc", settledULXC), slog.Int64("written_off_ulxc", cut.writtenOff))
		label := "other" // a requested model is the client's string; a metric label must be a bounded set
		if _, known := catalog.Get(cut.model); known {
			label = cut.model
		}
		metrics.AgentHoldCut(label)
	}
	return settledULXC, cashBackedULXC, err
}

// holdCut is what a settle's clamp to its hold cut off (B35.3): zero when the delivered cost fit.
type holdCut struct {
	model                       string
	delivered, held, writtenOff int64
}

// settleLXCReservation is one attempt of SettleLXCReservation, in one transaction. cut reports a clamp.
func (s *DualTokenStore) settleLXCReservation(ctx context.Context, reservationID string, finalLXC int64, meta AgentDebitMeta, cut *holdCut) (settledULXC, cashBackedULXC int64, err error) {
	if reservationID == "" {
		return 0, 0, errors.New("economy: settle requires reservation_id")
	}
	if finalLXC < 0 {
		finalLXC = 0
	}
	if s == nil || s.pool == nil {
		return 0, 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("economy: begin settle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var scopedKeyID, workspaceID, status, reqModel, reqID string
	var heldLXC, bps int64
	// requested_model + request_id come from the reservation ROW — the SINGLE source the hold wrote, so the
	// settle's rows stamp exactly what the hold row shows (never a second in-memory copy that could drift).
	// So does the platform fee's rate (B32.11): the hold counted the fee at it.
	err = tx.QueryRow(ctx,
		`SELECT scoped_key_id, workspace_id, held_ulxc, status, COALESCE(requested_model, ''), COALESCE(request_id, ''), platform_fee_bps
		   FROM lxc_reservations WHERE reservation_id = $1 FOR UPDATE`,
		reservationID).Scan(&scopedKeyID, &workspaceID, &heldLXC, &status, &reqModel, &reqID, &bps)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("economy: settle unknown reservation %q", reservationID)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("economy: read reservation: %w", err)
	}
	if status != "held" {
		// ⚠ IDEMPOTENT NO-OP, and it is what makes a double settle unable to double-decrement
		// cash-backed: the second call returns before consumeCashBacked is ever reached.
		return 0, 0, nil
	}
	// The lock order: the key's sub-budget and its agent before the workspace's balance, as the hold took them.
	if err := lockKeyAndAgent(ctx, tx, scopedKeyID); err != nil {
		return 0, 0, err
	}
	// Never bill above the conservative hold: the delivered charge and its platform fee together fit in it.
	if within := spendWithin(heldLXC, bps); finalLXC > within {
		model := meta.ServedModel
		if model == "" {
			model = reqModel
		}
		*cut = holdCut{model: model, delivered: finalLXC, held: heldLXC, writtenOff: finalLXC - within}
		finalLXC = within
	}
	fee := PlatformFee(finalLXC, bps)
	refund := heldLXC - finalLXC - fee // ≥ 0

	// Two compensating rows: release the whole hold (+held), then book the delivered charge (−final). Net
	// balance move = +refund. Both are INSERTs — 0055-safe. lifetime_spent nets to +final (was +held at hold).
	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return 0, 0, err
	}
	afterRelease := bal + heldLXC
	// The release (undo the hold) is a refund — nothing served, so requested_model + request_id only.
	if err := insertLXCLedger(ctx, tx, workspaceID, heldLXC, afterRelease, LXCTypeReservationRelease, "reservation settle: release hold",
		AgentDebitMeta{RequestedModel: reqModel, RequestID: reqID}.toMap()); err != nil {
		return 0, 0, err
	}
	afterSpend := afterRelease - finalLXC
	if finalLXC > 0 {
		// The delivered-charge spend row is self-describing: the model the customer REQUESTED (from the row)
		// AND the model that actually SERVED (from the caller, known only post-route), plus the request_id join.
		if err := insertLXCLedger(ctx, tx, workspaceID, -finalLXC, afterSpend, LXCTypeSpend, "reservation settle: delivered charge",
			AgentDebitMeta{RequestedModel: reqModel, ServedModel: meta.ServedModel, RequestID: reqID,
				PriceBasis: meta.PriceBasis, PoolListULXC: meta.PoolListULXC,
				PoolDiscountRate: meta.PoolDiscountRate, WrittenOffULXC: cut.writtenOff}.toSpendMap(finalLXC)); err != nil {
			return 0, 0, err
		}
	}
	// B32.11: the platform fee on the delivered charge, its own row in this transaction.
	if err := insertPlatformFee(ctx, tx, workspaceID, fee, afterSpend-fee, bps, finalLXC, reqID); err != nil {
		return 0, 0, err
	}
	afterSpend -= fee
	// Resolved BEFORE the balance write, which keeps test-funded credits (B22.1) within the balance and the
	// holds still open — this one is not.
	if _, err := tx.Exec(ctx,
		`UPDATE lxc_reservations SET status = 'settled', settled_ulxc = $2, resolved_at = now() WHERE reservation_id = $1`,
		reservationID, finalLXC); err != nil {
		return 0, 0, fmt.Errorf("economy: mark settled: %w", err)
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, afterSpend, minted, wsSpent-refund); err != nil {
		return 0, 0, err
	}
	// ⚠ CASH-BACKED CONSUMPTION, AGAINST afterRelease — NOT against the held-down balance. The hold
	// debited `balance` without touching cash_backed, so mid-hold `balance - cash_backed` is
	// negative and a naive subtraction is nonsense. afterRelease is the balance with the hold undone,
	// which is the figure this spend actually draws from.
	//
	// ⚠ HERE AND NOT AT THE HOLD: a hold is provisional and may be released in full. Only a settled
	// spend consumes. ReleaseLXCReservation writes no spend row and does not call this, so a
	// hold-then-release cannot decrement backing, and the `status != "held"` guard above makes a
	// second settle a no-op — neither can double-decrement.
	cashSpent, err := consumeCashBacked(ctx, tx, workspaceID, afterRelease, finalLXC)
	if err != nil {
		return 0, 0, err
	}
	// The fee consumes backing after the charge; only the charge's part may fund a royalty.
	if _, err := consumeCashBacked(ctx, tx, workspaceID, afterRelease-finalLXC, fee); err != nil {
		return 0, 0, err
	}
	if cashSpent, err = royaltyBacked(ctx, tx, workspaceID, finalLXC, cashSpent); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc - $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, refund); err != nil {
		return 0, 0, fmt.Errorf("economy: reclaim spent (settle): %w", err)
	}
	// B19.1: the part of the hold not charged goes back to the agent, and its platform fee is its own posting.
	if _, err := agentMovement(ctx, tx, scopedKeyID, -(refund + fee), "settle", reservationID, reqModel); err != nil {
		return 0, 0, err
	}
	if err := postAgentFee(ctx, tx, scopedKeyID, fee, bps, reservationID, reqModel); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("economy: commit settle: %w", err)
	}
	return finalLXC, cashSpent, nil
}

// ReleaseLXCReservation refunds a held reservation IN FULL (final charge 0): a self-cache hit (no upstream
// call, no contributor ⇒ free), or a serve that never delivered (crash/failure ⇒ the customer must not pay).
// One compensating LXCTypeReservationRelease credit of +held; spent_lxc drops by the whole held. Idempotent
// via the status-CAS. A release of an unknown reservation is a no-op (a stranded-sweeper double-run is safe).
func (s *DualTokenStore) ReleaseLXCReservation(ctx context.Context, reservationID, reason string) error {
	return retryLocks(ctx, func() error { return s.releaseLXCReservation(ctx, reservationID, reason) })
}

// releaseLXCReservation is one attempt of ReleaseLXCReservation, in one transaction.
func (s *DualTokenStore) releaseLXCReservation(ctx context.Context, reservationID, reason string) error {
	if reservationID == "" {
		return errors.New("economy: release requires reservation_id")
	}
	if s == nil || s.pool == nil {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var scopedKeyID, workspaceID, status, reqModel, reqID string
	var heldLXC int64
	// requested_model + request_id from the reservation ROW so the release row (in-band OR stranded-swept —
	// both flow through here) stamps the same model/id the hold recorded. A refund serves nothing → no served_model.
	err = tx.QueryRow(ctx,
		`SELECT scoped_key_id, workspace_id, held_ulxc, status, COALESCE(requested_model, ''), COALESCE(request_id, '')
		   FROM lxc_reservations WHERE reservation_id = $1 FOR UPDATE`,
		reservationID).Scan(&scopedKeyID, &workspaceID, &heldLXC, &status, &reqModel, &reqID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing to release — a double-sweep or a never-held id is a safe no-op
	}
	if err != nil {
		return fmt.Errorf("economy: read reservation: %w", err)
	}
	if status != "held" {
		return nil // already resolved — idempotent
	}
	// The lock order: the key's sub-budget and its agent before the workspace's balance, as the hold took them.
	if err := lockKeyAndAgent(ctx, tx, scopedKeyID); err != nil {
		return err
	}

	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	afterRelease := bal + heldLXC
	desc := "reservation release (full refund)"
	if reason != "" {
		desc = "reservation release: " + reason
	}
	if err := insertLXCLedger(ctx, tx, workspaceID, heldLXC, afterRelease, LXCTypeReservationRelease, desc,
		AgentDebitMeta{RequestedModel: reqModel, RequestID: reqID}.toMap()); err != nil {
		return err
	}
	// Resolved before the balance write, as in SettleLXCReservation (B22.1).
	if _, err := tx.Exec(ctx,
		`UPDATE lxc_reservations SET status = 'released', settled_ulxc = 0, resolved_at = now() WHERE reservation_id = $1`,
		reservationID); err != nil {
		return fmt.Errorf("economy: mark released: %w", err)
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, afterRelease, minted, wsSpent-heldLXC); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc - $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, heldLXC); err != nil {
		return fmt.Errorf("economy: reclaim spent (release): %w", err)
	}
	// B19.1: a released hold goes back to the agent in full.
	if _, err := agentMovement(ctx, tx, scopedKeyID, -heldLXC, "release", reservationID, reqModel); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReleaseStrandedReservations REFUNDS every hold older than olderThan (a crash between reserve and settle).
// It only ever RELEASES (never settles): a stranded hold's serve outcome is unknown, so the safe money move
// is to give the customer their LXC back. Returns the count released. Idempotent per row via the status-CAS.
func (s *DualTokenStore) ReleaseStrandedReservations(ctx context.Context, olderThan time.Duration) (int, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	rows, err := s.pool.Query(ctx,
		`SELECT reservation_id FROM lxc_reservations WHERE status = 'held' AND created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("economy: scan stranded reservations: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := s.ReleaseLXCReservation(ctx, id, "stranded hold swept (crash refund)"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func nullIfEmpty(str string) interface{} {
	if str == "" {
		return nil
	}
	return str
}
