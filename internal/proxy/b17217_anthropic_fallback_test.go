package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B17.217 — an Anthropic request that falls back to OpenAI is answered as an Anthropic message, not a chat
// completion its client reads as empty; and a body a text rewrite would lose is not sent on.
func TestB17217AnthropicFallbackAnswersInAnthropicShape(t *testing.T) {
	overloaded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	t.Cleanup(overloaded.Close)
	var sent []map[string]any
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		sent = append(sent, m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"17846"},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":3,"total_tokens":24}}`)
	}))
	t.Cleanup(openai.Close)
	p := newProxyWithFallback(t, openai.URL, overloaded.URL, "")

	ask := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		p.HandleAnthropic(w, req)
		return w
	}

	w := ask(`{"model":"claude-haiku-4-5","max_tokens":16,"system":[{"type":"text","text":"Be brief."}],"messages":[{"role":"user","content":"What is 8780 + 9066? Reply with the number only."}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Type    string `json:"type"`
		Content []struct {
			Type, Text string
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("answer is not JSON: %v; %s", err, w.Body.String())
	}
	if got.Type != "message" || len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "17846" ||
		got.StopReason != "end_turn" || got.Usage.InputTokens != 21 || got.Usage.OutputTokens != 3 {
		t.Fatalf("the Anthropic client was answered %s; want an Anthropic message saying 17846 with its usage", w.Body.String())
	}
	if len(sent) != 1 {
		t.Fatalf("OpenAI was sent %d requests, want 1", len(sent))
	}
	msgs, _ := sent[0]["messages"].([]any)
	if _, top := sent[0]["system"]; top || len(msgs) != 2 || sent[0]["model"] != "gpt-4o" || sent[0]["max_tokens"] != float64(16) {
		t.Fatalf("OpenAI was sent %v; want gpt-4o, max_tokens 16, the system prompt as its first message", sent[0])
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "Be brief." {
		t.Fatalf("OpenAI's first message is %v, want the system prompt", msgs[0])
	}

	sent = nil
	w = ask(`{"model":"claude-haiku-4-5","max_tokens":16,"tools":[{"name":"t","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	if len(sent) != 0 || w.Code != 529 {
		t.Fatalf("a request with tools reached OpenAI %d times and answered %d; want it not sent on, the 529 kept", len(sent), w.Code)
	}
}
