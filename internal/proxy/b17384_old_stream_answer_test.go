package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

// B17.384 — before B17.184 the stream path kept an answer as its text alone, with no finish_reason or stop_reason,
// so the "1 2 3 4 5 6 7 8" answers the nightly pooled while the provider was cutting them off still read as finished
// and were served as shared answers. A stored answer that cannot show it finished is not served: the request reaches
// the provider, the fresh answer replaces it, and that one (kept now with its stop reason) is served after.
func TestServe_AnOldStreamAnswerWithNoStopReasonIsNeverServed(t *testing.T) {
	cases := map[string]struct{ model, path, sse, old string }{
		"openai": {"gpt-4o", "/v1/proxy/openai/v1/chat/completions",
			"data: {\"choices\":[{\"delta\":{\"content\":\"1 2 3\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
			`{"choices":[{"message":{"content":"1 2 3 4 5 6 7 8","role":"assistant"}}]}`},
		"anthropic": {"claude-haiku-4-5", "/v1/proxy/anthropic/v1/messages",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"1 2 3\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			`{"content":[{"text":"1 2 3 4 5 6 7 8","type":"text"}]}`},
	}
	for provider, c := range cases {
		hits := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(c.sse))
		}))
		exact, mr := newExactCacheForTest(t)
		p := New(
			exact, nil, nil,
			compressor.New(), router.New(), pii.New(),
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
			"openai-key", "anthropic-key", "",
		)
		pointEveryProviderAt(p, upstream.URL)
		handle := p.HandleOpenAI
		if provider == "anthropic" {
			handle = p.HandleAnthropic
		}
		ask := func(step string) string {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{"model":"`+c.model+
				`","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Count from 1 to 100, one number per line. Word: heron."}]}`))
			req.Header.Set("Content-Type", "application/json")
			handle(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s: status %d, body %s", provider, step, w.Code, w.Body.String())
			}
			return w.Body.String()
		}

		ask("first ask")
		keys := mr.Keys()
		if len(keys) == 0 {
			t.Fatalf("%s: the finished streamed answer was not kept", provider)
		}
		for _, k := range keys {
			if !strings.HasSuffix(k, ":owner") {
				if err := mr.Set(k, c.old); err != nil { // the answer as the stream path kept it before B17.184
					t.Fatal(err)
				}
			}
		}
		if body := ask("old answer"); strings.Contains(body, "8") {
			t.Errorf("%s: the old stream answer with no stop reason was served: %s", provider, body)
		}
		if hits != 2 {
			t.Errorf("%s: the provider was asked %d time(s) with an old stream answer stored, want 2", provider, hits)
		}
		ask("fresh answer")
		if hits != 2 {
			t.Errorf("%s: the fresh streamed answer that replaced it was not served from the cache (%d provider asks, want 2)", provider, hits)
		}
		upstream.Close()
	}
}
