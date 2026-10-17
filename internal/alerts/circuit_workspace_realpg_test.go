package alerts

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// B42.10: the team and feature labels are the caller's own, so workspace B sending workspace A's labels
// must neither trip nor hold open A's circuit, while A's own labelled traffic still trips it and the
// monitor still closes it — and the monitor closes A's without touching B's.
func TestCircuit_LabelsFromAnotherWorkspaceNeverReachItsCircuit(t *testing.T) {
	pool := cacheServePool(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	wsA, wsB := "b4210-a-"+suffix, "b4210-b-"+suffix
	team, feature := "core-"+suffix, "search"
	mgr := New(pool, nil, []SpendRule{{ID: "r1", Team: team, Feature: feature, WindowHours: 1,
		WarningUSD: 100, CriticalUSD: 100, CircuitUSD: 0.5}})

	// gpt-4o output is $10 per million, so 100k output tokens is $1: over the $0.50 circuit.
	if err := mgr.RecordSpend(ctx, wsB, team, "", feature, "gpt-4o", 0, 100_000, "", "", "", "text", false); err != nil {
		t.Fatalf("RecordSpend B: %v", err)
	}
	if mgr.IsCircuitOpen(wsA, team, feature) {
		t.Fatal("workspace B's spend under A's labels opened A's circuit")
	}
	if !mgr.IsCircuitOpen(wsB, team, feature) {
		t.Fatal("workspace B's own circuit did not open")
	}

	if err := mgr.RecordSpend(ctx, wsA, team, "", feature, "gpt-4o", 0, 100_000, "", "", "", "text", false); err != nil {
		t.Fatalf("RecordSpend A: %v", err)
	}
	if !mgr.IsCircuitOpen(wsA, team, feature) {
		t.Fatal("A's own labelled traffic did not trip A's circuit")
	}

	// B spends another $1, then the threshold rises between A's $1 and B's $2 (token_events is
	// append-only, so the spend cannot leave the window here). The monitor closes A's circuit only.
	if err := mgr.RecordSpend(ctx, wsB, team, "", feature, "gpt-4o", 0, 100_000, "", "", "", "text", false); err != nil {
		t.Fatalf("RecordSpend B: %v", err)
	}
	mgr.rules[0].CircuitUSD = 1.5
	mgr.evaluateAllRules(ctx)
	if mgr.IsCircuitOpen(wsA, team, feature) {
		t.Fatal("monitor did not close A's circuit once A's spend fell under the threshold")
	}
	if !mgr.IsCircuitOpen(wsB, team, feature) {
		t.Fatal("monitor closed B's circuit on A's spend")
	}
}
