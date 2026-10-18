package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// stuckHoldSpender is Postgres frozen mid-query: the hold and the debit return only when their context ends.
type stuckHoldSpender struct {
	fundingSpender
	deadline *time.Time
}

func (s stuckHoldSpender) ReserveLXCForAgent(ctx context.Context, _, _, _ string, _ int64, _ economy.AgentDebitMeta) error {
	*s.deadline, _ = ctx.Deadline()
	return context.DeadlineExceeded
}

func (s stuckHoldSpender) SpendLXCForAgent(ctx context.Context, _, _, _ string, _ int64, _ string, _ economy.AgentDebitMeta) error {
	*s.deadline, _ = ctx.Deadline()
	return context.DeadlineExceeded
}

// B39.5 (#174) — an agent's pre-serve hold and debit carry their own deadline, so a request caught mid-query when
// Postgres dies cannot wait out the 60 s request timeout, and the failure answers 503, never "insufficient balance".
func TestB395_APreServeHoldThatCannotFinishAnswers503WithinItsDeadline(t *testing.T) {
	var deadline time.Time
	on := func() bool { return true }
	p := &Proxy{agentSpender: stuckHoldSpender{deadline: &deadline}, agentAllocEnabled: on, agentDebitSalt: []byte("salt-b395")}

	for name, call := range map[string]func() error{
		"hold": func() error {
			_, err := p.agentReserve(context.Background(), "key-b395", "ws-b395", "gpt-4o", "hello there, agent", "req-b395", 256)
			return err
		},
		"debit": func() error {
			_, err := p.agentAllocate(context.Background(), "key-b395", "ws-b395", "gpt-4o", "hello there, agent", "req-b395")
			return err
		},
	} {
		deadline = time.Time{}
		start := time.Now()
		err := call()
		if deadline.IsZero() || deadline.Sub(start) > preServeHoldTimeout+time.Second {
			t.Fatalf("%s: the call ran with deadline %v, want one within %v", name, deadline, preServeHoldTimeout)
		}
		w := httptest.NewRecorder()
		writeAgentRefusal(w, err)
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "insufficient") {
			t.Fatalf("%s: a hold that could not finish answered %d %q, want 503", name, w.Code, w.Body.String())
		}
	}
}
