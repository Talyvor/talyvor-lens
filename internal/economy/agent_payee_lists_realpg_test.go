package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// B28.303 — a payment an agent's schedule makes is judged by its payee lists like one it makes now: a tick to a
// blocked payee is recorded refused and posts nothing.
func TestB28303_AScheduledPaymentToABlockedPayeePostsNothing(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws = "ws-payee-schedule"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	payer, err := s.CreateAgent(ctx, ws, "payer", "user-payer")
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := s.CreateAgent(ctx, ws, "blocked", "user-payer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, payer.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAgentRules(ctx, ws, payer.ID, AgentRules{BlockedPayees: []string{blocked.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PayAgent(ctx, ws, payer.ID, blocked.ID, 1_000_000, "now"); !errors.Is(err, ErrAgentRule) {
		t.Fatalf("paying a blocked payee now = %v, want the rules' refusal", err)
	}
	now := time.Now()
	sc, err := s.CreateAgentSchedule(ctx, ws, payer.ID, blocked.ID, 1_000_000, "rent", "week", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunAgentSchedules(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListAgentScheduleRuns(ctx, ws, sc.ID)
	if err != nil || len(runs) != 1 || runs[0].Outcome != "refused" || !strings.Contains(runs[0].Detail, "may not pay blocked") {
		t.Fatalf("the schedule's runs = %+v (%v), want one refused naming the blocked payee", runs, err)
	}
	var pays int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind = 'pay'`, ws).Scan(&pays); err != nil || pays != 0 {
		t.Fatalf("%d pay postings (%v), want none", pays, err)
	}
}
