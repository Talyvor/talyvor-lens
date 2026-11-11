package economy

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG proofs for the F4-capstone step-A substrate: the per-scoped-key LXC sub-budget + the exactly-once
// agent debit. Uses a real pool (the package TestMain isolates search_path=lens_it_economy) — concurrency
// (FOR UPDATE + ON CONFLICT serialization) cannot be proven against a mock.

func agentHarness(t *testing.T) *DualTokenStore {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG agent sub-budget test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		// SEC-2: LXC columns are BIGINT µLXC.
		`CREATE TABLE IF NOT EXISTS lxc_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			lifetime_minted BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0,
			-- migration 0115: the cash-backed portion of the balance. These fixtures hand-roll the
			-- schema instead of migrating, so a column added by a migration has to be added here too.
			cash_backed_ulxc BIGINT NOT NULL DEFAULT 0, test_funded_ulxc BIGINT NOT NULL DEFAULT 0, -- 0158
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
		`CREATE TABLE IF NOT EXISTS lxc_spend_claims (request_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			lxc_amount BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`ALTER TABLE lxc_spend_claims ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0`, // 0197 (B32.11)
		`ALTER TABLE lxc_spend_claims ADD COLUMN IF NOT EXISTS question_id TEXT`,                            // 0374 (B58.3)
		// 0158: the LXC balance writer reads the workspace's open holds.
		`CREATE TABLE IF NOT EXISTS lxc_reservations (reservation_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, held_ulxc BIGINT NOT NULL, settled_ulxc BIGINT,
			status TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','released')),
			requested_model TEXT, request_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ)`,
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0`, // 0197 (B32.11)
		`TRUNCATE lxc_balances, lxc_ledger, agent_lxc_subbudgets, lxc_spend_claims`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return NewDualTokenStore(nil, pool, nil)
}

func fund(t *testing.T, s *DualTokenStore, ws string, lxc int64) {
	t.Helper()
	if _, err := s.CreditLXC(context.Background(), ws, lxc, "test top-up", nil); err != nil {
		t.Fatal(err)
	}
}

func agentState(t *testing.T, s *DualTokenStore, ws, key string) (bal, spent int64, claims int) {
	t.Helper()
	ctx := context.Background()
	bal, _ = s.GetLXCBalance(ctx, ws)
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(spent_lxc,0) FROM agent_lxc_subbudgets WHERE scoped_key_id=$1`, key).Scan(&spent)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM lxc_spend_claims WHERE scoped_key_id=$1`, key).Scan(&claims)
	return
}

// (proof 1) IDEMPOTENT: same request_id twice ⇒ ONE debit; concurrent same request_id ⇒ exactly one debit.
func TestAgentSpend_Idempotent_And_Concurrent(t *testing.T) {
	s := agentHarness(t)
	ctx := context.Background()
	fund(t, s, "wsA", 100*uLXC)

	// same request_id twice (sequential retry) ⇒ one debit of 10.
	if err := s.SpendLXCForAgent(ctx, "keyA", "wsA", "req-1", 10*uLXC, "task", AgentDebitMeta{}); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if err := s.SpendLXCForAgent(ctx, "keyA", "wsA", "req-1", 10*uLXC, "task", AgentDebitMeta{}); err != nil {
		t.Fatalf("idempotent replay must be nil, got %v", err)
	}
	if bal, spent, claims := agentState(t, s, "wsA", "keyA"); bal != 90*uLXC || spent != 10*uLXC || claims != 1 {
		t.Fatalf("retry must debit ONCE: bal=%d spent=%d claims=%d, want 90/10/1 (x1e6)", bal, spent, claims)
	}

	// concurrent same request_id (two goroutines) ⇒ exactly one debit of 5.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.SpendLXCForAgent(ctx, "keyA", "wsA", "req-2", 5*uLXC, "task", AgentDebitMeta{})
		}()
	}
	wg.Wait()
	if bal, spent, _ := agentState(t, s, "wsA", "keyA"); bal != 85*uLXC || spent != 15*uLXC {
		t.Fatalf("concurrent same-request_id must debit ONCE (5): bal=%d spent=%d, want 85/15 (x1e6)", bal, spent)
	}
	var claim2 int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM lxc_spend_claims WHERE request_id='req-2'`).Scan(&claim2)
	if claim2 != 1 {
		t.Fatalf("req-2 must have exactly one claim, got %d", claim2)
	}
}

// (proof 2) CEILING (depleting): spending halts EXACTLY at the ceiling; the over-remaining debit is rejected.
func TestAgentSpend_CeilingDepletes(t *testing.T) {
	s := agentHarness(t)
	ctx := context.Background()
	fund(t, s, "wsB", 1000*uLXC)                                           // plenty of balance
	if err := s.SetAgentCeiling(ctx, "keyB", "wsB", 50*uLXC); err != nil { // ceiling 50
		t.Fatal(err)
	}
	// spend 20 + 20 = 40 (ok), then 20 would exceed (40+20>50) ⇒ REJECT; then 10 (40+10=50) ok; then any ⇒ reject.
	if err := s.SpendLXCForAgent(ctx, "keyB", "wsB", "b1", 20*uLXC, "t", AgentDebitMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SpendLXCForAgent(ctx, "keyB", "wsB", "b2", 20*uLXC, "t", AgentDebitMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SpendLXCForAgent(ctx, "keyB", "wsB", "b3", 20*uLXC, "t", AgentDebitMeta{}); err != ErrSubBudgetExceeded {
		t.Fatalf("over-ceiling debit must be ErrSubBudgetExceeded, got %v", err)
	}
	if err := s.SpendLXCForAgent(ctx, "keyB", "wsB", "b4", 10*uLXC, "t", AgentDebitMeta{}); err != nil { // exactly to 50
		t.Fatalf("spend to exactly the ceiling must succeed, got %v", err)
	}
	if err := s.SpendLXCForAgent(ctx, "keyB", "wsB", "b5", uLXC/100, "t", AgentDebitMeta{}); err != ErrSubBudgetExceeded {
		t.Fatalf("any spend past the ceiling must reject, got %v", err)
	}
	_, spent, _ := agentState(t, s, "wsB", "keyB")
	if spent != 50*uLXC {
		t.Fatalf("spent_lxc must halt EXACTLY at ceiling 50 (x1e6), got %d", spent)
	}
}

// (proof 3) ATOMICITY: a REJECTED spend leaves NO orphan claim and NO debit — claim+debit roll back together.
func TestAgentSpend_Atomicity_NoOrphan(t *testing.T) {
	s := agentHarness(t)
	ctx := context.Background()
	fund(t, s, "wsC", 100*uLXC)
	_ = s.SetAgentCeiling(ctx, "keyC", "wsC", 5*uLXC) // tiny ceiling

	err := s.SpendLXCForAgent(ctx, "keyC", "wsC", "c1", 10*uLXC, "t", AgentDebitMeta{}) // exceeds ceiling 5
	if err != ErrSubBudgetExceeded {
		t.Fatalf("want ErrSubBudgetExceeded, got %v", err)
	}
	bal, spent, claims := agentState(t, s, "wsC", "keyC")
	if bal != 100*uLXC || spent != 0 || claims != 0 {
		t.Fatalf("REJECT must leave NOTHING changed: bal=%d spent=%d claims=%d, want 100/0/0 (x1e6; no orphan claim, no debit)", bal, spent, claims)
	}
}

// (proof 4) ZERO-BALANCE: flag-on + ceiling set + workspace LXC balance zero ⇒ spend REJECTED, nothing debits.
func TestAgentSpend_ZeroBalance_SafeWhenOn(t *testing.T) {
	s := agentHarness(t)
	ctx := context.Background()
	_ = s.SetAgentCeiling(ctx, "keyD", "wsD", 50*uLXC) // ceiling set, but NO funding
	err := s.SpendLXCForAgent(ctx, "keyD", "wsD", "d1", 10*uLXC, "t", AgentDebitMeta{})
	if err != ErrInsufficientLXC {
		t.Fatalf("zero-balance spend must be ErrInsufficientLXC, got %v", err)
	}
	if bal, spent, claims := agentState(t, s, "wsD", "keyD"); bal != 0 || spent != 0 || claims != 0 {
		t.Fatalf("zero-balance reject must debit nothing: bal=%d spent=%d claims=%d", bal, spent, claims)
	}
}
