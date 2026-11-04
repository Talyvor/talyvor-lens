package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/catalog"
)

// B38.11 — Claude Sonnet 4.5 is served until Anthropic retires it at 2026-11-30T17:00Z; from then a request for
// it, streamed or not and under its dated id, is refused 410 naming claude-sonnet-5-5 and never sent upstream.
func TestServe_Sonnet45IsServedUntilItsRetirementThenRefusedNamingSonnet55(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	p := newProxyWithFallback(t, upstream.URL, upstream.URL, upstream.URL)
	bodies := []string{
		`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
	}
	send := func(body string) *flushRecorder {
		w := newFlushRecorder()
		p.HandleAnthropic(w, httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body)))
		return w
	}

	restore := catalog.SetClock(func() time.Time { return time.Date(2026, 11, 30, 16, 59, 0, 0, time.UTC) })
	if w := send(bodies[0]); w.Code == http.StatusGone || hits != 1 {
		t.Errorf("before retirement → %d %q with %d upstream hits, want it served", w.Code, w.Body.String(), hits)
	}
	restore()

	hits = 0
	defer catalog.SetClock(func() time.Time { return time.Date(2026, 11, 30, 17, 0, 0, 0, time.UTC) })()
	for _, body := range bodies {
		w := send(body)
		var got struct{ Error string }
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusGone || !strings.HasPrefix(got.Error, "model retired: ") || !strings.Contains(got.Error, "use claude-sonnet-5-5") {
			t.Errorf("%s → %d %q, want 410 \"model retired: … use claude-sonnet-5-5\"", body, w.Code, w.Body.String())
		}
	}
	if hits != 0 {
		t.Errorf("a retired model's request reached the upstream %d times, want none", hits)
	}
}
