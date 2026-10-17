package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B28.125 — a prompt scheduled on an agent's wallet waits for its time, is then asked once through the real proxy as
// the agent's key, and what it cost is the agent's new statement lines, the run naming the first; the workspace's own
// balance does not move. Scheduled on a paused agent, it is refused with Lens's sentence and charges nothing.
func TestB28125_AScheduledPromptIsAskedAtItsTimeOnTheAgentsWallet(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.openAIURL = usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`).URL

	const ws, fund = "ws-log", int64(10_000_000)
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
	if _, err := store.FundAgent(ctx, ws, agent.ID, fund); err != nil {
		t.Fatal(err)
	}
	book0, err := store.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}

	at := time.Now().Add(time.Hour).Truncate(time.Minute)
	prompt := "What is 8995 + 8706? " + strings.Repeat("x", 7000)
	if _, err := store.CreatePromptSchedule(ctx, ws, agent.ID, prompt, "openai", "gpt-4o", "once", at); err != nil {
		t.Fatal(err)
	}
	run := func(now time.Time) (int, int) {
		t.Helper()
		answered, refused, err := store.RunPromptSchedules(ctx, now, p)
		if err != nil {
			t.Fatal(err)
		}
		return answered, refused
	}
	if a, r := run(at.Add(-time.Minute)); a+r != 0 {
		t.Fatalf("a minute before its time the schedule was asked: %d answered, %d refused", a, r)
	}
	if a, r := run(at); a != 1 || r != 0 {
		t.Fatalf("at its time: %d answered, %d refused, want 1 answered", a, r)
	}
	if a, r := run(at.Add(time.Hour)); a+r != 0 {
		t.Fatalf("a schedule that runs once was asked again: %d answered, %d refused", a, r)
	}

	list, err := store.ListPromptSchedules(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Active || list[0].NextRunAt != nil || len(list[0].Runs) != 1 {
		t.Fatalf("after its one run the schedule is %+v, want inactive with one run and no next time", list)
	}
	r := list[0].Runs[0]
	if r.Outcome != "answered" || r.Answer != "ok" {
		t.Fatalf("the run is %+v, want answered with the model's answer", r)
	}
	// The ledger: the agent's new lines are what the run says it took, and the first of them is the line it names.
	lines, err := store.AgentStatement(ctx, ws, agent.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var net int64
	named := false
	for _, l := range lines {
		if l.Kind == "fund" {
			continue
		}
		net += l.AmountULXC
		named = named || (l.EntryID == r.EntryID && (l.Kind == "hold" || l.Kind == "spend"))
	}
	if r.ChargedULXC <= 0 || r.ChargedULXC != -net || !named {
		t.Fatalf("the run took %d µLXC naming line %s; the agent's statement after its funding: %+v", r.ChargedULXC, r.EntryID, lines)
	}
	if lines[0].BalanceAfterULXC != fund-r.ChargedULXC {
		t.Fatalf("the agent's statement ends at %d, want %d − %d", lines[0].BalanceAfterULXC, fund, r.ChargedULXC)
	}
	book1, err := store.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if book1.UnallocatedULXC != book0.UnallocatedULXC {
		t.Fatalf("the agent paid, yet the workspace's own balance moved: %d → %d", book0.UnallocatedULXC, book1.UnallocatedULXC)
	}

	// Paused, the agent's rules refuse the next one: a refused run with Lens's sentence, and no new line.
	if err := store.PauseAgent(ctx, ws, agent.ID, "nightly check"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePromptSchedule(ctx, ws, agent.ID, "What is 1 + 1? "+strings.Repeat("y", 7000), "openai", "gpt-4o", "day", at); err != nil {
		t.Fatal(err)
	}
	if a, r := run(at); a != 0 || r != 1 {
		t.Fatalf("on a paused agent: %d answered, %d refused, want 1 refused", a, r)
	}
	list, _ = store.ListPromptSchedules(ctx, ws)
	if got := list[0].Runs; len(got) != 1 || got[0].Outcome != "refused" || !strings.Contains(got[0].Detail, "paused") || got[0].ChargedULXC != 0 {
		t.Fatalf("the paused agent's run is %+v, want refused saying it is paused, charging nothing", got)
	}
	if list[0].NextRunAt == nil || !list[0].NextRunAt.Equal(at.AddDate(0, 0, 1)) || !list[0].Active {
		t.Fatalf("a daily schedule's next time is %v, want %v", list[0].NextRunAt, at.AddDate(0, 0, 1))
	}
	after, _ := store.AgentStatement(ctx, ws, agent.ID, 50)
	if len(after) != len(lines) {
		t.Fatalf("the refused run wrote statement lines: %d → %d", len(lines), len(after))
	}
}
