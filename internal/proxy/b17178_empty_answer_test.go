package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

// B17.178 — the testers' agent-limit-boost asked "What is 8868 + 7543?" on three nights, in three new workspaces,
// and was answered "" each time: an empty answer, once stored, was served to every later asker. An answer that says
// nothing is never stored, and one already stored is never served — the request goes to the provider instead.
func TestServe_AnEmptyAnswerIsNeitherStoredNorServed(t *testing.T) {
	const question = "What is 8868 + 7543? Reply with the number only."
	const empty = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":0}}`
	const good = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"16411"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":3}}`

	hits := 0
	answer := empty
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(upstream.Close)

	exact, _ := newExactCacheForTest(t)
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	pointEveryProviderAt(p, upstream.URL)

	body := `{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"` + question + `"}]}`
	ask := func() string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	// The provider says nothing: the asker gets that, and nothing is stored for the next one.
	key := cache.FingerprintedKey(question, cache.RequestFingerprint([]byte(body)))
	ask()
	if stored, _ := exact.Get(context.Background(), "anthropic", "claude-haiku-4-5", key); stored != nil {
		t.Fatalf("the empty answer was stored: %s", stored)
	}
	answer = good
	if got := ask(); hits != 2 || !strings.Contains(got, "16411") {
		t.Fatalf("after an empty answer the same question reached the provider %d time(s) and got %s; want 2 and 16411", hits, got)
	}
	if ask(); hits != 2 {
		t.Fatalf("the provider's real answer was not stored: the provider was asked %d times, want 2", hits)
	}

	// An empty answer stored before this fix is not served: the provider answers, and its answer replaces it.
	if err := exact.Set(context.Background(), "anthropic", "claude-haiku-4-5", key, []byte(empty)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := ask(); hits != 3 || !strings.Contains(got, "16411") {
		t.Fatalf("a stored empty answer: the provider was asked %d times and the asker got %s; want 3 and 16411", hits, got)
	}
}

// An answer that only calls a tool says something, in either shape, and stays cacheable.
func TestEmptyAnswer_ToolCallsAndTextAreNotEmpty(t *testing.T) {
	for body, want := range map[string]bool{
		`{"content":[]}`: true,
		`{"content":[{"type":"text","text":" "}]}`:                             true,
		`{"content":[{"type":"tool_use","id":"t","name":"f","input":{}}]}`:     false,
		`{"choices":[{"message":{"content":""}}]}`:                             true,
		`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c"}]}}]}`: false,
		`{"choices":[{"message":{"content":"16411"}}]}`:                        false,
		`{"data":[{"embedding":[0.1]}]}`:                                       false,
	} {
		if got := emptyAnswer([]byte(body)); got != want {
			t.Errorf("emptyAnswer(%s) = %v, want %v", body, got, want)
		}
	}
}
