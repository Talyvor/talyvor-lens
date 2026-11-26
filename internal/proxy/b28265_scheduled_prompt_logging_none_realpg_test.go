package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B28.265 — a workspace whose logging policy is none keeps no answer text: a prompt it scheduled is still asked and
// paid for on the agent's wallet, but its run records the outcome and the charge, never what the model said.
func TestB28265_ALoggingNoneWorkspacesScheduledRunKeepsNoAnswer(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingNone)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.openAIURL = usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`).URL

	const ws = "ws-log"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, logging_policy) VALUES ($1, 'log-test', $1, 'none')`, ws); err != nil {
		t.Fatal(err)
	}
	seamFund(t, pool, ws, 100_000_000)
	var key string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace_api_keys (workspace_id, key_hash, key_prefix, scopes) VALUES ($1, 'h', 'p', '{proxy}')
		RETURNING id::text`, ws).Scan(&key); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "Scheduler", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, agent.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour).Truncate(time.Minute)
	if _, err := store.CreatePromptSchedule(ctx, ws, agent.ID, "What is 8995 + 8706?", "openai", "gpt-4o", "once", at); err != nil {
		t.Fatal(err)
	}
	if a, r, err := store.RunPromptSchedules(ctx, at, p); err != nil || a != 1 || r != 0 {
		t.Fatalf("at its time: %d answered, %d refused, %v; want 1 answered", a, r, err)
	}

	var outcome, answer string
	var charged int64
	if err := pool.QueryRow(ctx, `SELECT r.outcome, r.answer, r.charged_ulxc FROM prompt_schedule_runs r
		JOIN prompt_schedules s ON s.id = r.schedule_id WHERE s.workspace_id = $1`, ws).Scan(&outcome, &answer, &charged); err != nil {
		t.Fatal(err)
	}
	if outcome != "answered" || answer != "" {
		t.Fatalf("the run row is %s with answer %q, want answered with no answer kept", outcome, answer)
	}
	// The ledger still carries the charge: the agent's statement after its funding sums to what the run says it took.
	lines, err := store.AgentStatement(ctx, ws, agent.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var net int64
	for _, l := range lines {
		if l.Kind != "fund" {
			net += l.AmountULXC
		}
	}
	if charged <= 0 || charged != -net {
		t.Fatalf("the run took %d µLXC; the agent's statement after its funding nets %d", charged, net)
	}
}
