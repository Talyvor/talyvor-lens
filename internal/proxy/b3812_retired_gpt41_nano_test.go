package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B38.12 — a request for gpt-4.1-nano, which OpenAI shut down on 2026-10-23, is refused on the OpenAI seam with
// a "model retired" error naming gpt-5.6-luna, streamed or not and under its dated id, and is never sent upstream.
func TestServe_RetiredGPT41NanoIsRefusedNamingGPT56Luna(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	p := newProxyWithFallback(t, upstream.URL, upstream.URL, upstream.URL)

	for _, body := range []string{
		`{"model":"gpt-4.1-nano","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"gpt-4.1-nano","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		`{"model":"gpt-4.1-nano-2025-04-14","messages":[{"role":"user","content":"hi"}]}`,
	} {
		w := newFlushRecorder()
		p.HandleOpenAI(w, httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body)))
		var got struct{ Error string }
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusGone || !strings.HasPrefix(got.Error, "model retired: ") || !strings.Contains(got.Error, "use gpt-5.6-luna") {
			t.Errorf("%s → %d %q, want 410 \"model retired: … use gpt-5.6-luna\"", body, w.Code, w.Body.String())
		}
	}
	if hits != 0 {
		t.Errorf("a retired model's request reached the upstream %d times, want none", hits)
	}
}
