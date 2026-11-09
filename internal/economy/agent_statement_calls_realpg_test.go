package economy

import (
	"context"
	"fmt"
	"testing"
)

// B28.93 — Chat's Recent calls lists an agent's newest calls with model, cost and source from its statement. An agent
// asks 21 questions — 20 held and settled, one debited and settled — each answered from where its token_events row
// says; every line of every call on its statement names the model it asked and where its answer came from, and a
// call Lens has no token_events row for names no source.
func TestB2893_AnAgentsStatementNamesEachCallsModelAndSource(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws, key = "ws-b2893", "key-b2893"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "caller", "user-b2893")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 50_000_000); err != nil {
		t.Fatal(err)
	}
	served := []string{"upstream", "cache_hit_exact", "cache_hit_pooled", "node", "cache_hit_semantic"}
	type call struct{ model, source string }
	want := map[string]call{} // by the ref its lines share
	for i := 1; i <= 21; i++ {
		model := []string{"gpt-4o-mini", "claude-haiku-4-5"}[i%2]
		requestID := fmt.Sprintf("req-b2893-%d", i)
		meta := AgentDebitMeta{RequestedModel: model, RequestID: requestID}
		ref := fmt.Sprintf("res-b2893-%d", i)
		if i == 7 {
			ref = "debit-b2893-7"
			if err := s.SpendLXCForAgent(ctx, key, ws, ref, 100_000, "q7", meta); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SettleAgentDebit(ctx, ws, ref, 150_000, meta); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := s.ReserveLXCForAgent(ctx, key, ws, ref, 400_000, meta); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.SettleLXCReservation(ctx, ref, 100_000+int64(i), meta); err != nil {
				t.Fatal(err)
			}
		}
		if i == 21 { // its token_events row is not written yet
			want[ref] = call{model, ""}
			continue
		}
		src := served[i%len(served)]
		if _, err := pool.Exec(ctx, `INSERT INTO token_events (workspace_id, provider, model, input_tokens, output_tokens, request_id, serve_source)
			VALUES ($1, 'openai', $2, 10, 5, $3, $4)`, ws, model, requestID, src); err != nil {
			t.Fatal(err)
		}
		want[ref] = call{model, CallSource(src)}
	}

	lines, err := s.AgentStatement(ctx, ws, a.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, l := range lines {
		w, isCall := want[l.Ref]
		if !callKinds[l.Kind] || !isCall {
			if l.Model != "" || l.Source != "" {
				t.Errorf("%s line %q names model %q and source %q, want neither", l.Kind, l.Ref, l.Model, l.Source)
			}
			continue
		}
		seen[l.Ref] = true
		if l.Model != w.model || l.Source != w.source {
			t.Errorf("%s line of %s names model %q and source %q, want %q and %q", l.Kind, l.Ref, l.Model, l.Source, w.model, w.source)
		}
	}
	if len(seen) != 21 {
		t.Fatalf("the statement has lines for %d of the 21 calls", len(seen))
	}
}
