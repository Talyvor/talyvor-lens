package proxy

import (
	"testing"

	"github.com/talyvor/lens/internal/coderun"
)

// B17.246 — WITH RUN CODE ON, AN ANSWER STORED WITHOUT THE RUNS THAT WORKED IT OUT IS NEVER SERVED.
//
// The testers' chat-run-code was served, from the shared pool, answers kept before the runs were stored with them —
// one of them a round's "I'll write some JavaScript code" — and so showed no code. Here the model answers without
// running code, twice: the second ask goes to the model, not to the answer the first one stored.
func TestB17246_WithRunCodeOnAnAnswerWithoutItsRunsIsNotServedFromTheCache(t *testing.T) {
	runner, err := coderun.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	p, _, calls := chatCacheProxy(t, chatSSE("The capital of the UK is London.", true))
	p.SetCodeRunner(runner)
	chatAsk(t, p, "wsPoolA", RunCodeHeader, "on")
	chatAsk(t, p, "wsPoolA", RunCodeHeader, "on")
	if *calls != 2 {
		t.Fatalf("the model was asked %d times; want 2 — the second ask was served an answer that ran no code", *calls)
	}
	// Control: with Run code off the same kept answer is served (B17.248), so the cache is live here.
	chatAsk(t, p, "wsPoolA")
	if *calls != 2 {
		t.Fatalf("with Run code off the model was asked again (%d calls) — the cache kept nothing, so the check above proves nothing", *calls)
	}
}
