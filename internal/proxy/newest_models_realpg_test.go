package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B15.4 — THE NEWEST MODELS ANSWER THROUGH THE CHAT, AT THEIR OFFICIAL PRICES.
//
// Each model is asked through the real handler with the chat's own credential (a session key) and
// the chat's own body (apps/web/src/areas/chat/chatApi.ts), streamed. The upstream reports 10,000
// input and 100 output tokens, and the charge is read off the prepaid ledger row, against a figure
// computed HERE from the provider's published price — not from the catalog, which is what is under test.

// newestModelUpstream streams an answer in the provider's wire format, and — like OpenAI — refuses a
// GPT-6 body that carries max_tokens or temperature. It records the last body it was sent.
type newestModelUpstream struct {
	mu       sync.Mutex
	lastBody map[string]any
}

func (u *newestModelUpstream) serve(t *testing.T, anthropic bool, calls *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.lastBody = body
		u.mu.Unlock()
		model, _ := body["model"].(string)
		if strings.HasPrefix(model, "gpt-6") {
			for _, f := range []string{"max_tokens", "temperature"} {
				if _, sent := body[f]; sent {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, `{"error":{"message":"Unsupported parameter: '%s' is not supported with this model."}}`, f)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if anthropic {
			_, _ = io.WriteString(w, chatSSEWithUsage("Hello from "+model, 10000, 100))
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello from "+model+"\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatSSEWithUsage(answer string, in, out int) string {
	return fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}}\n\n", in) +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + jsonString(answer) + "}}\n\n" +
		fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", out) +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// askNewestModel sends one chat turn for model through the real handler and returns the recorder.
func askNewestModel(t *testing.T, p *Proxy, anthropic, stream bool, body string) *http.Response {
	t.Helper()
	path := "/v1/proxy/openai/v1/chat/completions"
	if anthropic {
		path = "/v1/proxy/anthropic/v1/messages"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	req = req.WithContext(auth.WithAuthContext(req.Context(), sessionKeyAuthContext(t, "ws-log")))
	var rec interface {
		http.ResponseWriter
		Result() *http.Response
	} = httptest.NewRecorder()
	if stream {
		rec = newFlushRecorder()
	}
	if anthropic {
		p.HandleAnthropic(rec, req)
	} else {
		p.HandleOpenAI(rec, req)
	}
	return rec.Result()
}

func TestNewestModels_EachAnswersThroughTheChatStreamedAtItsOfficialPrice(t *testing.T) {
	cases := []struct {
		model             string
		anthropic         bool
		inPer1M, outPer1M float64 // the provider's published price — see the source cited in seed.go
	}{
		{"claude-opus-5-5", true, 4.00, 20.00},
		{"claude-fable-5-1", true, 10.00, 50.00},
		{"claude-sonnet-5-5", true, 2.00, 10.00}, // B37.15
		{"claude-haiku-5-5", true, 0.10, 0.50},
		{"gpt-6-astra", false, 10.00, 50.00},
		{"gpt-6-sol", false, 2.00, 10.00},
		{"gpt-6-luna", false, 0.10, 0.50},
		{"gpt-6.1-sol", false, 2.00, 10.00},  // B38.4
		{"gpt-6.1-terra", false, 1.00, 5.00}, // B38.10
		{"gpt-5.2", false, 1.75, 14.00},      // B38.5
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			var calls int64
			up := &newestModelUpstream{}
			srv := up.serve(t, tc.anthropic, &calls)
			p.openAIURL, p.anthropicURL = srv.URL, srv.URL

			// The chat's own bodies: Anthropic requires max_tokens and the chat sends 4096; the OpenAI
			// body carries none.
			q := jsonString("b154 " + tc.model + " " + strings.Repeat("x", 400))
			body := `{"model":"` + tc.model + `","stream":true,"messages":[{"role":"user","content":` + q + `}]}`
			if tc.anthropic {
				body = `{"model":"` + tc.model + `","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":` + q + `}]}`
			}
			res := askNewestModel(t, p, tc.anthropic, true, body)
			got, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK || !strings.Contains(string(got), "Hello from "+tc.model) {
				t.Fatalf("status=%d body=%.300q — the model did not answer", res.StatusCode, got)
			}
			if atomic.LoadInt64(&calls) != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
			want := settleULXC(10000*tc.inPer1M/1e6 + 100*tc.outPer1M/1e6)
			rows, debited, desc := prepaidDebits(t, pool)
			if rows != 1 || debited != want || desc != "chat: metered usage" {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC, %q; want 1 row of %d µLXC (10k in + 100 out at $%.2f/$%.2f per 1M), %q",
					rows, debited, desc, want, tc.inPer1M, tc.outPer1M, "chat: metered usage")
			}
		})
	}
}

// A client that sends GPT-6 the fields every earlier OpenAI model took — max_tokens and temperature —
// is answered, not 400'd: Lens moves max_tokens to max_completion_tokens and drops temperature, on
// both copies of the upstream call.
func TestNewestModels_GPT6AcceptsAnOrdinaryChatBody_BothSeams(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			p, _, _, _ := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			var calls int64
			up := &newestModelUpstream{}
			if stream {
				p.openAIURL = up.serve(t, false, &calls).URL
			} else {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					atomic.AddInt64(&calls, 1)
					raw, _ := io.ReadAll(r.Body)
					var body map[string]any
					_ = json.Unmarshal(raw, &body)
					up.mu.Lock()
					up.lastBody = body
					up.mu.Unlock()
					if _, bad := body["max_tokens"]; bad {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if _, bad := body["temperature"]; bad {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Hello from gpt-6-sol"}}],"usage":{"prompt_tokens":10000,"completion_tokens":100}}`)
				}))
				t.Cleanup(srv.Close)
				p.openAIURL = srv.URL
			}
			body := fmt.Sprintf(`{"model":"gpt-6-sol","max_tokens":777,"temperature":0.2,%s"messages":[{"role":"user","content":%s}]}`,
				map[bool]string{true: `"stream":true,`, false: ""}[stream], jsonString(fmt.Sprintf("b154-seam-%v %s", stream, strings.Repeat("y", 400))))
			res := askNewestModel(t, p, false, stream, body)
			got, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK || !strings.Contains(string(got), "Hello from gpt-6-sol") {
				t.Fatalf("status=%d body=%.300q — GPT-6 refused the body", res.StatusCode, got)
			}
			up.mu.Lock()
			defer up.mu.Unlock()
			if n, _ := up.lastBody["max_completion_tokens"].(float64); n != 777 {
				t.Errorf("upstream max_completion_tokens = %v, want the caller's max_tokens 777; body=%v", up.lastBody["max_completion_tokens"], up.lastBody)
			}
		})
	}
}
