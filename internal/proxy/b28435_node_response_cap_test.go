package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/workspace"
)

// B28.435: a node answering /inference with 1 GB is read no further than maxNodeResponseBytes, and the
// request falls through to the direct path instead of holding the whole answer in memory.
func TestNodeRouting_OversizedAnswerIsCappedAndFallsThrough(t *testing.T) {
	const answerBytes = 1 << 30
	written := make(chan int64, 1)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n, _ := w.Write([]byte(`{"text":"`))
		total := int64(n)
		chunk := []byte(strings.Repeat("a", 64<<10))
		for total < answerBytes {
			n, err := w.Write(chunk)
			total += int64(n)
			if err != nil {
				break
			}
		}
		if total >= answerBytes {
			n, _ = w.Write([]byte(`"}`))
			total += int64(n)
		}
		written <- total
	}))
	t.Cleanup(node.Close)

	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	wireNode(t, p, "node-model", node.URL, node.Client())

	rec := httptest.NewRecorder()
	served := p.tryNodeRouting(rec, context.Background(),
		"vllm", "node-model", "a prompt", "a prompt",
		"", cache.Turn{}, "ws-log", "", "", "feat", "sess", "node-req-cap", false, "", localrouter.RoutingStrategy(""))
	if served {
		t.Fatalf("a 1 GB node answer was served (status=%d); it must be capped and fall through", rec.Code)
	}
	select {
	case n := <-written:
		if n >= answerBytes {
			t.Fatalf("the node wrote its whole %d-byte answer; Lens must stop reading at %d", n, maxNodeResponseBytes)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the node was still writing 30s after Lens gave up on it")
	}
}
