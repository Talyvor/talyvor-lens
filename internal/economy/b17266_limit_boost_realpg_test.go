package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// B17.266 — a daily limit raised on Limit boost lets a request through until its time; from its time on the same
// request is refused by the daily limit the rules set, and writes nothing: no posting on the agent's account and no
// reservation. The rules read "now" from the request's time, so the request after the boost names a time past it.
func TestB17266_ABoostedDailyLimitServesUntilItsTime_ThenTheSameRequestIsRefusedAndWritesNothing(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws, key = "ws-limit-boost", "key-limit-boost"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "boosted", "user-limit-boost")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 1_000_000); err != nil {
		t.Fatal(err)
	}
	// The rules allow 1 µLXC a day; the boost raises it to 1 LXC for an hour.
	if _, err := s.SetAgentRules(ctx, ws, a.ID, AgentRules{DailyLimitULXC: 1}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	b, err := s.BoostAgentRule(ctx, ws, a.ID, AgentRuleBoost{Rule: "daily_limit_ulxc", Value: 1_000_000, Until: until})
	if err != nil {
		t.Fatalf("the boost was refused: %v", err)
	}
	if b.RaisedFrom != 1 || b.Value != 1_000_000 || !b.Until.Equal(until) {
		t.Fatalf("the boost = %+v, want the daily limit raised from 1 to 1,000,000 µLXC until %s", b, until)
	}

	hold := func(ref string, at time.Time) error {
		return s.ReserveLXCForAgent(WithAgentRequest(ctx, AgentRequest{Model: "claude-haiku-4-5", Provider: "anthropic", At: at}),
			key, ws, ref, 2_472, AgentDebitMeta{RequestedModel: "claude-haiku-4-5"})
	}
	postings := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE account = $1`, agentAccount(a.ID)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	reservations := func(ref string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_reservations WHERE reservation_id = $1`, ref).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Under the boost: the request is held, one posting on the agent's account.
	before := postings()
	if err := hold("res-boosted", until.Add(-time.Minute)); err != nil {
		t.Fatalf("a request under the boosted daily limit was refused: %v", err)
	}
	if n := postings() - before; n != 1 {
		t.Fatalf("a request under the boost wrote %d postings on the agent's account, want 1", n)
	}
	if n := reservations("res-boosted"); n != 1 {
		t.Fatalf("a request under the boost left %d reservations, want 1", n)
	}

	// From its time on the boost is not read: the same request is refused by the rules' 1 µLXC a day, in the words
	// the owner reads, and writes nothing.
	before = postings()
	err = hold("res-after", until.Add(time.Second))
	if !errors.Is(err, ErrAgentRule) || !strings.Contains(err.Error(), "of its daily limit of $0.0000001") || strings.Contains(err.Error(), "LXC") {
		t.Fatalf("the same request after the boost = %v, want the refusal by the $0.0000001 daily limit, in dollars alone (B61.16)", err)
	}
	if n := postings() - before; n != 0 {
		t.Fatalf("the refused request wrote %d postings on the agent's account, want none", n)
	}
	if n := reservations("res-after"); n != 0 {
		t.Fatalf("the refused request left %d reservations, want none", n)
	}
	// And the rules still read 1 µLXC a day: the boost never touched them.
	r, err := s.GetAgentRules(ctx, ws, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.DailyLimitULXC != 1 {
		t.Fatalf("the rules' daily limit = %d µLXC after the boost, want the 1 µLXC it was raised from", r.DailyLimitULXC)
	}
}
