package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/coderun"
)

// B17.248 — AN ANSWER WORKED OUT WITH RUN CODE IN A CHAT THAT SHARES IS SERVED FROM THE POOL TO A CHAT WITHOUT IT.
//
// The testers' chat-pool-off sequence, as it runs: the asker's tab has Run code on and the other workspace's does
// not. Before, Lens's run_code tool was part of the stored answer's fingerprint, so a chat without Run code never
// matched it and the pool served nothing. The other way stays shut: a chat with Run code on is not served an answer
// that was not worked out with it.
func TestB17248_ARunCodeAnswerIsPooledToAChatWithoutRunCodeAndNotTheOtherWay(t *testing.T) {
	runner, err := coderun.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := chatCacheProxy(t, "")
	p.SetCodeRunner(runner)
	up := &b17108Upstream{}
	p.anthropicURL = up.serve(t).URL + "/v1/messages"

	ask := func(ws, question string, runCode bool) (string, int) {
		t.Helper()
		up.mu.Lock()
		before := len(up.bodies)
		up.mu.Unlock()
		body := `{"model":"claude-haiku-4-5","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"` + question + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		if runCode {
			req.Header.Set(RunCodeHeader, "on")
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := newFlushRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %.600s", ws, w.Code, w.Body.String())
		}
		up.mu.Lock()
		defer up.mu.Unlock()
		return w.Body.String(), len(up.bodies) - before
	}

	ran, plain := "What is the 100th prime? b17248 ran", "What is the 100th prime? b17248 plain"
	if _, asked := ask("wsPoolA", ran, true); asked != 2 {
		t.Fatalf("wsPoolA asked the model %d times with Run code on; want 2 (the run, then the answer)", asked)
	}
	got, asked := ask("wsPoolB", ran, false)
	if asked != 0 {
		t.Fatalf("wsPoolB, Run code off, asked the model %d times; want 0 — the answer worked out with Run code was not pooled for it", asked)
	}
	if !strings.Contains(got, "The 100th prime is 541.") {
		t.Errorf("wsPoolB was not served the finished answer: %.400q", got)
	}

	ask("wsPoolA", plain, false)
	if _, asked := ask("wsPoolB", plain, true); asked == 0 {
		t.Fatal("wsPoolB, Run code on, was served from the pool an answer asked with Run code off")
	}
}
