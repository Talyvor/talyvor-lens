package mining

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// micro converts a whole-LENS test value to its integer µLENS count (SEC-2:
// 1 LENS = 1e6 µLENS). Ledger balances/amounts are int64 µLENS now.
func micro(lens float64) int64 { return int64(lens * 1e6) }

// expectCreditOrDebit programmes the mock with one full Begin → ensure balance
// row (INSERT DO NOTHING) → FOR UPDATE read → INSERT ledger → UPDATE balance →
// Commit cycle. startingBalance is what the FOR UPDATE SELECT returns. All
// amounts are integer µLENS.
func expectCreditOrDebit(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	startingBalance, startingEarned, startingSpent int64,
	delta, expectedBalance, expectedEarned, expectedSpent int64,
) {
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(startingBalance, startingEarned, startingSpent))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs(workspaceID, delta, expectedBalance, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs(workspaceID, expectedBalance, expectedEarned, expectedSpent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
}

// expectCreditOnce programmes the mock for a CreditOnce (U6 idempotent mint):
// Begin → claim INSERT (mint_idempotency, RowsAffected=1 = first claim) → the
// same credit cycle as expectCreditOrDebit's body → Commit. The nil-verifier
// path adds no SQL (verifyEarn is a no-op without a wired verifier).
func expectCreditOnce(
	mock pgxmock.PgxPoolIface,
	requestID, workspaceID, mintType string,
	startingBalance, startingEarned, startingSpent int64,
	delta, expectedBalance, expectedEarned, expectedSpent int64,
) {
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO mint_idempotency").
		WithArgs(requestID, workspaceID, mintType, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(startingBalance, startingEarned, startingSpent))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs(workspaceID, delta, expectedBalance, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs(workspaceID, expectedBalance, expectedEarned, expectedSpent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
}

func newMockStore(t *testing.T) (*LedgerStore, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return newLedgerStore(mock), mock
}

// ─── Credit / Debit ──────────────────────────────

func TestCredit_IncreasesBalance(t *testing.T) {
	store, mock := newMockStore(t)
	// Starting at 0, credit micro(0.010) → balance micro(0.010), lifetime_earned 0.010.
	expectCreditOrDebit(mock, "ws_c", 0, 0, 0, micro(0.010), micro(0.010), micro(0.010), 0)
	err := store.Credit(context.Background(), "ws_c", micro(0.010), TypeCacheMine, "", nil)
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDebit_DecreasesBalance(t *testing.T) {
	store, mock := newMockStore(t)
	// Starting at micro(1.0), debit micro(0.25) → balance micro(0.75), lifetime_spent 0.25.
	expectCreditOrDebit(mock, "ws_d", micro(1.0), micro(1.0), 0, -micro(0.25), micro(0.75), micro(1.0), micro(0.25))
	err := store.Debit(context.Background(), "ws_d", micro(0.25), TypeSpend, "", nil)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}
}

func TestDebit_InsufficientBalance(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_e").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_e").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(micro(0.05), micro(0.05), micro(0.0)))
	mock.ExpectRollback()
	err := store.Debit(context.Background(), "ws_e", micro(0.50), TypeSpend, "", nil)
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestCredit_RejectsZeroOrNegative(t *testing.T) {
	store, _ := newMockStore(t)
	if err := store.Credit(context.Background(), "ws", 0, TypeCacheMine, "", nil); err == nil {
		t.Fatal("expected error for zero credit")
	}
	if err := store.Credit(context.Background(), "ws", -1, TypeCacheMine, "", nil); err == nil {
		t.Fatal("expected error for negative credit")
	}
}

// ─── GetBalance / GetSnapshot ────────────────────

func TestGetBalance_ReturnsZeroForNew(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery("SELECT balance FROM lens_token_balances").
		WithArgs("ws_new").
		WillReturnError(errNoRows())
	b, err := store.GetBalance(context.Background(), "ws_new")
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if b != 0 {
		t.Fatalf("expected micro(0.0) for new workspace, got %d", b)
	}
}

func TestGetSnapshot_NewWorkspaceReturnsZeroSnapshot(t *testing.T) {
	store, mock := newMockStore(t)
	// held_balance joined the select when the snapshot gained the third state — earned but not
	// yet spendable. See held_snapshot_integration_test.go.
	mock.ExpectQuery("SELECT workspace_id, balance, held_balance, lifetime_earned").
		WithArgs("ws_snap").
		WillReturnError(errNoRows())
	snap, err := store.GetSnapshot(context.Background(), "ws_snap")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.WorkspaceID != "ws_snap" || snap.Balance != 0 || snap.HeldBalance != 0 {
		t.Fatalf("expected zero snapshot, got %+v", snap)
	}
}

// ─── GetHistory ──────────────────────────────────

func TestGetHistory_PaginatedResults(t *testing.T) {
	store, mock := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT id, workspace_id, amount, balance_after").
		WithArgs("ws_h", 10, 20).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "balance_after",
			"type", "description", "metadata", "created_at",
		}).
			AddRow("e1", "ws_h", micro(0.010), micro(0.010), TypeCacheMine, "hit", []byte(`{"hit_type":"exact"}`), now).
			AddRow("e2", "ws_h", micro(0.001), micro(0.011), TypeCacheMine, "hit", []byte(`{}`), now))
	entries, err := store.GetHistory(context.Background(), "ws_h", 10, 20)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "e1" || entries[0].Amount != micro(0.010) {
		t.Fatalf("unexpected entry[0]: %+v", entries[0])
	}
	if entries[0].Metadata["hit_type"] != "exact" {
		t.Fatalf("metadata not unmarshalled, got %+v", entries[0].Metadata)
	}
}

// #145 family: GetHistory must MASK the requester identity in the tenant echo. A
// contributor's cache_mine row carried the requester in BOTH the description
// ("served to <requester>") and metadata.request_workspace_id, and GetHistory
// echoed both verbatim. Assert on the SERIALIZED JSON — the actual tenant
// boundary.
func TestGetHistory_MasksRequesterIdentity(t *testing.T) {
	store, mock := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT id, workspace_id, amount, balance_after").
		WithArgs("ws_owner", 20, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "balance_after",
			"type", "description", "metadata", "created_at",
		}).AddRow("e1", "ws_owner", micro(0.010), micro(0.010), TypeCacheMine,
			"cache hit (exact) served to wsB",
			[]byte(`{"hit_type":"exact","request_workspace_id":"wsB"}`), now))

	entries, err := store.GetHistory(context.Background(), "ws_owner", 0, 0)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	b, _ := json.Marshal(entries[0])
	if s := string(b); strings.Contains(s, "request_workspace_id") || strings.Contains(s, "wsB") {
		t.Errorf("LEAK: tenant history JSON still names the requester: %s", s)
	}
	// Passthrough: non-sensitive fields survive the mask.
	if entries[0].Metadata["hit_type"] != "exact" {
		t.Errorf("hit_type masked away (must survive): %+v", entries[0].Metadata)
	}
	if entries[0].Amount != micro(0.010) || entries[0].Type != TypeCacheMine || entries[0].BalanceAfter != micro(0.010) {
		t.Errorf("non-sensitive fields altered: %+v", entries[0])
	}
	if !strings.Contains(entries[0].Description, "served to another workspace") {
		t.Errorf("description not masked: %q", entries[0].Description)
	}
}

// The mask is GENERIC (keyed on request_workspace_id, not type='cache_mine') — a
// non-cache_mine row carrying the key is masked too (defense in depth), and
// non-sensitive metadata keys are left intact.
func TestGetHistory_MaskIsGeneric(t *testing.T) {
	store, mock := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT id, workspace_id, amount, balance_after").
		WithArgs("ws_owner", 20, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "balance_after",
			"type", "description", "metadata", "created_at",
		}).AddRow("e1", "ws_owner", micro(1.0), micro(1.0), "some_future_type",
			"a future credit", []byte(`{"request_workspace_id":"wsB","other":"keep"}`), now))

	entries, err := store.GetHistory(context.Background(), "ws_owner", 0, 0)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if _, leaked := entries[0].Metadata["request_workspace_id"]; leaked {
		t.Error("generic mask failed: request_workspace_id survived on a non-cache_mine row")
	}
	if entries[0].Metadata["other"] != "keep" {
		t.Error("generic mask over-reached: stripped a non-sensitive key")
	}
}

// errNoRows is the pgx.ErrNoRows sentinel surfaced through
// pgxmock — saves importing pgx directly in the test file.
func errNoRows() error { return errPgxNoRows }

// ─── Transfer ───────────────────────────────────────────────────────────────

// expectReadBalance programmes one readBalance sub-sequence inside a
// Transfer transaction: the INSERT DO NOTHING that ensures the row exists,
// followed by the SELECT … FOR UPDATE that acquires the pessimistic lock.
func expectReadBalance(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	bal, earned, spent int64,
) {
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(bal, earned, spent))
}

// TestTransfer_HappyPath verifies that a straightforward transfer debits the
// sender and credits the recipient atomically (normal alphabetical order —
// from < to, so no swap needed).
func TestTransfer_HappyPath(t *testing.T) {
	// Transfer("ws_a", "ws_b", micro(0.5), "test")
	// Lex order: ws_a < ws_b → ws_a locked first (no swap), ws_b second.
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectReadBalance(mock, "ws_a", micro(1.0), micro(1.0), 0) // first lock: ws_a (from)
	expectReadBalance(mock, "ws_b", micro(0.2), micro(0.2), 0) // second lock: ws_b (to)

	// debit ws_a: amount -micro(0.5), newBal micro(0.5), spent 0→micro(0.5)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_a", -micro(0.5), micro(0.5), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_a", micro(0.5), micro(1.0), micro(0.5)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// credit ws_b: amount +micro(0.5), newBal micro(0.7), earned micro(0.2)→micro(0.7)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_b", micro(0.5), micro(0.7), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_b", micro(0.7), micro(0.7), micro(0.0)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.Transfer(context.Background(), "ws_a", "ws_b", micro(0.5), "test"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestTransfer_LockOrderIsLexicographic is the deadlock-regression guard.
//
// It calls Transfer("ws_b", "ws_a", …) where from > to alphabetically.
// The fix must still acquire ws_a (the lex-smaller ID) FIRST before ws_b,
// regardless of which direction LENS is flowing.
//
// If the fix ever regresses to caller-order locking, the mock will surface an
// unexpected query (ws_b before ws_a) and the test fails immediately — no need
// to construct actual concurrent transactions.
func TestTransfer_LockOrderIsLexicographic(t *testing.T) {
	// Transfer("ws_b" → "ws_a", micro(0.5))  —  from > to alphabetically.
	// Correct: lock ws_a first, then ws_b (lex order), regardless of flow.
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectReadBalance(mock, "ws_a", micro(0.2), micro(0.2), 0) // MUST be ws_a first
	expectReadBalance(mock, "ws_b", micro(1.0), micro(1.0), 0) // ws_b second

	// debit ws_b (from): amount -micro(0.5), newBal micro(0.5), spent 0→micro(0.5)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_b", -micro(0.5), micro(0.5), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_b", micro(0.5), micro(1.0), micro(0.5)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// credit ws_a (to): amount +micro(0.5), newBal micro(0.7), earned micro(0.2)→micro(0.7)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_a", micro(0.5), micro(0.7), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_a", micro(0.7), micro(0.7), micro(0.0)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.Transfer(context.Background(), "ws_b", "ws_a", micro(0.5), "test"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestTransfer_InsufficientBalance verifies ErrInsufficientBalance is returned
// when the sender's balance is too low. Both locks are acquired before the
// balance check (the fix acquires all locks first), so both readBalance calls
// appear in the mock before the rollback.
func TestTransfer_InsufficientBalance(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectReadBalance(mock, "ws_a", micro(0.1), micro(0.1), 0) // from=ws_a, only micro(0.1) available
	expectReadBalance(mock, "ws_b", micro(1.0), micro(1.0), 0)
	mock.ExpectRollback()

	err := store.Transfer(context.Background(), "ws_a", "ws_b", micro(0.5), "test")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestTransfer_Validation covers the guard-clause fast-paths that reject bad
// inputs before touching the database.
func TestTransfer_Validation(t *testing.T) {
	store, _ := newMockStore(t)
	ctx := context.Background()

	if err := store.Transfer(ctx, "ws_a", "ws_b", micro(0.0001), ""); err == nil {
		t.Fatal("expected error: amount below minimum")
	}
	if err := store.Transfer(ctx, "", "ws_b", micro(1.0), ""); err == nil {
		t.Fatal("expected error: empty from workspace")
	}
	if err := store.Transfer(ctx, "ws_a", "", micro(1.0), ""); err == nil {
		t.Fatal("expected error: empty to workspace")
	}
	if err := store.Transfer(ctx, "ws_a", "ws_a", micro(1.0), ""); err == nil {
		t.Fatal("expected error: self-transfer")
	}
}

// STAGE micro(2.2)(b) SUPPLY COUNTING: pool_royalty joins the minted-supply
// allow-list — a royalty mint is LENS entering circulation and must be
// counted honestly. The list stays an explicit allow-list: marketplace_fee
// (moves existing LENS, doesn't mint) and receipt_mine_provisional (povi's
// deliberate exclusion pending its own go-live call) remain OUT.
func TestGetTotalSupply_CountsPoolRoyalty_ExcludesNonMints(t *testing.T) {
	s, mock := newMockStore(t)

	// The query passes the allow-list as ONE array arg (type = ANY($1)), so pinning the arg
	// pins the real list — countedSupplyTypeList is what GetTotalSupply itself reads.
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(amount\), 0\)`).
		WithArgs(countedSupplyTypeList).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(micro(42.5)))

	got, err := s.GetTotalSupply(context.Background())
	if err != nil {
		t.Fatalf("GetTotalSupply: %v", err)
	}
	if got != micro(42.5) {
		t.Errorf("supply = %v, want micro(42.5)", got)
	}

	// Pin the allow-list MEMBERSHIP exactly, against the real list rather than a copy of it
	// (a hand-copied list here could agree with itself while the query drifted). Counting is
	// how LENS enters supply, so both directions matter: an omission understates supply, an
	// accidental addition (e.g. a _held type) double-counts a mint that has not settled.
	wantCounted := []string{
		TypeCacheMine, TypeComputeMine, TypeEmbeddingMine, TypeAnnotationMine, TypePatternMine,
		TypePoolRoyalty,
		// Talyvor's compute fee (B56.20): the share of a node earning Talyvor keeps. With the node's
		// compute_mine it is the whole earning, so counting both keeps supply what it was before the fee.
		TypeComputeFee,
		// The four P-o-I final types: settled by the table-parameterized FinalizeSweeper,
		// each under its OWN label. They must be counted for exactly the reason pool_royalty
		// is — settlement is the moment the mint enters circulation. Before the sweeper was
		// fixed these µLENS were counted under the pool_royalty label; they are the SAME
		// µLENS, so this list keeps total supply identical while attributing it honestly.
		TypeEvalContribution, TypeRoutingPrediction, TypeLatencyLocality, TypeConfidentialCompute,
		// Staking YIELD. computeYield creates LENS that existed nowhere before, and it was
		// invisible to supply because Unstake credited principal+yield as one row of type
		// "unstake" — a type that is (correctly) in no counted list. The row is now split and
		// only this half is counted.
		//
		// ⚠ THE EXCLUSION BELOW IS THE OTHER HALF OF THIS DECISION: "unstake" itself must stay
		// OUT. The principal was already in supply when it was staked, so counting its return
		// would inflate supply by the whole stake on every unstake — a worse error than the one
		// this fixes, and the tempting one-line "fix".
		TypeStakeYield,
	}
	if len(countedSupplyTypeList) != len(wantCounted) {
		t.Errorf("counted-supply allow-list has %d types, want %d:\n got: %v\nwant: %v",
			len(countedSupplyTypeList), len(wantCounted), countedSupplyTypeList, wantCounted)
	}
	counted := map[string]bool{}
	for _, c := range countedSupplyTypeList {
		counted[c] = true
	}
	for _, w := range wantCounted {
		if !counted[w] {
			t.Errorf("type %q MUST be in the minted-supply allow-list (an omission silently shrinks supply)", w)
		}
	}

	// Guard the exclusions explicitly so a future edit can't sneak them in: the counted set
	// must NOT contain these. The _held types are the sharp edge — counting a held mint would
	// put un-settled LENS into supply.
	excluded := []string{"marketplace_fee", "receipt_mine_provisional", TypeBurn, TypeStakeSlash, TypeTransferIn,
		// "unstake" returns principal that never left supply; "stake" locks it. Neither is a mint.
		"unstake", "stake",
		TypePoolRoyaltyHeld, TypePoolRoyaltyRevoked,
		TypeEvalContributionHeld, TypeRoutingPredictionHeld, TypeLatencyLocalityHeld, TypeConfidentialComputeHeld}
	for _, ex := range excluded {
		if counted[ex] {
			t.Errorf("type %q must NOT be in the minted-supply allow-list", ex)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
