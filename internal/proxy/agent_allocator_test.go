package proxy

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talyvor/lens/internal/economy"
)

// F4-capstone step C.1 — the agent allocator. Pre-serve estimate-debit via SpendLXCForAgent, server-derived
// debit key, ceiling airtight under concurrency. Real PG (the debit is the money-critical path).

const agentTestModel = "gpt-4o" // priced in alerts.CostUSD, so lxcEstimate > 0

func agentAllocHarness(t *testing.T) (*Proxy, *economy.DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG agent allocator test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS lxc_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			lifetime_minted BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0,
			-- migration 0115: the cash-backed portion of the balance. These fixtures hand-roll the
			-- schema instead of migrating, so a column added by a migration has to be added here too.
			cash_backed_ulxc BIGINT NOT NULL DEFAULT 0, test_funded_ulxc BIGINT NOT NULL DEFAULT 0, -- 0158
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`, // BIGINT µLXC (prod 0083)
		`CREATE TABLE IF NOT EXISTS lxc_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
			metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`, // BIGINT µLXC (prod 0083)
		`CREATE TABLE IF NOT EXISTS agent_lxc_subbudgets (scoped_key_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL,
			ceiling_lxc BIGINT NOT NULL DEFAULT 50000000, spent_lxc BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`, // BIGINT µLXC + default 50 LXC = 50_000_000 µLXC (prod 0083:34-37)
		// B19.1: the spend path asks whether the key belongs to an agent account (migration 0140).
		`CREATE TABLE IF NOT EXISTS agent_accounts (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS agent_account_keys (scoped_key_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		// B19.13: the workspace's own spend reads what its agents hold.
		`CREATE TABLE IF NOT EXISTS agent_postings (workspace_id TEXT NOT NULL, account TEXT NOT NULL, amount_ulxc BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS agent_account_balances (workspace_id TEXT NOT NULL, account TEXT NOT NULL, balance_ulxc BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lxc_spend_claims (request_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			lxc_amount BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`, // BIGINT µLXC (prod 0083)
		`ALTER TABLE lxc_spend_claims ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0`, // 0197 (B32.11)
		`ALTER TABLE lxc_spend_claims ADD COLUMN IF NOT EXISTS question_id TEXT`,                            // 0375 (B58.3)
		`ALTER TABLE lxc_spend_claims ADD COLUMN IF NOT EXISTS replays INTEGER NOT NULL DEFAULT 0`,          // 0375 (B58.3)
		// 0158: the LXC balance writer reads the workspace's open holds.
		`CREATE TABLE IF NOT EXISTS lxc_reservations (reservation_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, held_ulxc BIGINT NOT NULL, settled_ulxc BIGINT,
			status TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','released')),
			requested_model TEXT, request_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ)`,
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0`, // 0197 (B32.11)
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS replays INTEGER NOT NULL DEFAULT 0`,          // 0385 (B50.47)
		`TRUNCATE lxc_balances, lxc_ledger, agent_lxc_subbudgets, lxc_spend_claims`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	p := &Proxy{}
	p.SetAgentSpender(store, func() bool { return true }) // flag ON
	return p, store, pool
}

func agentSpent(t *testing.T, pool *pgxpool.Pool, key string) int64 {
	t.Helper()
	var spent int64
	_ = pool.QueryRow(context.Background(), `SELECT COALESCE(spent_lxc,0) FROM agent_lxc_subbudgets WHERE scoped_key_id=$1`, key).Scan(&spent)
	return spent
}

// (proof 1) THE ADVERSARIAL OVER-BUDGET PROOF — the whole capstone. N concurrent agent requests whose
// aggregate estimate exceeds the ceiling ⇒ exactly ceiling debited, breaching ones blocked, and the "serve"
// (upstream) branch runs ONLY for the successful debits. No over-debit under -race.
func TestAgentAllocator_AdversarialOverBudget(t *testing.T) {
	p, store, pool := agentAllocHarness(t)
	ctx := context.Background()
	const prompt = "x that is reasonably long so the input-token estimate is a clean positive number " +
		"repeated several times to push the token count up and make lxcEstimate strictly greater than zero here"
	e := lxcEstimate(agentTestModel, prompt) // the per-request estimate the allocator will debit
	if e <= 0 {
		t.Fatalf("test setup: lxcEstimate must be > 0, got %v", e)
	}
	const allowedWant = 5
	ceiling := int64(allowedWant) * e

	if _, err := store.CreditLXC(ctx, "wsAgent", micro(1000), "fund", nil); err != nil { // ample balance
		t.Fatal(err)
	}
	if err := store.SetAgentCeiling(ctx, "keyAgent", "wsAgent", ceiling); err != nil {
		t.Fatal(err)
	}

	const N = 10 // aggregate 10×e = 2×ceiling — half must be blocked
	var served, blocked int64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// mirrors the handler: gate BEFORE serve — the "upstream" (served) runs iff not blocked.
			if p.agentAllocationBlocks(ctx, "keyAgent", "wsAgent", agentTestModel, prompt, "req-test") {
				atomic.AddInt64(&blocked, 1)
				return
			}
			atomic.AddInt64(&served, 1) // stand-in for client.Do — reached ONLY past the gate
		}()
	}
	wg.Wait()

	spent := agentSpent(t, pool, "keyAgent")
	// (a) exactly the ceiling debited — not a cent more.
	if spent != ceiling {
		t.Fatalf("spent_lxc must equal the ceiling EXACTLY: spent=%d ceiling=%d (e=%d)", spent, ceiling, e)
	}
	// (b)+(c) exactly allowedWant served (debited); the rest blocked; the upstream stand-in ran ONLY for the
	// successful debits (served == allowedWant, NOT N).
	if served != allowedWant || blocked != N-allowedWant {
		t.Fatalf("served=%d blocked=%d, want served=%d blocked=%d (upstream must run only for successful debits)",
			served, blocked, allowedWant, N-allowedWant)
	}
}

// (proof 2) SERVER-DERIVED KEY (anti-dodge): the debit key is NOT the client header — two requests (even a
// client replaying its X-Talyvor-Request-ID) get distinct server-derived keys ⇒ TWO debits. A client cannot
// replay to get free compute.
func TestAgentAllocator_ServerDerivedKey_NoDodge(t *testing.T) {
	p, store, pool := agentAllocHarness(t)
	ctx := context.Background()
	const prompt = "anti-dodge prompt long enough to make the estimate strictly positive for the debit here now"
	e := lxcEstimate(agentTestModel, prompt)
	if e <= 0 {
		t.Fatal("estimate must be > 0")
	}
	_, _ = store.CreditLXC(ctx, "wsD", micro(1000), "fund", nil)
	_ = store.SetAgentCeiling(ctx, "keyD", "wsD", 100*e) // ample ceiling

	// The debit-key derivation takes ONLY the apiKeyID (no client header param) — two derivations differ.
	k1, err1 := deriveAgentDebitKey(p.agentDebitSalt, "keyD")
	k2, err2 := deriveAgentDebitKey(p.agentDebitSalt, "keyD")
	if err1 != nil || err2 != nil {
		t.Fatalf("derive: %v %v", err1, err2)
	}
	if k1 == k2 || k1 == "" {
		t.Fatalf("server-derived keys must be distinct + non-empty: k1=%q k2=%q", k1, k2)
	}

	// Two allocations for the SAME agent (as if the same client request-id was replayed) ⇒ two debits.
	if p.agentAllocationBlocks(ctx, "keyD", "wsD", agentTestModel, prompt, "req-test") {
		t.Fatal("first allocation should succeed")
	}
	if p.agentAllocationBlocks(ctx, "keyD", "wsD", agentTestModel, prompt, "req-test") {
		t.Fatal("second allocation should ALSO succeed (fresh key — no idempotent dodge)")
	}
	if spent := agentSpent(t, pool, "keyD"); spent != 2*e {
		t.Fatalf("a client cannot dodge: two requests must debit TWICE, spent=%d want %d", spent, 2*e)
	}
}

// (proof 3) PRE-SERVE: an over-budget agent request blocks AND the serve branch is never taken.
func TestAgentAllocator_BlockIsPreServe(t *testing.T) {
	p, store, pool := agentAllocHarness(t)
	ctx := context.Background()
	const prompt = "pre-serve block prompt padded out so the input estimate is strictly positive for a debit ok"
	e := lxcEstimate(agentTestModel, prompt)
	_, _ = store.CreditLXC(ctx, "wsP", micro(1000), "fund", nil)
	_ = store.SetAgentCeiling(ctx, "keyP", "wsP", e/2) // ceiling below one request → always blocks

	served := 0
	if !p.agentAllocationBlocks(ctx, "keyP", "wsP", agentTestModel, prompt, "req-test") {
		served++ // would serve
	}
	if served != 0 {
		t.Fatal("an over-ceiling agent request must block BEFORE serve (served must be 0)")
	}
	if spent := agentSpent(t, pool, "keyP"); spent != 0 {
		t.Fatalf("blocked request must debit nothing, spent=%d", spent)
	}
}

// (proof 4) NON-AGENT UNCHANGED: apiKeyID == "" ⇒ the agent path is skipped entirely (no debit, allow).
func TestAgentAllocator_NonAgentSkipped(t *testing.T) {
	p, store, pool := agentAllocHarness(t)
	ctx := context.Background()
	const prompt = "non agent prompt padded so the estimate would be positive were the path even entered here"
	_, _ = store.CreditLXC(ctx, "wsN", micro(1000), "fund", nil)
	// empty apiKeyID (JWT/admin/anon) ⇒ never blocks, never debits.
	if p.agentAllocationBlocks(ctx, "", "wsN", agentTestModel, prompt, "req-test") {
		t.Fatal("non-agent (empty APIKeyID) must NOT be blocked by the agent path")
	}
	if spent := agentSpent(t, pool, ""); spent != 0 {
		t.Fatalf("non-agent must not touch any sub-budget, spent=%d", spent)
	}
}

// (proof 5) FLAG OFF: LXCAgentAllocationEnabled=false ⇒ agent path skipped even for an API-key request.
func TestAgentAllocator_FlagOffSkips(t *testing.T) {
	p, store, pool := agentAllocHarness(t)
	p.SetAgentSpender(store, func() bool { return false }) // flag OFF
	ctx := context.Background()
	const prompt = "flag off prompt padded so the estimate would be positive were the path entered at all now"
	_, _ = store.CreditLXC(ctx, "wsF", micro(1000), "fund", nil)
	_ = store.SetAgentCeiling(ctx, "keyF", "wsF", micro(0.0001)) // tiny ceiling — would block IF the path ran
	if p.agentAllocationBlocks(ctx, "keyF", "wsF", agentTestModel, prompt, "req-test") {
		t.Fatal("flag OFF ⇒ agent path must be skipped (no block) even for an API-key request")
	}
	if spent := agentSpent(t, pool, "keyF"); spent != 0 {
		t.Fatalf("flag OFF must debit nothing, spent=%d", spent)
	}
}

// (proof 6) THE ROW IS READABLE, END TO END: a served agent debit stamps the
// REQUESTED model and the token_events request_id onto its lxc_ledger row — so
// per-model spend is derivable and the money row joins to token_events. Proves
// the values thread from the allocator (where they live) through to the row.
func TestAgentAllocator_StampsModelAndRequestID(t *testing.T) {
	p, store, pool := agentAllocHarness(t)
	ctx := context.Background()
	const prompt = "readable ledger prompt padded so the input estimate is a clean strictly positive debit here now"
	e := lxcEstimate(agentTestModel, prompt)
	if e <= 0 {
		t.Fatal("estimate must be > 0")
	}
	_, _ = store.CreditLXC(ctx, "wsRow", micro(1000), "fund", nil)
	_ = store.SetAgentCeiling(ctx, "keyRow", "wsRow", 100*e)

	const reqID = "req-tok-e2e-1" // the token_events request_id (NOT the server-derived debit key)
	if p.agentAllocationBlocks(ctx, "keyRow", "wsRow", agentTestModel, prompt, reqID) {
		t.Fatal("funded agent request should be served (not blocked)")
	}

	var gotModel, gotReqID, gotType string
	var gotAmount int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(metadata->>'requested_model',''), COALESCE(metadata->>'request_id',''), type, amount
		FROM lxc_ledger WHERE workspace_id='wsRow' AND type='spend'`).
		Scan(&gotModel, &gotReqID, &gotType, &gotAmount); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if gotModel != agentTestModel {
		t.Errorf("requested_model = %q, want %q (the REQUESTED model — the debit precedes routing)", gotModel, agentTestModel)
	}
	if gotReqID != reqID {
		t.Errorf("request_id = %q, want %q (must be the token_events id, not the server debit key)", gotReqID, reqID)
	}
	if gotType != "spend" || gotAmount != -e {
		t.Errorf("money columns disturbed: type=%q amount=%d, want spend/%d", gotType, gotAmount, -e)
	}
}

// micro converts a whole-LXC test value to integer µLXC (SEC-2: 1 LXC = 1e6 µLXC).
func micro(lxc float64) int64 { return int64(lxc * 1e6) }
