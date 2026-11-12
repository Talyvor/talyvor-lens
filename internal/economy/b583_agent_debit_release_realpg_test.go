package economy

import (
	"context"
	"testing"
	"time"
)

// B58.3 — a question's pre-serve debit is given back, once, when nothing was served for it: by its release on
// the exit that served nothing, or by the sweeper when it was neither settled nor served. Real Postgres; asserted
// on lxc_ledger, the agent's postings and the balances.

func b583Agent(t *testing.T) (*DualTokenStore, string, string, string) {
	t.Helper()
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws, key = "ws-b583", "key-b583"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "asker", "user-b583")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	return s, ws, key, a.ID
}

// b583Books is the workspace's balance and the agent's.
func b583Books(t *testing.T, s *DualTokenStore, ws, agentID string) (bal, agent int64) {
	t.Helper()
	ctx := context.Background()
	bal, err := s.GetLXCBalance(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings
		WHERE workspace_id = $1 AND account = $2`, ws, "agent:"+agentID).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	return bal, agent
}

func TestB583_AReleaseGivesTheDebitAndItsFeeBackOnce(t *testing.T) {
	s, ws, key, agentID := b583Agent(t)
	ctx := context.Background()
	s.SetPlatformFee(func(context.Context, FeeQuerier, string) (int64, error) { return 300, nil }) // a fee to give back
	bal0, agent0 := b583Books(t, s, ws, agentID)
	meta := AgentDebitMeta{RequestedModel: "gpt-4o", RequestID: "q-1", Question: true}
	if err := s.SpendLXCForAgent(ctx, key, ws, "debit-1", 250_000, "pre-serve estimate", meta); err != nil {
		t.Fatal(err)
	}
	if bal, agent := b583Books(t, s, ws, agentID); bal >= bal0 || agent >= agent0 {
		t.Fatalf("the debit took nothing: balance %d → %d, agent %d → %d", bal0, bal, agent0, agent)
	}
	for range 2 {
		if err := s.ReleaseAgentDebit(ctx, ws, "debit-1", "upstream error"); err != nil {
			t.Fatal(err)
		}
	}
	if bal, agent := b583Books(t, s, ws, agentID); bal != bal0 || agent != agent0 {
		t.Errorf("after two releases: balance %d, agent %d — want %d and %d, given back once", bal, agent, bal0, agent0)
	}
	var refunds int
	var question string
	if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(metadata->>'request_id'), '') FROM lxc_ledger
		WHERE workspace_id = $1 AND description = 'agent debit release: upstream error'`, ws).Scan(&refunds, &question); err != nil {
		t.Fatal(err)
	}
	if refunds != 1 || question != "q-1" {
		t.Errorf("%d refund rows for question %q, want 1 for q-1", refunds, question)
	}
	var feeRows, feeNet int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0)::bigint FROM lxc_ledger
		WHERE workspace_id = $1 AND type = 'platform_fee'`, ws).Scan(&feeRows, &feeNet); err != nil {
		t.Fatal(err)
	}
	if feeRows != 2 || feeNet != 0 {
		t.Errorf("%d platform fee rows netting %d, want the fee and its reversal netting 0", feeRows, feeNet)
	}
}

func TestB583_TheSweeperGivesBackOnlyADebitNothingWasServedFor(t *testing.T) {
	s, ws, key, agentID := b583Agent(t)
	ctx := context.Background()
	bal0, _ := b583Books(t, s, ws, agentID)
	debit := func(key2, id, question string, isQuestion bool) {
		t.Helper()
		if err := s.SpendLXCForAgent(ctx, key2, ws, id, 100_000, "debit", AgentDebitMeta{RequestID: question, Question: isQuestion}); err != nil {
			t.Fatal(err)
		}
	}
	debit(key, "unserved", "q-unserved", true) // stranded: swept back
	debit(key, "served", "q-served", true)     // its answer has a usage row: kept
	debit(key, "settled", "q-settled", true)   // settled after its answer: kept
	debit(key, "charge", "alert-1", false)     // a charge for Talyvor's own work, not a question: kept
	debit(key, "fresh", "q-fresh", true)       // younger than the threshold: kept for now
	if _, err := s.SettleAgentDebit(ctx, ws, "settled", 100_000, AgentDebitMeta{RequestID: "q-settled"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE lxc_spend_claims SET created_at = now() - interval '1 hour' WHERE request_id <> 'fresh'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO token_events (workspace_id, provider, model, input_tokens, output_tokens, request_id)
		VALUES ($1, 'openai', 'gpt-4o', 10, 5, 'q-served')`, ws); err != nil {
		t.Fatal(err)
	}
	balBefore, agentBefore := b583Books(t, s, ws, agentID)
	for _, want := range []int{1, 0} { // a second sweep finds nothing
		n, err := s.ReleaseStrandedAgentDebits(ctx, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("swept %d debits, want %d", n, want)
		}
	}
	bal, agent := b583Books(t, s, ws, agentID)
	if bal != balBefore+100_000 || agent != agentBefore+100_000 {
		t.Errorf("after the sweep: balance %d, agent %d — want %d and %d, only the unserved debit back",
			bal, agent, balBefore+100_000, agentBefore+100_000)
	}
	if bal0-bal != 400_000 {
		t.Errorf("the workspace paid %d in all, want 400000 for the four debits that stand", bal0-bal)
	}
}

// Two requests under one Idempotency-Key share one debit: the second is a replay and may be answering on it, so
// the first one's release gives nothing back; if neither answer was served, the sweeper does.
func TestB583_AReplayedDebitIsGivenBackOnlyByTheSweeper(t *testing.T) {
	s, ws, key, agentID := b583Agent(t)
	ctx := context.Background()
	bal0, agent0 := b583Books(t, s, ws, agentID)
	meta := AgentDebitMeta{RequestID: "q-shared", Question: true}
	for range 2 { // the first try, and its retry replaying the same claim
		if err := s.SpendLXCForAgent(ctx, key, ws, "shared", 100_000, "pre-serve estimate", meta); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReleaseAgentDebit(ctx, ws, "shared", "upstream error"); err != nil {
		t.Fatal(err)
	}
	if bal, agent := b583Books(t, s, ws, agentID); bal != bal0-100_000 || agent != agent0-100_000 {
		t.Errorf("after a release under a replay: balance %d, agent %d — want the debit to stand (%d, %d)",
			bal, agent, bal0-100_000, agent0-100_000)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE lxc_spend_claims SET created_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReleaseStrandedAgentDebits(ctx, 15*time.Minute); err != nil || n != 1 {
		t.Fatalf("swept %d, %v — want 1", n, err)
	}
	if bal, agent := b583Books(t, s, ws, agentID); bal != bal0 || agent != agent0 {
		t.Errorf("after the sweep: balance %d, agent %d — want %d and %d", bal, agent, bal0, agent0)
	}
}
