package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B28.186 — USE A LISTING IN CHAT.
//
// A Chat request naming a marketplace listing (X-Talyvor-Listing: <listing id>, sent by the BFF for a conversation
// attached to one) is one use of it: the listing's instructions go in as the request's system prompt, the model call
// itself is billed to the buyer as any request is, and the use is charged as POST …/marketplace/listings/{id}/use
// charges it (internal/market/use.go) — a paid listing's price on the buyer's marketplace bill. The answer says how
// in X-Talyvor-Listing-Charge (billed, free, own, linked, licensed or trial). A request whose model call fails is not
// charged. Only a skill or an agent can be asked in Chat.
//
// Each request is one use: a question whose model calls a tool takes a request for each round, and each is charged.

const (
	listingHeader       = "X-Talyvor-Listing"
	listingChargeHeader = "X-Talyvor-Listing-Charge"
	// The body formats a listing can be asked in: the system prompt goes in differently in each.
	chatShapeOpenAI    = "openai" // a system message first: OpenAI and the OpenAI-compatible providers
	chatShapeAnthropic = "anthropic"
)

// chatListing wraps a proxy route whose requests have the body format shape ("" for one a listing cannot be asked in).
func chatListing(store *market.Store, meter market.Meter, agents marketAgents, shape string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			listingID := strings.TrimSpace(r.Header.Get(listingHeader))
			if listingID == "" {
				next.ServeHTTP(w, r)
				return
			}
			if shape == "" {
				writeJSONErr(w, http.StatusBadRequest, "a marketplace listing can be asked in Chat only through an OpenAI-compatible or an Anthropic model")
				return
			}
			actx := auth.GetAuthContext(r.Context())
			if actx == nil || actx.WorkspaceID == "" {
				writeJSONErr(w, http.StatusUnauthorized, "a listing is used by a workspace")
				return
			}
			wsID := actx.WorkspaceID
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20)) // the proxy's own cap
			if err != nil {
				writeJSONErr(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "request body must be a JSON object")
				return
			}
			var model string
			_ = json.Unmarshal(body["model"], &model)
			agentID, person, err := marketCaller(r, agents, wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			m, byKind := meter, stripeByKind{}
			if k, ok := meter.(stripeByKind); ok {
				m, byKind = k.meterFor(wsID), k
			}
			lw := &listingWriter{ResponseWriter: w}
			run := &chatListingRunner{next: next, w: lw, r: r, body: body, shape: shape}
			deps := market.UseDeps{Runner: run, Meter: m, Agents: agents, Charged: func(u market.Use) {
				w.Header().Set(listingChargeHeader, u.Charge)
				run.kind = u.Kind
			}}
			// The runner sends the chat's own messages; Input only satisfies the use's plan, which needs one. A chat that
			// hangs up once its answer is in cannot cancel the use's record and its bill: only the model call is the chat's.
			u, err := store.Use(context.WithoutCancel(r.Context()), deps, wsID, agentID, listingID, market.UseRequest{Model: model, Input: "(the chat's question)", Person: person})
			switch {
			case err != nil && lw.status != 0:
				// The model call answered, and was relayed as it is; the use was removed and is not charged.
			case err != nil:
				writeMarketUseErr(lw, err, byKind, wsID)
			case u.MeterError != "":
				slog.Warn("market: a chat's use ran but is not yet on the buyer's bill; the next pass bills it",
					"use", u.ID, "workspace", wsID, "err", u.MeterError)
			}
		}
	}
}

// chatListingRunner is the use's one model call: the chat's own request, with the listing's instructions as its system
// prompt, served by the proxy straight to the chat.
type chatListingRunner struct {
	next  http.Handler
	w     *listingWriter
	r     *http.Request
	body  map[string]json.RawMessage
	shape string
	kind  string // the listing's, once the use is charged
	ran   bool
}

func (c *chatListingRunner) Run(_ context.Context, _ string, messages []market.Message) (string, error) {
	if (c.kind != "skill" && c.kind != "agent") || c.ran || len(messages) == 0 || messages[0].Role != "system" {
		return "", fmt.Errorf("%w in Chat: only a skill or an agent can be asked there", market.ErrNotRunnable)
	}
	c.ran = true
	guard := market.NewPromptGuard(messages[0].Content)
	c.w.guard, c.w.shape = &guard, c.shape
	if err := withSystem(c.body, c.shape, messages[0].Content); err != nil {
		return "", fmt.Errorf("%w: %v", market.ErrInvalid, err)
	}
	b, _ := json.Marshal(c.body)
	req := c.r.Clone(c.r.Context())
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	c.next.ServeHTTP(c.w, req)
	if err := c.w.finish(); err != nil {
		return "", err
	}
	if c.w.status == 0 {
		c.w.status = http.StatusOK // written without a WriteHeader
	}
	if c.w.status/100 != 2 {
		return "", &runError{status: c.w.status, body: "the model call was not answered"}
	}
	return "", nil
}

// withSystem puts instructions first in body's system prompt, before any the chat sent.
func withSystem(body map[string]json.RawMessage, shape, instructions string) error {
	if strings.TrimSpace(instructions) == "" {
		return nil
	}
	if shape == chatShapeAnthropic {
		// Anthropic's system is a string or a list of text blocks.
		var blocks []json.RawMessage
		if s := bytes.TrimSpace(body["system"]); len(s) > 0 && s[0] == '[' {
			if err := json.Unmarshal(s, &blocks); err != nil {
				return fmt.Errorf("system: %v", err)
			}
		} else if len(s) > 0 && string(s) != "null" {
			var text string
			if err := json.Unmarshal(s, &text); err != nil {
				return fmt.Errorf("system: %v", err)
			}
			blocks = []json.RawMessage{rawJSON(map[string]string{"type": "text", "text": text})}
		}
		body["system"] = rawJSON(append([]json.RawMessage{rawJSON(map[string]string{"type": "text", "text": instructions})}, blocks...))
		return nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(body["messages"], &msgs); err != nil {
		return fmt.Errorf("messages: %v", err)
	}
	body["messages"] = rawJSON(append([]json.RawMessage{rawJSON(map[string]string{"role": "system", "content": instructions})}, msgs...))
	return nil
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// listingWriter relays the proxy's answer and keeps the status it answered; the use's charge is named only on a 2xx.
// With a guard, a 2xx answer is read before the chat gets it, and one that repeats the listing's instructions is
// withheld as POST …/use withholds it (B50.31): a stream is relayed frame by frame until the frame that would repeat
// them, which ends it with the withheld text instead; a whole answer is held until it is complete.
type listingWriter struct {
	http.ResponseWriter
	status int
	guard  *market.PromptGuard
	shape  string
	stream bool
	held   []byte                  // what the proxy wrote that is not yet relayed
	said   map[string]*market.Said // each string field of the stream's frames, by its path, so far
	block  int                     // the content block an Anthropic stream is in
	cut    bool                    // withheld: nothing more is relayed
}

func (w *listingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		if code/100 != 2 {
			w.Header().Del(listingChargeHeader)
		}
		if w.guarding() {
			w.Header().Del("Content-Length") // a withheld answer is not as long as the one the proxy wrote
			w.stream = strings.Contains(w.Header().Get("Content-Type"), "event-stream")
		}
		// B43.23 (CodeQL go/reflected-xss #53, #54): the answer is JSON, an event stream or a plain refusal, never a page:
		// one with no type (which the server would sniff, and could echo the request as HTML) or an HTML one goes out as
		// plain text, and is never sniffed.
		h := w.ResponseWriter.Header()
		if ct := strings.ToLower(h.Get("Content-Type")); ct == "" || strings.Contains(ct, "html") {
			h.Set("Content-Type", "text/plain; charset=utf-8")
		}
		h.Set("X-Content-Type-Options", "nosniff")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *listingWriter) guarding() bool { return w.guard != nil && w.status/100 == 2 }

func (w *listingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if !w.guarding() {
		return w.ResponseWriter.Write(p)
	}
	if w.cut {
		return len(p), nil
	}
	w.held = append(w.held, p...)
	for w.stream && !w.cut {
		i := bytes.Index(w.held, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := w.held[:i+2]
		w.held = w.held[i+2:]
		if err := w.relay(frame); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// relay passes one frame of a stream to the chat, or ends the stream with the withheld text if, with it, the answer
// repeats the listing's instructions.
func (w *listingWriter) relay(frame []byte) error {
	leaks := false
	for _, line := range strings.Split(string(frame), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		v := decodeAnswer([]byte(strings.TrimSpace(data)))
		if m, ok := v.(map[string]any); ok {
			if i, ok := m["index"].(float64); ok {
				w.block = int(i)
			}
		}
		// Every string a frame carries is checked as its own run: the text, a tool call's arguments, reasoning, and
		// each choice of several apart, since their frames interleave.
		eachString("", v, func(path, s string) {
			if w.said == nil {
				w.said = map[string]*market.Said{}
			}
			if w.said[path] == nil {
				w.said[path] = &market.Said{}
			}
			leaks = w.guard.Add(w.said[path], s) || leaks
		})
	}
	if !leaks {
		_, err := w.ResponseWriter.Write(frame)
		return err
	}
	w.cut, w.held = true, nil
	withheld := "\n\n" + market.WithheldAnswer
	var end string
	if w.shape == chatShapeAnthropic {
		end = fmt.Sprintf("event: content_block_delta\ndata: %s\n\nevent: content_block_stop\ndata: %s\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			rawJSON(map[string]any{"type": "content_block_delta", "index": w.block, "delta": map[string]string{"type": "text_delta", "text": withheld}}),
			rawJSON(map[string]any{"type": "content_block_stop", "index": w.block}))
	} else {
		end = fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n",
			rawJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": withheld}, "finish_reason": "stop"}}}))
	}
	_, err := io.WriteString(w.ResponseWriter, end)
	return err
}

// finish relays what is still held once the proxy is done: the rest of a stream, or a whole answer, its text swapped
// for the withheld text if it repeats the listing's instructions.
func (w *listingWriter) finish() error {
	if !w.guarding() || w.cut || len(w.held) == 0 {
		return nil
	}
	held := w.held
	w.held = nil
	if w.stream {
		return w.relay(held) // shortcut: a stream framed other than by blank lines is held whole and read here
	}
	out, leaks := held, false
	said := map[string]*market.Said{}
	eachString("", decodeAnswer(held), func(path, s string) {
		path = arrayIndex.ReplaceAllString(path, "") // an answer's blocks, and its choices, are read as one text
		if said[path] == nil {
			said[path] = &market.Said{}
		}
		leaks = w.guard.Add(said[path], s) || leaks
	})
	if leaks {
		out = withheldBody(held, w.shape)
	}
	_, err := w.ResponseWriter.Write(out)
	return err
}

// decodeAnswer is a whole answer or one frame of a streamed one; one that is not JSON is its text, so it is checked too.
func decodeAnswer(data []byte) any {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}

var arrayIndex = regexp.MustCompile(`\.[0-9]+`)

// eachString calls f with the path and the value of every string in v.
func eachString(path string, v any, f func(path, s string)) {
	switch v := v.(type) {
	case string:
		f(path, v)
	case map[string]any:
		for k, e := range v {
			eachString(path+"."+k, e, f)
		}
	case []any:
		for i, e := range v {
			eachString(path+"."+strconv.Itoa(i), e, f)
		}
	}
}

// withheldBody is a whole answer whose content, tool calls and log probabilities are replaced by the withheld text;
// its id, model and usage are kept.
func withheldBody(body []byte, shape string) []byte {
	var v map[string]json.RawMessage
	_ = json.Unmarshal(body, &v)
	if v == nil {
		v = map[string]json.RawMessage{}
	}
	if shape == chatShapeAnthropic {
		v["content"] = rawJSON([]map[string]string{{"type": "text", "text": market.WithheldAnswer}})
		return rawJSON(v)
	}
	var choices []json.RawMessage
	_ = json.Unmarshal(v["choices"], &choices)
	withheld := make([]any, max(len(choices), 1))
	for i := range withheld {
		withheld[i] = map[string]any{"index": i, "message": map[string]string{"role": "assistant", "content": market.WithheldAnswer}, "finish_reason": "stop"}
	}
	v["choices"] = rawJSON(withheld)
	return rawJSON(v)
}

// Flush keeps a streamed answer streaming.
func (w *listingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *listingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
