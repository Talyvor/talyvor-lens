package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
//
// B41.79 — CALL ANY LISTING AS A MODEL. A request whose model is "listing/<listing id>" is the same use, from any app or
// SDK: the listing's own model answers, and a prompt listing's filled template goes in as its instructions, its
// {{variables}} taken from the body's "variables" object (an SDK's extra_body), which is not sent on.

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
			if listingID == "" && shape == "" {
				next.ServeHTTP(w, r)
				return
			}
			if shape == "" {
				writeJSONErr(w, http.StatusBadRequest, "a marketplace listing can be asked in Chat only through an OpenAI-compatible or an Anthropic model")
				return
			}
			const maxBody = 4 << 20 // the proxy's own cap
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "read body: "+err.Error())
				return
			}
			// The body read so far is put back in front of the rest: a request that names no listing reaches the proxy as it came.
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(raw), r.Body), r.Body}
			var body map[string]json.RawMessage
			var model string
			if json.Unmarshal(raw, &body) == nil {
				_ = json.Unmarshal(body["model"], &model)
			}
			viaModel := listingID == "" && strings.HasPrefix(model, listingModelPrefix)
			if listingID == "" && !viaModel {
				next.ServeHTTP(w, r)
				return
			}
			if len(raw) > maxBody {
				writeJSONErr(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			if body == nil {
				writeJSONErr(w, http.StatusBadRequest, "request body must be a JSON object")
				return
			}
			if viaModel {
				listingID, model = strings.TrimSpace(strings.TrimPrefix(model, listingModelPrefix)), "" // "": the listing's own model
				if listingID == "" {
					writeJSONErr(w, http.StatusBadRequest, `model "listing/" names no listing: use "listing/<listing id>"`)
					return
				}
			}
			var variables map[string]string
			if v, ok := body["variables"]; ok {
				if err := json.Unmarshal(v, &variables); err != nil {
					writeJSONErr(w, http.StatusBadRequest, "variables must be an object of strings: "+err.Error())
					return
				}
				delete(body, "variables")
			}
			actx := auth.GetAuthContext(r.Context())
			if actx == nil || actx.WorkspaceID == "" {
				writeJSONErr(w, http.StatusUnauthorized, "a listing is used by a workspace")
				return
			}
			wsID := actx.WorkspaceID
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
			run := &chatListingRunner{next: next, w: lw, r: r, body: body, shape: shape, setModel: viaModel}
			deps := market.UseDeps{Runner: run, Meter: m, Agents: agents, Charged: func(u market.Use) {
				w.Header().Set(listingChargeHeader, u.Charge)
				run.kind = u.Kind
			}}
			// The runner sends the chat's own messages; Input only satisfies the use's plan, which needs one. A chat that
			// hangs up once its answer is in cannot cancel the use's record and its bill: only the model call is the chat's.
			u, err := store.Use(context.WithoutCancel(r.Context()), deps, wsID, agentID, listingID, market.UseRequest{Model: model, Input: "(the chat's question)", Variables: variables, Person: person})
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
	// setModel: the request named the listing as its model, so the model the use resolved is the one called.
	setModel bool
}

// listingModelPrefix starts a model id that names a listing (B41.79).
const listingModelPrefix = "listing/"

func (c *chatListingRunner) Run(_ context.Context, model string, messages []market.Message) (string, error) {
	// A skill's or an agent's instructions are its system prompt; a prompt's are its filled template.
	asked := (c.kind == "skill" || c.kind == "agent") && len(messages) > 0 && messages[0].Role == "system" ||
		c.kind == "prompt" && len(messages) == 1
	if !asked || c.ran {
		return "", fmt.Errorf("%w in Chat: only a skill, an agent or a prompt can be asked there", market.ErrNotRunnable)
	}
	c.ran = true
	if c.setModel {
		c.body["model"] = rawJSON(model)
	}
	if err := withSystem(c.body, c.shape, messages[0].Content); err != nil {
		return "", fmt.Errorf("%w: %v", market.ErrInvalid, err)
	}
	b, _ := json.Marshal(c.body)
	req := c.r.Clone(c.r.Context())
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	c.next.ServeHTTP(c.w, req)
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
type listingWriter struct {
	http.ResponseWriter
	status int
}

func (w *listingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		if code/100 != 2 {
			w.Header().Del(listingChargeHeader)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Flush keeps a streamed answer streaming.
func (w *listingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *listingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
