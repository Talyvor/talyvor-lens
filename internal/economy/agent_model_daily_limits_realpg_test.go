package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// B28.301 — an agent's rules cap what it spends on one model in a day, judged inside its hold like its other
// limits: an Opus hold over Opus's cap writes no posting while a Haiku hold writes one; a released hold gives
// its model's day back; and the next day the cap starts again.
//
// The rules read "now" from the request's time, so each hold names the time it is judged at — taken from the
// first hold's own posting, so where the day begins does not depend on when the test runs.
func TestB28301_AnOpusHoldOverItsCapWritesNothing_AHaikuHoldWritesOnePosting(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws, key, opus, haiku = "ws-model-caps", "key-model-caps", "claude-opus-4-1", "claude-haiku-4-5"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "capped by model", "user-model-caps")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 50_000_000); err != nil {
		t.Fatal(err)
	}
	// Opus may spend 5 LXC a day; Haiku's zero is no cap, and is not kept.
	rules, err := s.SetAgentRules(ctx, ws, a.ID, AgentRules{ModelDailyLimitsULXC: map[string]int64{opus: 5_000_000, haiku: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.ModelDailyLimitsULXC) != 1 || rules.ModelDailyLimitsULXC[opus] != 5_000_000 {
		t.Fatalf("saved model caps = %v, want only %s at 5 LXC", rules.ModelDailyLimitsULXC, opus)
	}

	hold := func(model, ref string, at time.Time) error {
		return s.ReserveLXCForAgent(WithAgentRequest(ctx, AgentRequest{Model: model, Provider: "anthropic", At: at}),
			key, ws, ref, 3_000_000, AgentDebitMeta{RequestedModel: model})
	}
	postings := func(model string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE account = $1 AND model IS NOT DISTINCT FROM NULLIF($2, '')`,
			agentAccount(a.ID), model).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	all := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	refused := func(err error) bool {
		return errors.Is(err, ErrAgentRule) && strings.Contains(err.Error(), "daily limit of $0.50 (5 LXC) for the model")
	}

	// 3 LXC of Opus is under its cap: one posting on the agent's account, naming Opus.
	if err := hold(opus, "res-opus-1", time.Time{}); err != nil {
		t.Fatalf("an Opus hold under its cap: %v", err)
	}
	if n := postings(opus); n != 1 {
		t.Fatalf("an Opus hold under its cap wrote %d postings naming Opus on the agent's account, want 1", n)
	}
	var t1 time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM agent_postings WHERE ref = 'res-opus-1' AND account = $1`,
		agentAccount(a.ID)).Scan(&t1); err != nil {
		t.Fatal(err)
	}

	// Another 3 LXC of Opus is over it, however the agent spells it: refused, and nothing is written anywhere
	// in the workspace's book.
	before := all()
	for i, spelt := range []string{opus, "Claude-Opus-4-1", "claude-opus-4-1-20250805", "claude-opus-4-1-latest"} {
		if err := hold(spelt, fmt.Sprintf("res-opus-2-%d", i), t1); !refused(err) {
			t.Fatalf("a %s hold over Opus's cap = %v, want the Opus daily limit's refusal", spelt, err)
		}
	}
	if n := all(); n != before {
		t.Fatalf("an Opus hold over its cap wrote %d postings, want none", n-before)
	}
	var reservations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_reservations WHERE reservation_id LIKE 'res-opus-2-%'`).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("an Opus hold over its cap left %d reservations (%v), want none", reservations, err)
	}

	// The same 3 LXC of Haiku is not Opus: one posting on the agent's account, naming Haiku.
	if err := hold(haiku, "res-haiku-1", t1); err != nil {
		t.Fatalf("a Haiku hold while Opus is at its cap: %v", err)
	}
	if n := postings(haiku); n != 1 {
		t.Fatalf("a Haiku hold wrote %d postings naming Haiku on the agent's account, want 1", n)
	}

	// Releasing the first Opus hold gives its 3 LXC back to Opus's day: the release names Opus too.
	if err := s.ReleaseLXCReservation(ctx, "res-opus-1", "test"); err != nil {
		t.Fatal(err)
	}
	if err := hold(opus, "res-opus-3", t1); err != nil {
		t.Fatalf("an Opus hold after the first was released: %v", err)
	}

	// Opus is at 3 of 5 again: another 3 is over it today, and under it tomorrow.
	if err := hold(opus, "res-opus-4", t1); !refused(err) {
		t.Fatalf("a second Opus hold the same day = %v, want the Opus daily limit's refusal", err)
	}
	u := t1.UTC() // the agent's timezone is UTC
	tomorrow := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	if err := hold(opus, "res-opus-5", tomorrow); err != nil {
		t.Fatalf("an Opus hold the next day: %v", err)
	}
}
