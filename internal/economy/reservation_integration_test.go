package economy

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG proofs for the agent-allocation RESERVATION lifecycle (billing redesign). Money is asserted on
// lxc_ledger ROWS + AMOUNTS and the workspace BALANCE — never a status code. Concurrency (the FOR UPDATE
// status-CAS + ON CONFLICT hold claim) cannot be proven against a mock, so this needs a real pool.

func reservationHarness(t *testing.T) *DualTokenStore {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG reservation test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS lxc_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0, cash_backed_ulxc BIGINT NOT NULL DEFAULT 0, test_funded_ulxc BIGINT NOT NULL DEFAULT 0, -- 0158 
			lifetime_minted BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS lxc_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
			metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS agent_lxc_subbudgets (scoped_key_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL,
			ceiling_lxc BIGINT NOT NULL DEFAULT 50000000, spent_lxc BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		// B19.1: the spend path asks whether the key belongs to an agent account (migration 0140).
		`CREATE TABLE IF NOT EXISTS agent_accounts (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS agent_account_keys (scoped_key_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		// B19.13: the workspace's own spend reads what its agents hold.
		`CREATE TABLE IF NOT EXISTS agent_postings (workspace_id TEXT NOT NULL, account TEXT NOT NULL, amount_ulxc BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS agent_account_balances (workspace_id TEXT NOT NULL, account TEXT NOT NULL, balance_ulxc BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lxc_reservations (reservation_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, held_ulxc BIGINT NOT NULL, settled_ulxc BIGINT,
			status TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','released')),
			requested_model TEXT, request_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ)`,
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0`, // 0197 (B32.11)
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS replays INTEGER NOT NULL DEFAULT 0`,          // 0385 (B50.47)
		`TRUNCATE lxc_balances, lxc_ledger, agent_lxc_subbudgets, lxc_reservations`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return newDualTokenStore(nil, pool, nil)
}

func resFund(t *testing.T, s *DualTokenStore, ws string, ulxc int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, $2, $2)
		 ON CONFLICT (workspace_id) DO UPDATE SET balance = EXCLUDED.balance, cash_backed_ulxc = EXCLUDED.cash_backed_ulxc`, ws, ulxc); err != nil {
		t.Fatal(err)
	}
}

func resBalance(t *testing.T, s *DualTokenStore, ws string) int64 {
	t.Helper()
	b, err := s.GetLXCBalance(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func resLedgerRows(t *testing.T, s *DualTokenStore, ws string) []struct {
	amount int64
	typ    string
} {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT amount, type FROM lxc_ledger WHERE workspace_id = $1 ORDER BY id`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		amount int64
		typ    string
	}
	for rows.Next() {
		var r struct {
			amount int64
			typ    string
		}
		if err := rows.Scan(&r.amount, &r.typ); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func resAgentSpent(t *testing.T, s *DualTokenStore, key string) int64 {
	t.Helper()
	var spent int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1`, key).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	return spent
}

// TestReserveHoldsExactly: a hold debits exactly `held`, writes ONE reservation_hold ledger row, bumps the
// agent's spent by `held`, and marks the reservation held.
func TestReserveHoldsExactly(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	if err := s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000, AgentDebitMeta{RequestedModel: "m", RequestID: "rq1"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if b := resBalance(t, s, "ws"); b != 700_000 {
		t.Fatalf("balance after hold = %d, want 700000", b)
	}
	rows := resLedgerRows(t, s, "ws")
	if len(rows) != 1 || rows[0].amount != -300_000 || rows[0].typ != LXCTypeReservationHold {
		t.Fatalf("hold ledger = %+v, want one -300000 %s", rows, LXCTypeReservationHold)
	}
	if sp := resAgentSpent(t, s, "agent"); sp != 300_000 {
		t.Fatalf("agent spent = %d, want 300000", sp)
	}
}

// TestSettleBillsDeliveredRefundsRest: THE money invariant. Hold 300k, settle at the delivered 120k → the
// balance nets −120k (not −300k), the ledger shows release(+300k)+spend(−120k), and the agent's spent nets
// to 120k. The customer pays what was delivered; the reservation's excess returns.
func TestSettleBillsDeliveredRefundsRest(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	_ = s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000, AgentDebitMeta{RequestID: "rq1"})
	if _, _, err := s.SettleLXCReservation(ctx, "res1", 120_000, AgentDebitMeta{RequestID: "rq1"}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if b := resBalance(t, s, "ws"); b != 880_000 {
		t.Fatalf("balance after settle = %d, want 880000 (charged 120000, not 300000)", b)
	}
	rows := resLedgerRows(t, s, "ws")
	// hold(-300k) · release(+300k) · spend(-120k)
	if len(rows) != 3 ||
		rows[1].amount != 300_000 || rows[1].typ != LXCTypeReservationRelease ||
		rows[2].amount != -120_000 || rows[2].typ != LXCTypeSpend {
		t.Fatalf("settle ledger = %+v", rows)
	}
	if sp := resAgentSpent(t, s, "agent"); sp != 120_000 {
		t.Fatalf("agent spent after settle = %d, want 120000", sp)
	}
}

// TestReleaseRefundsInFull: an own-cache hit / failed serve. Hold 300k, release → balance restored to the
// original, ledger nets to zero, agent spent back to zero. The customer pays NOTHING.
func TestReleaseRefundsInFull(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	_ = s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000, AgentDebitMeta{})
	if err := s.ReleaseLXCReservation(ctx, "res1", "cache hit"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if b := resBalance(t, s, "ws"); b != 1_000_000 {
		t.Fatalf("balance after release = %d, want 1000000 (free)", b)
	}
	if sp := resAgentSpent(t, s, "agent"); sp != 0 {
		t.Fatalf("agent spent after release = %d, want 0", sp)
	}
}

// TestCeilingBlocksBeforeHold: an over-ceiling hold is rejected with no debit — the ceiling stays enforced
// against the conservative reservation, pre-serve. Balance and ledger are untouched.
func TestCeilingBlocksBeforeHold(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 100_000_000) // funded well beyond the ceiling
	if err := s.SetAgentCeiling(ctx, "agent", "ws", 500_000); err != nil {
		t.Fatal(err)
	}
	err := s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 600_000, AgentDebitMeta{}) // > 500k ceiling
	if err != ErrSubBudgetExceeded {
		t.Fatalf("over-ceiling reserve = %v, want ErrSubBudgetExceeded", err)
	}
	if b := resBalance(t, s, "ws"); b != 100_000_000 {
		t.Fatalf("balance after blocked hold = %d, want unchanged", b)
	}
	if len(resLedgerRows(t, s, "ws")) != 0 {
		t.Fatal("a blocked hold must write no ledger row")
	}
}

// TestExactlyOnce: a replayed hold debits once; a replayed settle bills once; a release after settle is a
// no-op. The balance reflects exactly one delivered charge.
func TestExactlyOnce(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	for i := 0; i < 3; i++ { // replay the hold
		_ = s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000, AgentDebitMeta{})
	}
	if b := resBalance(t, s, "ws"); b != 700_000 {
		t.Fatalf("balance after 3x hold(same id) = %d, want 700000 (once)", b)
	}
	for i := 0; i < 3; i++ { // replay the settle
		if _, _, err := s.SettleLXCReservation(ctx, "res1", 120_000, AgentDebitMeta{}); err != nil {
			t.Fatalf("settle replay %d: %v", i, err)
		}
	}
	_ = s.ReleaseLXCReservation(ctx, "res1", "late") // release after settle: no-op
	if b := resBalance(t, s, "ws"); b != 880_000 {
		t.Fatalf("balance after replayed settle + late release = %d, want 880000 (one 120000 charge)", b)
	}
}

// TestStrandedSweeperRefunds: a hold with no settle (a crash) is swept to a full refund — never auto-settled.
func TestStrandedSweeperRefunds(t *testing.T) {
	s := reservationHarness(t)
	ctx := context.Background()
	resFund(t, s, "ws", 1_000_000)
	_ = s.ReserveLXCForAgent(ctx, "agent", "ws", "res1", 300_000, AgentDebitMeta{})
	// Age the hold so the sweeper considers it stranded.
	if _, err := s.pool.Exec(ctx, `UPDATE lxc_reservations SET created_at = now() - interval '1 hour' WHERE reservation_id = 'res1'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReleaseStrandedReservations(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if b := resBalance(t, s, "ws"); b != 1_000_000 {
		t.Fatalf("balance after sweep = %d, want 1000000 (crash refunds the customer)", b)
	}
}
