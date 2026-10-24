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

func TestReplayAsSSE_OpenAI_ClientReceivesChunks(t *testing.T) {
	cached := []byte(`{"choices":[{"message":{"role":"assistant","content":"Hello, world!"}}]}`)
	w := newFlushRecorder()

	if err := replayAsSSE(w, "openai", cached); err != nil {
		t.Fatalf("replayAsSSE: %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Hello, world!") {
		t.Errorf("body missing accumulated content: %q", body)
	}
	if strings.Count(body, "data: ") < 2 {
		t.Errorf("expected at least 2 data: frames, got %q", body)
	}
	if w.flushCount < 2 {
		t.Errorf("expected ≥ 2 flushes, got %d", w.flushCount)
	}
}

func TestReplayAsSSE_OpenAI_DoneFrameSentLast(t *testing.T) {
	cached := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	w := newFlushRecorder()

	if err := replayAsSSE(w, "openai", cached); err != nil {
		t.Fatalf("replayAsSSE: %v", err)
	}

	body := strings.TrimRight(w.Body.String(), "\n")
	if !strings.HasSuffix(body, "data: [DONE]") {
		t.Errorf("body should end with [DONE] frame; got:\n%s", body)
	}
}

func TestReplayAsSSE_OpenAI_HeaderSet(t *testing.T) {
	cached := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	w := newFlushRecorder()

	if err := replayAsSSE(w, "openai", cached); err != nil {
		t.Fatalf("replayAsSSE: %v", err)
	}

	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
}

func TestReplayAsSSE_Anthropic_MessageStopEventSent(t *testing.T) {
	cached := []byte(`{"content":[{"type":"text","text":"hi"}]}`)
	w := newFlushRecorder()

	if err := replayAsSSE(w, "anthropic", cached); err != nil {
		t.Fatalf("replayAsSSE: %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("missing message_stop event in body:\n%s", body)
	}
	trimmed := strings.TrimRight(body, "\n")
	if !strings.HasSuffix(trimmed, `data: {"type":"message_stop"}`) {
		t.Errorf("body should end with message_stop data frame; tail = %q", trimmed[len(trimmed)-80:])
	}
}

func TestReplayAsSSE_Anthropic_ContentBlockDeltaContainsText(t *testing.T) {
	cached := []byte(`{"content":[{"type":"text","text":"Hello, world!"}]}`)
	w := newFlushRecorder()

	if err := replayAsSSE(w, "anthropic", cached); err != nil {
		t.Fatalf("replayAsSSE: %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, "event: content_block_delta") {
		t.Errorf("missing content_block_delta event in body:\n%s", body)
	}
	if !strings.Contains(body, "Hello, world!") {
		t.Errorf("delta did not carry the cached text:\n%s", body)
	}
}

func TestReplayAsSSE_InvalidJSON_ReturnsErrorAndLeavesWUntouched(t *testing.T) {
	w := newFlushRecorder()
	err := replayAsSSE(w, "openai", []byte("{not valid json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if w.Body.Len() != 0 {
		t.Errorf("on parse failure, w should be untouched; got body=%q", w.Body.String())
	}
	// No headers should have been committed either.
	if w.Header().Get("X-Talyvor-Cache-Replay") != "" {
		t.Errorf("X-Talyvor-Cache-Replay should not be set when replay fails")
	}
}

func TestServe_StreamTrueCacheHit_UsesSSEReplay(t *testing.T) {
	exact, _ := newExactCacheForTest(t)
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	cached := []byte(`{"choices":[{"message":{"role":"assistant","content":"cached hello"}}]}`)
	if err := exact.Set(context.Background(), "openai", "gpt-4", cache.FingerprintedKey("hi", plainFP), cached); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := newFlushRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream (raw JSON was returned instead of SSE replay)", got)
	}
	if w.Header().Get("X-Talyvor-Cache-Replay") != "true" {
		t.Error("X-Talyvor-Cache-Replay header missing on stream-cache-hit replay")
	}
	out := w.Body.String()
	if !strings.Contains(out, "cached hello") {
		t.Errorf("body missing cached content: %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("body missing [DONE] terminal frame: %q", out)
	}
}

// B17.122 — a buffered cache hit is marked a replay too: the pool-isolation testers read a free repeat with no mark as
// asked afresh.
func TestServe_BufferedCacheHit_IsMarkedReplay(t *testing.T) {
	exact, _ := newExactCacheForTest(t)
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	cached := []byte(`{"choices":[{"message":{"role":"assistant","content":"cached hello"}}]}`)
	if err := exact.Set(context.Background(), "openai", "gpt-4", cache.FingerprintedKey("hi", plainFP), cached); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "cached hello") {
		t.Fatalf("status = %d, body=%s; want the cached answer", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Talyvor-Cache-Replay") != "true" {
		t.Error("X-Talyvor-Cache-Replay header missing on a buffered cache hit")
	}
}
