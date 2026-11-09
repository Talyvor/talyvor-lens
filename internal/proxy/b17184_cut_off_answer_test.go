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

// B17.184 — the testers' chat-continue-cut-off asked for a count with max_tokens 16 and got "1 2 3 4 5 6 7 8" back
// as a shared answer with no "Cut off" mark: the cut-off answer had been kept, and the pool served it to the next
// users as finished. An answer cut off at the length limit reaches its asker unchanged and is never kept, so the
// same request sent again reaches the provider. A finished answer is still kept, so the harness is caching.
func TestServe_ACutOffAnswerIsNeverKept(t *testing.T) {
	type shape struct{ json, sse string }
	answers := map[string]map[string]shape{
		"openai": {
			"length": {
				`{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"1 2 3 4 5 6 7 8"},"finish_reason":"length"}],"usage":{"prompt_tokens":20,"completion_tokens":16}}`,
				"data: {\"choices\":[{\"delta\":{\"content\":\"1 2 3 4 5 6 7 8\"}}]}\n\n" +
					"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
					"data: [DONE]\n\n",
			},
			"stop": {
				`{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"1 2 3"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":6}}`,
				"data: {\"choices\":[{\"delta\":{\"content\":\"1 2 3\"}}]}\n\n" +
					"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: [DONE]\n\n",
			},
		},
		"anthropic": {
			"length": {
				`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"1 2 3 4 5 6 7 8"}],"stop_reason":"max_tokens","usage":{"input_tokens":20,"output_tokens":16}}`,
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"1 2 3 4 5 6 7 8\"}}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":16}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
			"stop": {
				`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"1 2 3"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":6}}`,
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"1 2 3\"}}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
		},
	}
	for _, provider := range []string{"openai", "anthropic"} {
		for _, stream := range []bool{false, true} {
			for reason, wantHits := range map[string]int{"length": 2, "stop": 1} {
				answer := answers[provider][reason]
				hits := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hits++
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte(answer.sse))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(answer.json))
				}))
				exact, _ := newExactCacheForTest(t)
				p := New(
					exact, nil, nil,
					compressor.New(), router.New(), pii.New(),
					nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
					fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
					"openai-key", "anthropic-key", "",
				)
				pointEveryProviderAt(p, upstream.URL)

				model, path, handle := "gpt-4o", "/v1/proxy/openai/v1/chat/completions", p.HandleOpenAI
				if provider == "anthropic" {
					model, path, handle = "claude-haiku-4-5", "/v1/proxy/anthropic/v1/messages", p.HandleAnthropic
				}
				body := `{"model":"` + model + `","max_tokens":16,"stream":` + map[bool]string{false: "false", true: "true"}[stream] +
					`,"messages":[{"role":"user","content":"Count from 1 to 100, one number per line. Word: heron."}]}`
				for range 2 {
					w := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					handle(w, req)
					if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "1 2 3") {
						t.Fatalf("%s stream=%v %s: status %d, body %s", provider, stream, reason, w.Code, w.Body.String())
					}
				}
				upstream.Close()
				if hits != wantHits {
					t.Errorf("%s stream=%v finish %q: the provider was asked %d time(s) for the same request twice, want %d",
						provider, stream, reason, hits, wantHits)
				}
			}
		}
	}
}
