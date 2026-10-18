package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/coderun"
)

// B17.226 — WITH RUN CODE ON, A CHAT KEPT OUT OF THE POOL PUTS NO ANSWER IN IT, AND A CHAT THAT SHARES DOES.
//
// The testers' chat-pool-off sequence, through the real handler and sandbox, with Chat's Run code on in both
// workspaces: wsPoolA asks in a kept-out chat and wsPoolB, asking the same, goes to the model; wsPoolA asks another
// question in a chat that shares and wsPoolB is served it from the pool, the model not asked. Before B17.225 no
// code-running answer was stored at all, so the control failed every night.
func TestB17226_WithRunCodeOnAKeptOutAnswerIsNeverPooledAndASharedOneIs(t *testing.T) {
	runner, err := coderun.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := chatCacheProxy(t, "")
	p.SetCodeRunner(runner)
	up := &b17108Upstream{}
	p.anthropicURL = up.serve(t).URL + "/v1/messages"

	ask := func(ws, question string, header ...string) (*flushRecorder, int) {
		t.Helper()
		up.mu.Lock()
		before := len(up.bodies)
		up.mu.Unlock()
		body := `{"model":"claude-haiku-4-5","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"` + question + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set(RunCodeHeader, "on")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := newFlushRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "541") {
			t.Fatalf("%s: status %d, answer without 541: %.600s", ws, w.Code, w.Body.String())
		}
		up.mu.Lock()
		defer up.mu.Unlock()
		return w, len(up.bodies) - before
	}

	kept, shared := "What is the 100th prime? b17226 kept", "What is the 100th prime? b17226 shared"
	ask("wsPoolA", kept, PoolHeader, "off")
	if _, asked := ask("wsPoolB", kept); asked != 2 {
		t.Fatalf("wsPoolB asked the model %d times; want 2 — it was served the kept-out chat's answer", asked)
	}
	ask("wsPoolA", shared)
	w, asked := ask("wsPoolB", shared)
	if asked != 0 {
		t.Fatalf("wsPoolB asked the model %d times; want 0 — the chat that shares put no answer in the pool", asked)
	}
	if !strings.Contains(w.Body.String(), "The 100th prime is 541.") {
		t.Errorf("wsPoolB was not served the shared chat's finished answer: %.400q", w.Body.String())
	}
}
