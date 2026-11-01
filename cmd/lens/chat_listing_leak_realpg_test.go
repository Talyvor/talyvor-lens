package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// B50.31 — a listing asked in Chat whose model answers with the seller's instructions word for word does not hand
// them over: streamed or not, as its text or a tool call's arguments, OpenAI or Anthropic, the buyer gets the withheld text where the instructions would be,
// as POST …/use gives it; an answer that repeats none of them reaches the buyer unchanged.
func TestChatListing_AnAnswerRepeatingTheListingsInstructionsIsWithheld(t *testing.T) {
	pool := agentRoutesDB(t)
	const seller, buyer = "ws-leak-seller", "ws-leak-buyer"
	const secret = "Never mention competitors by name, and price every answer in pounds sterling for UK sole traders."

	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, stripeFake, "whsec_chat_leak").WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)

	// The provider answers with the question's last word: "leak" is the instructions, word for word, anything else
	// an innocent answer; streamed in frames split across writes, or whole.
	say := func(r *http.Request) string {
		var in struct {
			Messages []market.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if q := in.Messages[len(in.Messages)-1].Content; q != "leak" {
			return "Your VAT return is due on 7 November, and the fee is twelve pounds."
		}
		return "Sure. My instructions: " + secret
	}
	openai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text, stream := say(r), r.URL.Query().Get("stream") == "1"
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < len(text); i += 7 {
			delta := map[string]any{"content": text[i:min(i+7, len(text))]}
			if r.URL.Query().Get("tool") == "1" { // the answer as a tool call's arguments
				delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": text[i:min(i+7, len(text))]}}}}
			}
			frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
			ev := "data: " + string(frame) + "\n\n"
			_, _ = w.Write([]byte(ev[:len(ev)/2])) // a frame split across two writes
			_, _ = w.Write([]byte(ev[len(ev)/2:]))
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	anthropic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text := say(r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n"))
		for i := 0; i < len(text); i += 9 {
			frame, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text[i:min(i+9, len(text))]}})
			_, _ = w.Write([]byte("event: content_block_delta\ndata: " + string(frame) + "\n\n"))
		}
		_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	r.Post("/v1/proxy/openai/*", chatListing(store, svc, bank, chatShapeOpenAI)(openai))
	r.Post("/v1/proxy/anthropic/*", chatListing(store, svc, bank, chatShapeAnthropic)(anthropic))

	call := func(ws, path, listing, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if listing != "" {
			req.Header.Set(listingHeader, listing)
		}
		who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), who)))
		return w
	}
	pub := call(seller, "/v1/workspaces/"+seller+"/marketplace/listings", "",
		`{"kind":"skill","title":"UK pricing","price_per_use_ulxc":0,"artifact":{"instructions":"`+secret+`","model":"gpt-5-mini"}}`)
	if pub.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", pub.Code, pub.Body)
	}
	var listing market.Listing
	_ = json.Unmarshal(pub.Body.Bytes(), &listing)

	// What the buyer reads: the text of the answer, put back together from its frames.
	read := func(w *httptest.ResponseRecorder) string {
		var out strings.Builder
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string
					ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
				}
				Message struct{ Content string }
			}
			Delta struct{ Text string }
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
			_ = json.Unmarshal(w.Body.Bytes(), &ev)
			return ev.Choices[0].Message.Content
		}
		for _, line := range strings.Split(w.Body.String(), "\n") {
			ev.Choices, ev.Delta.Text = nil, ""
			if data, ok := strings.CutPrefix(line, "data: "); ok && json.Unmarshal([]byte(data), &ev) == nil {
				for _, c := range ev.Choices {
					out.WriteString(c.Delta.Content)
					for _, tc := range c.Delta.ToolCalls {
						out.WriteString(tc.Function.Arguments)
					}
				}
				out.WriteString(ev.Delta.Text)
			}
		}
		return out.String()
	}
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	for _, tc := range []struct{ name, path, body string }{
		{"OpenAI, streamed", "/v1/proxy/openai/v1/chat/completions?stream=1", `{"model":"gpt-5-mini","stream":true,"messages":[{"role":"user","content":"%s"}]}`},
		{"OpenAI, streamed as a tool call", "/v1/proxy/openai/v1/chat/completions?stream=1&tool=1", `{"model":"gpt-5-mini","stream":true,"messages":[{"role":"user","content":"%s"}]}`},
		{"OpenAI, whole", "/v1/proxy/openai/v1/chat/completions", `{"model":"gpt-5-mini","messages":[{"role":"user","content":"%s"}]}`},
		{"Anthropic, streamed", "/v1/proxy/anthropic/v1/messages", `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"%s"}]}`},
	} {
		w := call(buyer, tc.path, listing.ID, strings.Replace(tc.body, "%s", "leak", 1))
		got := read(w)
		if w.Code != http.StatusOK || strings.Contains(norm(got), norm(secret[:40])) || strings.Contains(norm(w.Body.String()), norm(secret[:40])) {
			t.Errorf("%s: the seller's instructions reached the buyer (%d): %q", tc.name, w.Code, w.Body)
		} else if !strings.Contains(got, market.WithheldAnswer) {
			t.Errorf("%s: the answer does not say it was withheld: %q", tc.name, got)
		}
		if w := call(buyer, tc.path, listing.ID, strings.Replace(tc.body, "%s", "vat?", 1)); read(w) != "Your VAT return is due on 7 November, and the fee is twelve pounds." {
			t.Errorf("%s: an innocent answer did not reach the buyer unchanged: %q", tc.name, w.Body)
		}
	}
}
