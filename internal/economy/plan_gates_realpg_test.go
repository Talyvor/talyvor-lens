package economy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/byok"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/plans"
)

// planGatesOnPlan puts ws on plan through a live subscription, as the webhook writes it; addOn marks Team's BYOK
// add-on.
func planGatesOnPlan(t *testing.T, pool *pgxpool.Pool, ws, plan string, addOn bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO subscriptions (workspace_id, stripe_subscription_id,
		stripe_customer_id, price_id, status, livemode, last_event_at, plan, byok)
		VALUES ($1, 'sub_' || $1, 'cus_' || $1, 'price_' || $2, 'active', false, NOW(), $2, $3)`, ws, plan, addOn); err != nil {
		t.Fatal(err)
	}
}

// planRefusal reports whether err is a plan's refusal of plan, with the plan that would allow it, in words naming both
// plans and never the setting, which is a field of its own (B55.29).
func planRefusal(err error, plan, allows string) bool {
	var r *plans.Refusal
	return errors.As(err, &r) && errors.Is(err, plans.ErrRefused) && r.Plan == plan && r.Allows == allows &&
		r.Setting == plans.Setting && !strings.Contains(err.Error(), "LENS_") &&
		strings.Contains(err.Error(), plans.Name(plan)) && strings.Contains(err.Error(), plans.Name(allows))
}

// B32.12 — with Nicolai's values: a Free workspace's fourth agent and second member are refused naming the plan;
// a Team workspace's 26th agent is refused and its first 25 are not; saving a provider key on Team is refused
// without the BYOK add-on and allowed with it; a Business workspace creates 100 agents; on Free a capability B30
// registers takes test money only even with a live clearance.
func TestB3212_WhatEachPlanUnlocks(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	agents := func(ws string, n int) error {
		t.Helper()
		for i := 1; i <= n; i++ {
			if _, err := s.CreateAgent(ctx, ws, fmt.Sprintf("agent %d", i), "user_owner"); err != nil {
				return fmt.Errorf("agent %d: %w", i, err)
			}
		}
		return nil
	}
	count := func(ws string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_accounts WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Free: three agents, then the fourth is refused naming the plan and Team; one member, then the second.
	if err := agents("ws-b3212-free", 3); err != nil {
		t.Fatalf("free's first three agents: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "ws-b3212-free", "agent 4", "user_owner"); !planRefusal(err, "free", "team") ||
		err.Error() != "Free allows 3 agents, and all 3 are in use. Team allows 25." {
		t.Fatalf("free's fourth agent = %v, want refused in words naming the free plan and the team plan", err)
	}
	if n := count("ws-b3212-free"); n != 3 {
		t.Fatalf("free has %d agents after the refusal, want 3", n)
	}
	free, err := plans.Of(ctx, pool, "ws-b3212-free")
	if err != nil {
		t.Fatal(err)
	}
	if err := free.CheckSeats(1); err != nil {
		t.Fatalf("free's first member = %v, want allowed", err)
	}
	if err := free.CheckSeats(2); !planRefusal(err, "free", "team") || err.Error() != "Free allows 1 seat, and it is taken. Team allows 5." {
		t.Fatalf("free's second member = %v, want refused in words naming the free plan and the team plan", err)
	}

	// Team: 25 agents, then the 26th is refused naming Business.
	planGatesOnPlan(t, pool, "ws-b3212-team", "team", false)
	if err := agents("ws-b3212-team", 25); err != nil {
		t.Fatalf("team's first 25 agents: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "ws-b3212-team", "agent 26", "user_owner"); !planRefusal(err, "team", "business") ||
		err.Error() != "Team allows 25 agents, and all 25 are in use. Business allows unlimited agents." {
		t.Fatalf("team's 26th agent = %v, want refused in words naming the team plan and the business plan", err)
	}
	if n := count("ws-b3212-team"); n != 25 {
		t.Fatalf("team has %d agents after the refusal, want 25", n)
	}

	// Business: 100 agents.
	planGatesOnPlan(t, pool, "ws-b3212-business", "business", true)
	if err := agents("ws-b3212-business", 100); err != nil {
		t.Fatalf("business's 100 agents: %v", err)
	}
	if n := count("ws-b3212-business"); n != 100 {
		t.Fatalf("business has %d agents, want 100", n)
	}

	// Own provider keys on Team: refused without the BYOK add-on, allowed with it.
	ring, err := envelope.NewKeyring(bytes.Repeat([]byte{7}, envelope.KEKLen))
	if err != nil {
		t.Fatal(err)
	}
	keys := byok.New(pool, ring)
	if _, err := keys.Put(ctx, "ws-b3212-team", "openai", "sk-team-key-0001"); !planRefusal(err, "team", "business") ||
		err.Error() != "Team has your own provider keys only with the BYOK add-on. Business includes them." {
		t.Fatalf("a provider key on team without the BYOK add-on = %v, want refused naming the add-on and the business plan", err)
	}
	planGatesOnPlan(t, pool, "ws-b3212-team-byok", "team", true)
	if k, err := keys.Put(ctx, "ws-b3212-team-byok", "openai", "sk-team-key-0002"); err != nil || k.Last4 != "0002" {
		t.Fatalf("a provider key on team with the BYOK add-on = %+v, %v, want stored", k, err)
	}
	if own, err := keys.OwnKeys(ctx, "ws-b3212-team-byok"); err != nil || own["openai"] != "sk-team-key-0002" {
		t.Fatalf("team with the add-on is served on %v, %v, want its own key", own, err)
	}

	// On Free a capability B30 registers takes test money only even with a live clearance; on Team the same
	// clearance takes live money.
	gb := WithUseCountry(ctx, "GB")
	if _, err := s.ClearCapability(gb, CapabilityFX, "nicolai", ClearanceTerms{Reference: "partner agreement PA-12",
		Licence: "EMI-900123", Partner: "Test FX Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	spend := func(ws, funding string) (int64, error) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1) ON CONFLICT DO NOTHING`, ws); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreditLXC(gb, ws, 100_000_000, "stripe top-up", map[string]interface{}{"funding": funding}); err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(gb)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(gb) }()
		testFunded, err := spendForCapability(gb, tx, ws, CapabilityFX, 10_000_000)
		if err != nil {
			return 0, err
		}
		return testFunded, tx.Commit(gb)
	}
	var refusal *CapabilityRefusal
	if _, err := spend("ws-b3212-free", FundingLive); !planRefusal(err, "free", "team") || !errors.As(err, &refusal) ||
		!errors.Is(err, ErrCapabilityNotCleared) || !strings.HasPrefix(err.Error(), "On Free, ") ||
		!strings.HasSuffix(err.Error(), " takes no real money. On Team it does, once it is cleared.") {
		t.Fatalf("live money for a cleared B30 capability on free = %v, want refused in words naming the free plan and the team plan", err)
	}
	if took, err := spend("ws-b3212-free-test", FundingTest); err != nil || took != 10_000_000 {
		t.Fatalf("test money for a cleared B30 capability on free took %d test-funded µLXC, %v; want all 10 LXC of it", took, err)
	}
	liveVerified(t, pool, "ws-b3212-team") // B30.4: FX needs L2 for live money
	if took, err := spend("ws-b3212-team", FundingLive); err != nil || took != 0 {
		t.Fatalf("live money for a cleared B30 capability on team took %d test-funded µLXC, %v; want live money, none test-funded", took, err)
	}
}
