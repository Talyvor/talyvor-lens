package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/coderun"
)

// B17.225 — WITH RUN CODE ON, A KEPT CHAT'S ANSWER IS SERVED FROM THE CACHE, AND A TEMPORARY CHAT'S NEVER IS.
//
// The testers' chat-temporary sequence, through the real handler and sandbox, with Chat's Run code on: a temporary
// chat asks the model; the same question in a kept chat is asked afresh (the temporary answer was kept nowhere); asked
// once more it is served the kept answer from the cache, the model not asked; a temporary chat again asks afresh.
// Before B17.225 every round of a code-running request bypassed the cache, so the third ask went to the model.
func TestB17225_WithRunCodeOnAKeptAnswerIsServedFromTheCacheAndATemporaryOneNever(t *testing.T) {
	runner, err := coderun.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := chatCacheProxy(t, "")
	p.SetCodeRunner(runner)
	up := &b17108Upstream{}
	p.anthropicURL = up.serve(t).URL + "/v1/messages"

	body := `{"model":"claude-haiku-4-5","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"What is the 100th prime? b17225"}]}`
	ask := func(header ...string) (*flushRecorder, int) {
		t.Helper()
		up.mu.Lock()
		before := len(up.bodies)
		up.mu.Unlock()
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "wsPoolA")
		req.Header.Set(RunCodeHeader, "on")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: "wsPoolA", AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := newFlushRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "541") {
			t.Fatalf("status %d, answer without 541: %.600s", w.Code, w.Body.String())
		}
		up.mu.Lock()
		defer up.mu.Unlock()
		return w, len(up.bodies) - before
	}
	temporary := []string{CacheBypassHeader, "bypass", CacheStoreHeader, "off"}

	if _, asked := ask(temporary...); asked != 2 {
		t.Fatalf("the temporary chat asked the model %d times; want 2 (the question, then the run's output)", asked)
	}
	if _, asked := ask(); asked != 2 {
		t.Fatalf("the kept chat asked the model %d times; want 2 — it was served the temporary chat's answer", asked)
	}
	w, asked := ask()
	if asked != 0 {
		t.Fatalf("asked a third time the model was asked %d times; want 0 — the kept answer was not served from the cache", asked)
	}
	if w.Header().Get("X-Talyvor-Cache-Replay") != "true" || !strings.Contains(w.Body.String(), "The 100th prime is 541.") {
		t.Errorf("the third ask was not the finished answer replayed from the cache: replay=%q body=%.400q",
			w.Header().Get("X-Talyvor-Cache-Replay"), w.Body.String())
	}
	// B17.246 — and it shows the code that worked it out, as the fresh answer did, ahead of the answer.
	stream := w.Body.String()
	run := strings.Index(stream, `"type":"talyvor.code_run"`)
	if run < 0 || run > strings.Index(stream, "The 100th prime is 541.") || !strings.Contains(stream[run:], `"stdout":"541`) {
		t.Errorf("the answer served from the cache carries no code_run frame printing 541 ahead of it: %.600q", stream)
	}
	if _, asked := ask(temporary...); asked != 2 {
		t.Errorf("a temporary chat after the answer was cached asked the model %d times; want 2 — it was served the cached answer", asked)
	}
}
