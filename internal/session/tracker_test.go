package session

import (
	"context"
	"math"
	"testing"
)

func TestGetOrCreate_CreatesNewSession(t *testing.T) {
	tr := New(nil)
	s := tr.GetOrCreate(context.Background(), "s1", "ws-1", "agent-1")
	if s == nil {
		t.Fatal("GetOrCreate returned nil")
	}
	if s.ID != "s1" || s.WorkspaceID != "ws-1" || s.AgentName != "agent-1" {
		t.Errorf("session identity wrong: %+v", s)
	}
	if s.TurnCount != 0 {
		t.Errorf("new session TurnCount = %d, want 0", s.TurnCount)
	}
}

func TestGetOrCreate_ReturnsExistingOnSecondCall(t *testing.T) {
	tr := New(nil)
	first := tr.GetOrCreate(context.Background(), "s1", "ws-1", "agent")
	second := tr.GetOrCreate(context.Background(), "s1", "ws-1", "other-agent")
	if first != second {
		t.Errorf("second call returned a different *Session; want same pointer")
	}
	// Mutations on second-call args must NOT overwrite the cached state.
	if second.WorkspaceID != "ws-1" || second.AgentName != "agent" {
		t.Errorf("existing session was overwritten by second GetOrCreate: %+v", second)
	}
	// B42.2: another workspace naming the same id gets nothing, and ws-1's session is untouched.
	if other := tr.GetOrCreate(context.Background(), "s1", "ws-other", "other-agent"); other != nil {
		t.Errorf("ws-other was handed ws-1's session: %+v", other)
	}
	if first.WorkspaceID != "ws-1" || first.AgentName != "agent" {
		t.Errorf("ws-1's session changed: %+v", first)
	}
}

func TestRecordTurn_IncrementsTurnCount(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	tr.GetOrCreate(ctx, "s1", "ws-1", "agent")

	for i := 1; i <= 3; i++ {
		if err := tr.RecordTurn(ctx, "s1", Turn{TurnNumber: i, Role: "user"}); err != nil {
			t.Fatalf("RecordTurn: %v", err)
		}
	}
	s, _ := tr.GetSession("s1")
	if s.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", s.TurnCount)
	}
	if len(s.Turns) != 3 {
		t.Errorf("len(Turns) = %d, want 3", len(s.Turns))
	}
}

func TestRecordTurn_UpdatesTotalCost(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	tr.GetOrCreate(ctx, "s1", "ws-1", "agent")

	_ = tr.RecordTurn(ctx, "s1", Turn{CostUSD: 0.25, InputTokens: 100, OutputTokens: 50})
	_ = tr.RecordTurn(ctx, "s1", Turn{CostUSD: 0.50, InputTokens: 200, OutputTokens: 100})
	_ = tr.RecordTurn(ctx, "s1", Turn{CostUSD: 1.25, InputTokens: 400, OutputTokens: 200})

	s, _ := tr.GetSession("s1")
	if math.Abs(s.TotalCostUSD-2.00) > 1e-9 {
		t.Errorf("TotalCostUSD = %v, want 2.00", s.TotalCostUSD)
	}
	if s.TotalInputTokens != 700 {
		t.Errorf("TotalInputTokens = %d, want 700", s.TotalInputTokens)
	}
	if s.TotalOutputTokens != 350 {
		t.Errorf("TotalOutputTokens = %d, want 350", s.TotalOutputTokens)
	}
}

func TestRecordTurn_TracksCacheHitsSeparately(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	tr.GetOrCreate(ctx, "s1", "ws-1", "agent")

	_ = tr.RecordTurn(ctx, "s1", Turn{Cached: true})
	_ = tr.RecordTurn(ctx, "s1", Turn{Cached: true})
	_ = tr.RecordTurn(ctx, "s1", Turn{Cached: false})

	s, _ := tr.GetSession("s1")
	if s.CacheHits != 2 {
		t.Errorf("CacheHits = %d, want 2", s.CacheHits)
	}
	if s.CacheMisses != 1 {
		t.Errorf("CacheMisses = %d, want 1", s.CacheMisses)
	}
}

func TestGetSessionCost_ReturnsCorrectTotal(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	tr.GetOrCreate(ctx, "s1", "ws-1", "agent")

	_ = tr.RecordTurn(ctx, "s1", Turn{CostUSD: 0.10})
	_ = tr.RecordTurn(ctx, "s1", Turn{CostUSD: 0.20})
	_ = tr.RecordTurn(ctx, "s1", Turn{CostUSD: 0.30})

	if got := tr.GetSessionCost("s1"); math.Abs(got-0.60) > 1e-9 {
		t.Errorf("GetSessionCost = %v, want 0.60", got)
	}
}

func TestSummariseSession_CalculatesHitRate(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	tr.GetOrCreate(ctx, "s1", "ws-1", "agent")

	// 3 cache hits, 1 miss → 75% hit rate.
	for i := 0; i < 3; i++ {
		_ = tr.RecordTurn(ctx, "s1", Turn{Cached: true})
	}
	_ = tr.RecordTurn(ctx, "s1", Turn{Cached: false})

	sum := tr.SummariseSession(ctx, "s1")
	if math.Abs(sum.CacheHitRate-0.75) > 1e-9 {
		t.Errorf("CacheHitRate = %v, want 0.75", sum.CacheHitRate)
	}
	if sum.TurnCount != 4 {
		t.Errorf("TurnCount = %d, want 4", sum.TurnCount)
	}
}

func TestSummariseSession_FindsMostUsedModel(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	tr.GetOrCreate(ctx, "s1", "ws-1", "agent")

	// gpt-4o appears 3 times; claude-haiku once.
	for i := 0; i < 3; i++ {
		_ = tr.RecordTurn(ctx, "s1", Turn{Model: "gpt-4o"})
	}
	_ = tr.RecordTurn(ctx, "s1", Turn{Model: "claude-haiku-4-5"})

	sum := tr.SummariseSession(ctx, "s1")
	if sum.MostUsedModel != "gpt-4o" {
		t.Errorf("MostUsedModel = %q, want gpt-4o", sum.MostUsedModel)
	}
}
