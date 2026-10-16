package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/workspace"
)

// B42.2: X-Talyvor-Session naming another workspace's session is refused with 404 before the
// model is called, and that session gains no turn; the caller's own session still works.
func TestProxy_SessionHeader_OtherWorkspacesSessionIs404(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	var upstreamCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	p.openAIURL = up.URL
	p.sessionTracker = session.New(nil)
	p.sessionTracker.GetOrCreate(context.Background(), "sess-a", "ws-a", "agent-a")

	call := func(sessionID string) *httptest.ResponseRecorder {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "ws-log")
		req.Header.Set("X-Talyvor-Session", sessionID)
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w
	}

	if w := call("sess-a"); w.Code != http.StatusNotFound {
		t.Fatalf("naming ws-a's session: status %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Errorf("the model was called %d time(s) for a refused session", n)
	}
	if s, _ := p.sessionTracker.GetSession("sess-a"); s.TurnCount != 0 || s.WorkspaceID != "ws-a" {
		t.Errorf("ws-a's session changed: %+v", s)
	}

	if w := call("sess-own"); w.Code != http.StatusOK {
		t.Fatalf("own session: status %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if s, ok := p.sessionTracker.GetSession("sess-own"); !ok || s.TurnCount != 1 || s.WorkspaceID != "ws-log" {
		t.Errorf("own session = %+v (found %v), want one turn in ws-log", s, ok)
	}
}
