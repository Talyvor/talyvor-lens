// Package mining is the LENS-token compute-mining engine. This
// file lands the cache-mining track: workspaces earn LENS when
// the responses they cached get served to other workspaces.
// Future files in this package (storage_mining.go, model_mining.go,
// etc.) will share the LedgerStore implemented here.

package mining

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbjson"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/workspace"
)

// ─── constants ───────────────────────────────────

// Phase-4a Item 0: the CacheMiner cache-mining track was RETIRED here. It was
// constructed-but-never-called (superseded by pool-royalty, which value-anchors
// a contributor's earning at s×avoided_COGS on opt-in pooled cross-tenant hits).
// Its only unique path — own-cache minting (owner==requester) — is self-inflation
// (no second party, no real value received) and trivially farmable in a
// single-workspace loop that no linkage guard can see. The generic held-mint
// plumbing it once used (traffic_mint_holds, TypeCacheMineHeld, the sweeper) stays
// as inert generic infra now reused by the pattern held mint; TypeCacheMine below
// remains a counted supply type with no producer.

// Ledger transaction types — keep in sync with the type column
// CHECK constraint we'd add in a future migration (we leave the
// column as free-text for now so new mining tracks don't need a
// migration).
const (
	TypeCacheMine = "cache_mine"
	TypeSpend     = "spend"
	TypeTransfer  = "transfer"
)

// ─── types ───────────────────────────────────────

// LedgerEntry mirrors one row of lens_token_ledger. Amount/BalanceAfter are
// integer µLENS (SEC-2); the JSON keys carry the explicit _ulens unit suffix so
// no consumer mistakes the 1e6-scaled integer for a float LENS value.
type LedgerEntry struct {
	ID           string                 `json:"id"`
	WorkspaceID  string                 `json:"workspace_id"`
	Amount       int64                  `json:"amount_ulens"`
	BalanceAfter int64                  `json:"balance_after_ulens"`
	Type         string                 `json:"type"`
	Description  string                 `json:"description"`
	Metadata     map[string]interface{} `json:"metadata"`
	CreatedAt    time.Time              `json:"created_at"`
}

// BalanceSnapshot mirrors one row of lens_token_balances. Balances are integer
// µLENS (SEC-2); JSON keys carry the explicit _ulens unit suffix.
type BalanceSnapshot struct {
	WorkspaceID string `json:"workspace_id"`
	// Balance is SPENDABLE LENS. A held mint does not touch it.
	Balance int64 `json:"balance_ulens"`
	// HeldBalance is EARNED BUT NOT YET SPENDABLE — a pool royalty in its holdback window,
	// settled to Balance by internal/poolroyalty's finalize sweeper once finalize_after passes
	// (LENS_POOL_HOLDBACK_WINDOW, default 72h).
	//
	// ⚠ REPORTED SEPARATELY, NEVER FOLDED IN. The first real royalty minted 822 µLENS held while
	// the screen showed a balance of 0 — correct, and unexplainable, because a caller reading only
	// Balance cannot tell "earned nothing" from "earned 822 that has not settled". Adding it to
	// Balance would be the opposite error: offering a number that cannot be spent, so a conversion
	// refuses an amount the user was just shown. Three states, three numbers.
	HeldBalance    int64     `json:"held_balance_ulens"`
	LifetimeEarned int64     `json:"lifetime_earned_ulens"`
	LifetimeSpent  int64     `json:"lifetime_spent_ulens"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ─── errors ──────────────────────────────────────

// ErrInsufficientBalance is the typed error LedgerStore.Debit
// returns when the workspace doesn't have enough LENS to cover
// the debit. Callers `errors.Is` against it to map to 402 Payment
// Required at the HTTP boundary.
var ErrInsufficientBalance = errors.New("mining: insufficient balance")

// ─── pgxDB shim ──────────────────────────────────

// pgxDB is the subset of *pgxpool.Pool the store needs. Letting
// tests substitute pgxmock keeps integration tests cheap.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ─── LedgerStore ─────────────────────────────────

// LedgerStore is the persistence layer for every token movement
// in Lens. All mutators run inside a transaction so the ledger
// insert and the balance update are atomically consistent.
type LedgerStore struct {
	pool pgxDB
	// verifier is the U6 verified-to-earn gate. nil ⇒ no gate (tests + pre-flip
	// behave exactly as before). Checked inside applyTx/heldInner for mint-type
	// credits only — see mint_gate.go.
	verifier MintVerifier
	// mintRateCap is the U6 PR2 per-workspace rolling-window minted-LENS ceiling
	// in µLENS (<=0 ⇒ off); mintRateWindow is the window (default 24h). Enforced
	// inside applyTx/heldInner AFTER the balance FOR UPDATE — see checkMintRateCap.
	mintRateCap    int64
	mintRateWindow time.Duration
	// reputationGate is the P1 #9 reputation-bonded-minting flag. nil/false ⇒ no-op (mint path
	// byte-identical). When on, applyTx/CreditHeldTx scale a bonded mint by f(R) and gate it to 0
	// below the access floor — an ADDITIVE constraint downstream of verifyEarn (mint_gate.go).
	reputationGate func() bool

	// shadowSink / shadowTypes are shadow mode (shadow.go): a mint type in shadowTypes records
	// what it WOULD pay and credits nothing. nil sink or empty set ⇒ entirely off, mint path
	// byte-identical.
	shadowSink  ShadowSink
	shadowTypes map[string]struct{}

	// driftHaircut is KE-2: a REDUCE-ONLY multiplier in [floor, 1.0] applied AFTER the reputation factor,
	// derived from Keel's HARDENED idiosyncratic drift finding for the workspace. nil ⇒ no-op (byte-identical).
	// It can only LOWER a bonded mint (never increase, never below the floor, never burn/slash). Reads via the
	// caller's tx so mining imports no keel. See mint_gate.go:reputationBondedAmount.
	driftHaircut func(ctx context.Context, tx pgx.Tx, workspaceID string) (float64, error)

	// wall is B25.1's test-money wall (workspace.CheckMoneyWall), checked by Transfer inside its transaction.
	// nil ⇒ no wall (tests); production sets it.
	wall MoneyWall
}

// MoneyWall refuses money moving between a test (synthetic) workspace and a real one — workspace.CheckMoneyWall.
type MoneyWall func(ctx context.Context, q workspace.RowQuerier, a, b string) error

// SetMoneyWall sets the check Transfer makes before it moves LENS between two workspaces (B25.1).
func (s *LedgerStore) SetMoneyWall(w MoneyWall) { s.wall = w }

// NewLedgerStore wraps a real *pgxpool.Pool.
func NewLedgerStore(pool *pgxpool.Pool) *LedgerStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newLedgerStore(db)
}

// newLedgerStore is the test-friendly constructor (accepts the
// pgxmock interface).
func newLedgerStore(db pgxDB) *LedgerStore {
	return &LedgerStore{pool: db}
}

// PgxDB is the public alias for the internal pgx interface. Lets
// sister packages (e.g. internal/economy) build a LedgerStore
// against a pgxmock pool in tests without exporting unrelated
// internals.
type PgxDB = pgxDB

// NewLedgerStoreForTesting wraps an arbitrary pgxDB-shaped pool
// (typically a pgxmock.PgxPoolIface) so tests in other packages
// can construct a LedgerStore without owning a real
// *pgxpool.Pool. Production code should use NewLedgerStore.
func NewLedgerStoreForTesting(db PgxDB) *LedgerStore {
	return newLedgerStore(db)
}

// Credit adds `amount` LENS to a workspace. Append-only — no row
// is ever updated, only inserted. The balance table is upserted
// inside the same tx so reads always see a consistent snapshot.
func (s *LedgerStore) Credit(
	ctx context.Context,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: credit amount must be positive")
	}
	if err := s.apply(ctx, workspaceID, amount, txType, description, metadata, true); err != nil {
		return err
	}
	// A credit is LENS entering circulation (mining rewards). Transfers move
	// between workspaces via Transfer (not Credit), so this doesn't double-count.
	metrics.MintedTokens(MicroToFloat(amount))
	return nil
}

// Debit removes `amount` LENS from a workspace. Returns
// ErrInsufficientBalance when the workspace doesn't have enough
// to cover it — the tx rolls back so no partial state leaks.
func (s *LedgerStore) Debit(
	ctx context.Context,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: debit amount must be positive")
	}
	return s.apply(ctx, workspaceID, amount, txType, description, metadata, false)
}

// applyTx contains all the SQL for a single credit or debit within a
// caller-supplied transaction. It does NOT call Begin, Commit, or Rollback —
// the caller owns the transaction boundary. This makes it composable: multiple
// ledger operations can share one transaction and roll back atomically together.
//
// Use Credit / Debit for standalone operations.
// Use CreditTx / DebitTx when multiple operations must be atomic together.
func (s *LedgerStore) applyTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
	add bool,
) error {
	// Observe the ledger-write hot path. This is the latency the
	// LensTokenLedgerSlow alert guards.
	start := time.Now()
	defer func() { metrics.ObserveLedgerWrite(time.Since(start)) }()

	// U6 Sybil floor: a mint-type credit only proceeds for a verified-to-earn
	// workspace. No-op for debits (add=false), conservation credits (non-mint
	// txType), and when no verifier is wired. Runs on THIS tx so a block
	// (ErrEarnNotVerified) rolls the whole mint back — no ledger row, no balance
	// change, no metrics.
	if add {
		if err := s.verifyEarn(ctx, tx, workspaceID, txType); err != nil {
			return err
		}
		// SHADOW MODE (shadow.go). Placed at this kernel — one of the only two every LENS mint
		// funnels through — so a mint added later is shadowable without touching its call site,
		// and BEFORE the ledger INSERT below, so no row is written and then undone.
		if err := s.shadowIntercept(ctx, workspaceID, txType, amount, metadata); err != nil {
			return err
		}
		// P1 #9: reputation bond — DOWNSTREAM of the U6 floor (compose, never bypass). For a bonded
		// mint type with the gate on, scale `amount` by f(R) and gate to ErrReputationFloor below
		// the access floor. No-op (amount unchanged, no read) when off or non-bonded → byte-identical.
		eff, rerr := s.reputationBondedAmount(ctx, tx, workspaceID, txType, amount, metadata)
		if rerr != nil {
			return rerr
		}
		amount = eff // the rest of the credit (delta, earned, rate-cap, ledger row) uses the effective amount
	}

	// Two-step pessimistic lock: ensure the row exists without touching
	// updated_at on every write, then acquire an explicit FOR UPDATE lock.
	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return fmt.Errorf("mining: ensure balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_earned, lifetime_spent
		FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	var bal, earned, spent int64
	if err := row.Scan(&bal, &earned, &spent); err != nil {
		return fmt.Errorf("mining: read balance: %w", err)
	}

	var delta int64
	if add {
		delta = amount
		earned += amount
	} else {
		if bal < amount {
			return ErrInsufficientBalance
		}
		delta = -amount
		spent += amount
	}
	newBal := bal + delta

	// U6 PR2: rolling-window mint rate cap. Only for mint-type credits, and
	// placed AFTER the FOR UPDATE above so concurrent same-workspace mints
	// serialize on the balance row and the SUM sees prior committed mints
	// (exact, no race). A block (ErrMintRateCapExceeded) rolls the whole mint
	// back — no ledger row, no balance change, no metrics.
	if add {
		if err := s.checkMintRateCap(ctx, tx, workspaceID, txType, amount); err != nil {
			return err
		}
	}

	// dbjson.JSONB encodes as JSON text on both pgx protocols — a raw []byte
	// is inferred as bytea under the SimpleProtocol that LENS_DB_PGBOUNCER
	// forces and rejected by jsonb with 22P02 (#133).
	meta, err := dbjson.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger
			(workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, workspaceID, delta, newBal, txType, description, meta); err != nil {
		return fmt.Errorf("mining: insert ledger row: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1
	`, workspaceID, newBal, earned, spent); err != nil {
		return fmt.Errorf("mining: update balance: %w", err)
	}

	return nil
}

// apply is the shared transactional path Credit + Debit funnel through.
// Opens its own transaction, delegates all SQL to applyTx, then commits.
// Existing callers (Credit / Debit) are unchanged.
func (s *LedgerStore) apply(
	ctx context.Context,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
	add bool,
) error {
	if s.pool == nil {
		// No DB — silently succeed so tests that skip the DB
		// path can still exercise higher-level mining logic.
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.applyTx(ctx, tx, workspaceID, amount, txType, description, metadata, add); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreditTx adds `amount` LENS to workspaceID within a caller-supplied
// transaction. The caller owns Begin / Commit / Rollback — this lets
// multiple ledger credits (and other DB writes) share one atomic transaction.
func (s *LedgerStore) CreditTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: credit amount must be positive")
	}
	return s.applyTx(ctx, tx, workspaceID, amount, txType, description, metadata, true)
}

// DebitTx removes `amount` LENS from workspaceID within a caller-supplied
// transaction. Returns ErrInsufficientBalance when the balance is too low.
// The caller owns Begin / Commit / Rollback.
func (s *LedgerStore) DebitTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: debit amount must be positive")
	}
	return s.applyTx(ctx, tx, workspaceID, amount, txType, description, metadata, false)
}

// GetBalance returns 0.0 (not an error) for a workspace with no
// ledger history. That matches the spec — new workspaces are a
// supported case, not an exceptional one.
func (s *LedgerStore) GetBalance(ctx context.Context, workspaceID string) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT balance FROM lens_token_balances WHERE workspace_id = $1
	`, workspaceID)
	var b int64
	if err := row.Scan(&b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("mining: read balance: %w", err)
	}
	return b, nil
}

// GetSnapshot returns the full balance row (with lifetime
// totals). Returns a zero-value snapshot (not nil) for a fresh
// workspace so callers don't have to nil-check.
func (s *LedgerStore) GetSnapshot(ctx context.Context, workspaceID string) (BalanceSnapshot, error) {
	if s.pool == nil {
		return BalanceSnapshot{WorkspaceID: workspaceID}, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT workspace_id, balance, held_balance, lifetime_earned, lifetime_spent, updated_at
		FROM lens_token_balances WHERE workspace_id = $1
	`, workspaceID)
	var s2 BalanceSnapshot
	if err := row.Scan(&s2.WorkspaceID, &s2.Balance, &s2.HeldBalance, &s2.LifetimeEarned, &s2.LifetimeSpent, &s2.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BalanceSnapshot{WorkspaceID: workspaceID}, nil
		}
		return BalanceSnapshot{}, fmt.Errorf("mining: read snapshot: %w", err)
	}
	return s2, nil
}

// ─── Transfer / Burn (Batch 3) ──────────────────

// MinTransferAmount is the floor for Transfer, in µLENS (SEC-2). Spec-mandated
// — below this we risk dust attacks polluting the ledger.
const MinTransferAmount int64 = 1_000 // 0.001 LENS

// TypeTransfer / TypeBurn are the ledger row types for the
// Batch-3 economy primitives.
const (
	TypeTransferOut = "transfer_out"
	TypeTransferIn  = "transfer_in"
	TypeBurn        = "burn"
)

// TypePoolRoyalty tags a Pool-B royalty mint (Phase-2 Stage 2.1): a served
// cross-tenant pooled cache hit credits the contributing workspace
// s × avoided_COGS. Canonical home is here with the other ledger row types;
// internal/poolroyalty (the writer) aliases it. Counted in GetTotalSupply
// since Stage 2.2 — a royalty mint is LENS entering circulation.
const TypePoolRoyalty = "pool_royalty"

// Transfer atomically debits `from` and credits `to`. Both
// ledger rows + both balance updates happen inside one
// transaction so a partial failure can't drop or duplicate LENS.
func (s *LedgerStore) Transfer(
	ctx context.Context,
	fromWorkspace, toWorkspace string,
	amount int64, // µLENS
	description string,
) error {
	if amount < MinTransferAmount {
		return fmt.Errorf("mining: transfer amount must be ≥ %v", MinTransferAmount)
	}
	if fromWorkspace == "" || toWorkspace == "" {
		return errors.New("mining: from / to workspace required")
	}
	if fromWorkspace == toWorkspace {
		return errors.New("mining: cannot transfer to self")
	}
	if s.pool == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin transfer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// B25.1: test LENS moves only between test workspaces, real LENS only between real ones.
	if s.wall != nil {
		if err := s.wall(ctx, tx, fromWorkspace, toWorkspace); err != nil {
			return err
		}
	}

	// Impose a global lock order: always acquire the lexicographically
	// smaller workspace ID first, regardless of debit/credit direction.
	//
	// Without this, two concurrent opposite-direction transfers deadlock:
	//   Tx A: Transfer("ws_alice", "ws_bob") → locks ws_alice → waits for ws_bob
	//   Tx B: Transfer("ws_bob", "ws_alice") → locks ws_bob   → waits for ws_alice
	//
	// With a consistent global order both transactions always lock the same
	// workspace first, so one blocks and waits while the other completes —
	// no cycle, no deadlock.
	firstWS, secondWS := fromWorkspace, toWorkspace
	if firstWS > secondWS {
		firstWS, secondWS = secondWS, firstWS
	}

	// Acquire both locks before any writes.
	firstBal, firstEarned, firstSpent, err := readBalance(ctx, tx, firstWS)
	if err != nil {
		return err
	}
	secondBal, secondEarned, secondSpent, err := readBalance(ctx, tx, secondWS)
	if err != nil {
		return err
	}

	// Map locked balances back to from/to semantics.
	var fromBal, fromEarned, fromSpent int64
	var toBal, toEarned, toSpent int64
	if fromWorkspace == firstWS {
		fromBal, fromEarned, fromSpent = firstBal, firstEarned, firstSpent
		toBal, toEarned, toSpent = secondBal, secondEarned, secondSpent
	} else {
		fromBal, fromEarned, fromSpent = secondBal, secondEarned, secondSpent
		toBal, toEarned, toSpent = firstBal, firstEarned, firstSpent
	}

	// Debit `from`.
	if fromBal < amount {
		return ErrInsufficientBalance
	}
	fromBalNew := fromBal - amount
	fromSpentNew := fromSpent + amount
	meta := map[string]interface{}{"counterparty": toWorkspace}
	if err := insertLedgerRow(ctx, tx, fromWorkspace, -amount, fromBalNew,
		TypeTransferOut, description, meta); err != nil {
		return err
	}
	if err := writeBalance(ctx, tx, fromWorkspace, fromBalNew, fromEarned, fromSpentNew); err != nil {
		return err
	}

	// Credit `to`.
	toBalNew := toBal + amount
	toEarnedNew := toEarned + amount
	metaIn := map[string]interface{}{"counterparty": fromWorkspace}
	if err := insertLedgerRow(ctx, tx, toWorkspace, amount, toBalNew,
		TypeTransferIn, description, metaIn); err != nil {
		return err
	}
	if err := writeBalance(ctx, tx, toWorkspace, toBalNew, toEarnedNew, toSpent); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Burn removes LENS from a workspace's balance and from
// circulating supply. Used when a workspace spends LENS on
// upstream AI calls (LENS-paid mode). Burns are irreversible.
func (s *LedgerStore) Burn(ctx context.Context, workspaceID string, amount int64, description string) error {
	if amount <= 0 {
		return errors.New("mining: burn amount must be positive")
	}
	if s.pool == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin burn: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	bal, earned, spent, err := readBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if bal < amount {
		return ErrInsufficientBalance
	}
	balNew := bal - amount
	spentNew := spent + amount
	if err := insertLedgerRow(ctx, tx, workspaceID, -amount, balNew, TypeBurn, description, nil); err != nil {
		return err
	}
	if err := writeBalance(ctx, tx, workspaceID, balNew, earned, spentNew); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// countedSupplyTypeList is the SINGLE SOURCE OF TRUTH for the types GetTotalSupply counts:
// the query reads it directly (type = ANY($1)), so a caller checking membership cannot
// drift from what supply actually sums.
//
// ⚠️ DELIBERATELY NOT EQUAL to mintTypeList — see mint_gate.go. These sets answer different
// questions: this one is "when did LENS enter circulation" (settlement), that one is "where
// does value first accrue to an earner" (the mint moment). Aligning them would either
// un-gate a held mint or double-gate a finalize.
//
// Everything here is a SETTLED / counted type. The _held types are absent by design (a held
// mint has not entered circulation); transfers and marketplace_fee move existing LENS;
// receipt_mine_provisional stays excluded per its own go-live treatment.
var countedSupplyTypeList = []string{
	TypeCacheMine,
	TypeComputeMine,
	TypeComputeFee, // B56.20: Talyvor's share of a compute earning; with the node's compute_mine it is the whole earning
	TypeEmbeddingMine,
	TypeAnnotationMine,
	TypePatternMine,
	TypePoolRoyalty,
	// The four P-o-I final types. Their µLENS were ALWAYS counted — until the finalize
	// sweeper was fixed they were summed under the pool_royalty label — so counting them
	// under their own labels keeps total supply byte-identical while making per-mint-type
	// supply honest. Dropping any of them would shrink supply and the LXC math reading it.
	TypeEvalContribution,
	TypeRoutingPrediction,
	TypeLatencyLocality,
	TypeConfidentialCompute,
	// Staking yield. The principal's return stays UNCOUNTED (type "unstake") because it was
	// already in supply when it was staked; only this newly-created half is a mint. Splitting the
	// row is what makes counting it possible without inflating supply on every unstake.
	TypeStakeYield,
}

// TypeConvertToLXC is the LENS→LXC conversion DEBIT. The canonical constant lives here,
// with the other ledger row types and with the two supply predicates that must classify it;
// internal/economy aliases it as LENSTypeConvertToLXC for the writer-side name (the same
// shape poolroyalty uses for TypePoolRoyalty).
//
// IT IS A BURN, NOT A TRANSFER. ConvertLENStoLXC debits the workspace's LENS and credits only
// LXC — a different token on a different ledger. Nothing holds that LENS afterwards: there is
// no treasury and deliberately no house account (LXC is a liability, and spending it is what
// discharges it). So the µLENS is destroyed, which puts it in burnedSupplyTypeList and NEVER
// in countedSupplyTypeList — adding it to the latter would retroactively un-mint work that
// really happened.
const TypeConvertToLXC = "convert_to_lxc"

// burnedSupplyTypeList is the SINGLE SOURCE OF TRUTH for the types that DESTROY LENS, in the
// same shape as countedSupplyTypeList above and for the same reason: GetCirculatingSupply and
// GetTotalBurned both read it directly, so the two public readouts cannot drift into reporting
// different circulating supplies.
//
// Membership test: does this row remove µLENS from a wallet with NO counterparty credited
// anywhere? Transfers, stake locks and marketplace moves all have a counterparty and are
// excluded; the _revoked types are excluded because held LENS never entered supply to begin
// with (and are negative, which the counted query's `amount > 0` filter drops independently).
var burnedSupplyTypeList = []string{
	TypeBurn,
	TypeStakeSlash,
	// convert_to_lxc was in NEITHER list until this change — excluded from total twice over
	// (absent from the allow-list AND negative) and absent here — so counted circulating stayed
	// FLAT while the wallet fell. ComputeFairRate reads both, and its algebra reduces to
	// circulating/totalMinted, so the omission overstated the rate pricing the NEXT conversion.
	TypeConvertToLXC,
}

// TypeStakeYield is the LENS newly created by a staking position's accrued yield.
//
// ⚠ IT IS A MINT, AND THE PRINCIPAL IS NOT. MarketplaceStore.Unstake used to credit
// principal+yield as ONE row of type "unstake", which is in no counted list — so the yield was
// real LENS in a wallet that GetTotalSupply never saw (found by #400's sweep, reported, unfixed
// then). The fix could NOT be "count unstake": the principal was already in supply when it was
// staked, so counting its return would inflate supply on every unstake — a worse error. The row
// is split instead, and only this half is counted.
//
// ⚠ AND IT IS THE ONE COUNTED MINT WITH NO GATED MINT MOMENT. It has no _held
// stage and is not in mintTypeList, so neither U6 control sees it: measured, an
// unverified workspace mints yield through a refusing verifier and past the
// rolling rate cap, which does not count it either. Pinned (with the reason it
// is not simply gated) in TestEveryCountedMintHasAGatedMintMoment; the decision
// is open in QUEUE.md W6.1.
const TypeStakeYield = "stake_yield"

// BurnedSupplyTypes returns the ledger types that reduce circulating supply. Exported so a
// guard test can pin every destructive label against the REAL list the queries use.
func BurnedSupplyTypes() []string {
	out := make([]string, len(burnedSupplyTypeList))
	copy(out, burnedSupplyTypeList)
	return out
}

// CountedSupplyTypes returns the ledger types GetTotalSupply counts. Exported so the
// finalize sweeper's guard test can pin every settled label it can write as counted —
// against the REAL list the query uses, not a copy.
func CountedSupplyTypes() []string {
	out := make([]string, len(countedSupplyTypeList))
	copy(out, countedSupplyTypeList)
	return out
}

// GetTotalSupply returns the all-time minted LENS — the sum of
// every credit-side ledger row that came from a mining track or the Pool-B
// royalty mint (Stage 2.2: pool_royalty counted so royalty LENS is honestly
// in supply). Explicit allow-list: transfers and marketplace_fee move
// existing LENS (not mints) and stay excluded; receipt_mine_provisional
// stays excluded per its own go-live treatment (PoVI preconditions).
//
// The four P-o-I final types (eval_contribution / eval_routing_prediction /
// eval_latency_locality / eval_confidential_compute) are counted here for the SAME
// reason pool_royalty is: settlement is when a held mint enters circulation. They were
// added when the finalize sweeper stopped labelling every settled mint pool_royalty —
// their µLENS were ALWAYS in supply (counted under the wrong label), so counting them
// under their own labels keeps total supply byte-identical. Attribution moved; the money
// did not. Dropping any of them from this list would silently shrink supply and the LXC
// conversion math that reads it.
//
// B25.1: test LENS — rows a test (synthetic) workspace wrote, marked `test` (migration 0173) — is never
// counted, here or in the burned total: supply and the backing value it prices are real figures.
func (s *LedgerStore) GetTotalSupply(ctx context.Context) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM lens_token_ledger
		WHERE amount > 0 AND type = ANY($1) AND NOT test`, countedSupplyTypeList)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("mining: total supply: %w", err)
	}
	return n, nil
}

// GetCirculatingSupply = total minted - total burned. The
// difference is what's currently in workspace wallets + staked.
//
// "Burned" is burnedSupplyTypeList — every type that removes µLENS from a
// wallet with NO counterparty credited: plain burns (TypeBurn), slashed stake
// (TypeStakeSlash — a slash destroys collateral, PoVI Part 3), and the LENS→LXC
// conversion debit (TypeConvertToLXC), whose LENS is destroyed against newly
// minted LXC on a different ledger. Supply feeds the LXC conversion math, so a
// destructive type missing from this list overstates circulating and therefore
// overstates the rate that prices the next conversion.
func (s *LedgerStore) GetCirculatingSupply(ctx context.Context) (int64, error) {
	total, err := s.GetTotalSupply(ctx)
	if err != nil {
		return 0, err
	}
	if s.pool == nil {
		return total, nil
	}
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(-amount), 0) FROM lens_token_ledger WHERE type = ANY($1) AND NOT test`,
		burnedSupplyTypeList)
	var burned int64
	if err := row.Scan(&burned); err != nil {
		return 0, fmt.Errorf("mining: burned: %w", err)
	}
	return total - burned, nil
}

// GetTotalBurned returns the cumulative LENS removed from supply. It reads the
// SAME burnedSupplyTypeList as GetCirculatingSupply, which is the point: the
// economy-stats display computes total − burned and must land on exactly the
// number GetCirculatingSupply reports. Two hand-maintained lists would be two
// different "circulating supply" figures on two screens.
func (s *LedgerStore) GetTotalBurned(ctx context.Context) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(-amount), 0) FROM lens_token_ledger WHERE type = ANY($1) AND NOT test`,
		burnedSupplyTypeList)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("mining: total burned: %w", err)
	}
	return n, nil
}

// ─── tx helpers shared by Transfer + Burn ───────

// readBalance + writeBalance + insertLedgerRow factor out the SQL
// the Transfer / Burn flows share. They take a pgx.Tx so the
// caller controls transactional semantics.

func readBalance(ctx context.Context, tx pgx.Tx, workspaceID string) (bal, earned, spent int64, err error) {
	if _, err = tx.Exec(ctx, `
		INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return 0, 0, 0, fmt.Errorf("mining: ensure balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_earned, lifetime_spent
		FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	if err = row.Scan(&bal, &earned, &spent); err != nil {
		return 0, 0, 0, fmt.Errorf("mining: read balance: %w", err)
	}
	return
}

func writeBalance(ctx context.Context, tx pgx.Tx, workspaceID string, bal, earned, spent int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1`, workspaceID, bal, earned, spent)
	if err != nil {
		return fmt.Errorf("mining: update balance: %w", err)
	}
	return nil
}

func insertLedgerRow(ctx context.Context, tx pgx.Tx, workspaceID string, delta, balanceAfter int64,
	txType, description string, metadata map[string]interface{}) error {
	meta, err := dbjson.Marshal(metadata) // JSON text on both protocols (#133)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger
			(workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, workspaceID, delta, balanceAfter, txType, description, meta); err != nil {
		return fmt.Errorf("mining: insert ledger row: %w", err)
	}
	return nil
}

// GetHistory returns the ledger entries for a workspace, newest
// first. Bounds: limit ≤ 200, offset ≥ 0.
// requesterMetaKey is the counterparty workspace id some mint writers stamp into
// a CONTRIBUTOR's ledger row (cache_mine; any pre-#155 pool-royalty held rows).
// It is FUNCTIONALLY read by CountCacheHitsBenefited via SQL on the STORED row,
// so it must stay stored — but it must never be echoed to the tenant in history.
const requesterMetaKey = "request_workspace_id"

// servedToPattern matches the trailing "served to <workspace>" leak shape — the
// cache_mine "cache hit (X) served to <requester>" description. Targeted (not a
// blind replace of the requester value, which could blunder on short/common
// substrings and wrongly couple the two vectors).
var servedToPattern = regexp.MustCompile(`served to \S+$`)

// maskHistoryEntry scrubs counterparty identity from a ledger entry BEFORE it is
// returned to a tenant (#145 family). Two INDEPENDENT vectors:
//   - metadata: drop request_workspace_id — GENERIC, fires on the key's presence
//     for any row type (defense in depth vs future leaky writers).
//   - description: neutralize a trailing "served to <workspace>".
//
// It mutates only the in-memory entry (a per-row value with a freshly-unmarshaled
// metadata map); the stored DB row and the SQL counter that reads
// request_workspace_id are provably untouched. Retroactive: masks historic rows
// on read, no backfill.
func maskHistoryEntry(e *LedgerEntry) {
	delete(e.Metadata, requesterMetaKey) // safe on nil / absent
	if servedToPattern.MatchString(e.Description) {
		e.Description = servedToPattern.ReplaceAllString(e.Description, "served to another workspace")
	}
}

func (s *LedgerStore) GetHistory(ctx context.Context, workspaceID string, limit, offset int) ([]LedgerEntry, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, amount, balance_after, type, description, metadata, created_at
		FROM lens_token_ledger
		WHERE workspace_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("mining: query history: %w", err)
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var meta []byte
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.Amount, &e.BalanceAfter,
			&e.Type, &e.Description, &meta, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("mining: scan history: %w", err)
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &e.Metadata)
		}
		maskHistoryEntry(&e) // #145: scrub counterparty identity from the tenant echo
		out = append(out, e)
	}
	return out, rows.Err()
}
