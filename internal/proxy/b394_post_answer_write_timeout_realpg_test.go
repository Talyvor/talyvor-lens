package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/metrics"
)

// timedFlushRecorder notes when the answer's text first reached the client.
type timedFlushRecorder struct {
	*flushRecorder
	mu   sync.Mutex
	sent time.Time
}

func (f *timedFlushRecorder) Flush() {
	f.flushRecorder.Flush()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent.IsZero() && strings.Contains(f.Body.String(), "hi") {
		f.sent = time.Now()
	}
}

// TestB394_SettleOnLockedBalanceGivesUp: B39.4. Another transaction holds the agent's balance row while the
// answer streams, so the settle after the answer cannot get its lock. It must give up within
// postAnswerWriteTimeout — not wait forever holding a connection — fail as a failed settle does (counted in
// lens_agent_settle_failures_total), and leave the hold for the sweeper to refund, never charge it twice.
func TestB394_SettleOnLockedBalanceGivesUp(t *testing.T) {
	p, _, pool := costWireProxy(t)
	ctx := context.Background()

	var lock pgx.Tx
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The hold is already taken; lock the balance row the settle needs before answering.
		tx, err := pool.Begin(ctx)
		if err == nil {
			_, err = tx.Exec(ctx, `SELECT 1 FROM lxc_balances WHERE workspace_id = 'ws-log' FOR UPDATE`)
		}
		if err != nil {
			t.Errorf("lock balance row: %v", err)
		}
		lock = tx
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, streamCachedSSE)
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() {
		if lock != nil {
			_ = lock.Rollback(ctx)
		}
	})
	p.openAIURL = up.URL

	failed := metrics.AgentSettleFailuresTotal.WithLabelValues("reservation")
	before := testutil.ToFloat64(failed)
	var gaveUp time.Time
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				if testutil.ToFloat64(failed) > before {
					gaveUp = time.Now() // counted in the last poll interval, as the request returned
				}
				return
			case <-time.After(20 * time.Millisecond):
				if testutil.ToFloat64(failed) > before {
					gaveUp = time.Now()
					return
				}
			}
		}
	}()

	prompt := strings.Repeat("x", 40000)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"`+prompt+`"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-1", WorkspaceID: "ws-log"}))
	w := &timedFlushRecorder{flushRecorder: newFlushRecorder()}

	done := make(chan struct{})
	go func() { defer close(done); p.HandleOpenAI(w, req) }()
	select {
	case <-done:
	case <-time.After(4 * postAnswerWriteTimeout):
		t.Fatal("the request never finished: a write after the answer is waiting on the locked row")
	}
	close(stop)
	<-watched

	w.mu.Lock()
	sent := w.sent
	w.mu.Unlock()
	if w.Code != http.StatusOK || sent.IsZero() {
		t.Fatalf("the answer was not sent: status %d, body %q", w.Code, w.Body.String())
	}
	if gaveUp.IsZero() {
		t.Fatal("the settle's failure was not counted in lens_agent_settle_failures_total")
	}
	if waited := gaveUp.Sub(sent); waited < postAnswerWriteTimeout-time.Second || waited > postAnswerWriteTimeout+3*time.Second {
		t.Fatalf("the settle gave up %v after the answer was sent, want about %v", waited, postAnswerWriteTimeout)
	}
	if st := reservationStatus(t, pool, "ws-log"); st != "held" {
		t.Fatalf("reservation status = %q, want 'held' for the sweeper to refund (a timed-out settle charges nothing)", st)
	}
}
