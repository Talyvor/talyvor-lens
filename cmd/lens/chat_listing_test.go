package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/market"
)

// B28.186 — a listing's instructions go first in an Anthropic request's system prompt, before the chat's own.
func TestWithSystem_AnthropicPutsTheInstructionsFirst(t *testing.T) {
	for in, want := range map[string]string{
		`{"messages":[]}`:                             `[{"text":"Be terse.","type":"text"}]`,
		`{"system":"Mine.","messages":[]}`:            `[{"text":"Be terse.","type":"text"},{"text":"Mine.","type":"text"}]`,
		`{"system":[{"type":"text","text":"Mine."}]}`: `[{"text":"Be terse.","type":"text"},{"type":"text","text":"Mine."}]`,
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(in), &body)
		if err := withSystem(body, chatShapeAnthropic, "Be terse."); err != nil || string(body["system"]) != want {
			t.Errorf("%s: system = %s (err %v), want %s", in, body["system"], err, want)
		}
	}
}

// B43.23 (CodeQL go/reflected-xss #53, #54): an answer relayed to the chat, as it is or frame by frame, is never served
// as a page, even one that echoes the request as markup.
func TestListingWriter_NeverServesHTML(t *testing.T) {
	guard := market.NewPromptGuard("Be terse.")
	for _, tc := range []struct {
		name, setType, want string
		guard               *market.PromptGuard
		body                string
	}{
		{"as it is, sniffed", "", "text/plain; charset=utf-8", nil, "<html><script>alert(1)</script></html>"},
		{"as it is, html", "text/html", "text/plain; charset=utf-8", nil, "<script>alert(1)</script>"},
		{"as it is, json", "application/json", "application/json", nil, `{"error":"<script>"}`},
		{"a stream, html", "text/html; charset=utf-8", "text/plain; charset=utf-8", &guard, "data: <script>alert(1)</script>\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := httptest.NewRecorder()
			w := &listingWriter{ResponseWriter: out, guard: tc.guard}
			if tc.setType != "" {
				w.Header().Set("Content-Type", tc.setType)
			}
			w.WriteHeader(http.StatusOK)
			w.stream = tc.guard != nil // relayed frame by frame, whatever type the proxy named
			if _, err := w.Write([]byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			if got := out.Header().Get("Content-Type"); got != tc.want {
				t.Fatalf("Content-Type = %q, want %q", got, tc.want)
			}
			if got := out.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if out.Body.String() != tc.body {
				t.Fatalf("body = %q, want it relayed as it is: %q", out.Body.String(), tc.body)
			}
		})
	}
}
