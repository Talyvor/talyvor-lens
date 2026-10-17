package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/pii"
)

// B15.2 — THE CHAT'S ANSWERS ARE STORED, SO A REPEATED QUESTION IS ANSWERED FROM THE CACHE.
//
// Each test sends the browser chat's own request — a session key, and the body the suite builds
// (apps/web/src/areas/chat/chatApi.ts: model, max_tokens 4096, stream, one user message) — through
// the real handler to an Anthropic-shaped SSE upstream that counts its calls.

const chatQuestion = `{"model":"claude-haiku-4-5","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"what is the capital of the UK?"}]}`

func chatSSE(answer string, end bool) string {
	s := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":16,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + jsonString(answer) + "}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n"
	if end {
		s += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return s
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// chatCacheProxy is the B9.3 harness — real PG, pooling on, wsPoolA and wsPoolB opted in and funded,
// the production minter armed — pointed at an SSE upstream that streams `stream`.
func chatCacheProxy(t *testing.T, stream string) (*Proxy, *pgxpool.Pool, *int64) {
	t.Helper()
	p, pool, _ := anthropicPoolProxy(t)
	armProductionMinter(t, p, pool)
	store := economy.NewDualTokenStore(nil, pool, nil)
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL
	return p, pool, &calls
}

func chatAsk(t *testing.T, p *Proxy, ws string, header ...string) *flushRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(chatQuestion))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", ws)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
	w := newFlushRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
	}
	return w
}

func TestChatCache_TheSameQuestionInANewConversationIsAnsweredFromTheCache(t *testing.T) {
	p, _, calls := chatCacheProxy(t, chatSSE("The capital of the UK is London.", true))
	chatAsk(t, p, "wsPoolA")
	w := chatAsk(t, p, "wsPoolA")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("the model was asked %d times, want 1 — the repeat was not served from the cache", got)
	}
	if w.Header().Get("X-Talyvor-Cache-Replay") != "true" || !strings.Contains(w.Body.String(), "London") {
		t.Errorf("the repeat was not the cached answer replayed as a stream: replay=%q body=%.200q",
			w.Header().Get("X-Talyvor-Cache-Replay"), w.Body.String())
	}
}

// The same question from a second workspace is a pooled serve, billed exactly as B9.3 bills a chat
// pooled answer: list × 0.70 on a "chat: pooled answer" row, half of it held for the contributor.
func TestChatCache_TheSameQuestionFromAnotherWorkspaceIsAPooledServeChargedAsB93(t *testing.T) {
	p, pool, calls := chatCacheProxy(t, chatSSE("The capital of the UK is London.", true))
	chatAsk(t, p, "wsPoolA")
	chatAsk(t, p, "wsPoolB")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("the model was asked %d times, want 1 — wsPoolB was not served from the pool", got)
	}
	var charged int64
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT -amount, COALESCE(metadata,'{}'::jsonb) FROM lxc_ledger
		  WHERE workspace_id = 'wsPoolB' AND amount < 0 AND description = 'chat: pooled answer'`).Scan(&charged, &raw); err != nil {
		t.Fatalf("want exactly one 'chat: pooled answer' charge for wsPoolB: %v", err)
	}
	var meta map[string]any
	_ = json.Unmarshal(raw, &meta)
	list, _ := meta["pool_list_ulxc"].(float64)
	saved, _ := meta["pool_saved_ulxc"].(float64)
	if meta["pool_discount_rate"] != 0.3 || saved <= 0 || float64(charged)+saved != list {
		t.Errorf("charge %d µLXC %v; want list × 0.70 with charged + saved = list at rate 0.3", charged, meta)
	}
	var held int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(amount),0) FROM lens_token_ledger WHERE workspace_id = 'wsPoolA' AND type = $1`,
		mining.TypePoolRoyaltyHeld).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != charged/2 {
		t.Errorf("contributor held %d µLENS, want %d (half the %d µLXC charge)", held, charged/2, charged)
	}
}

func TestChatCache_ACancelledStreamIsNeverStored(t *testing.T) {
	p, _, _ := anthropicPoolProxy(t)
	sent, release := make(chan struct{}), make(chan struct{})
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&calls, 1) > 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE("The capital of the UK is London.", true))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"The capital\"}}\n\n")
		w.(http.Flusher).Flush()
		close(sent)
		select { // the rest of the answer never comes before the reader hangs up
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	p.anthropicURL = up.URL

	ctx, cancel := context.WithCancel(auth.WithAuthContext(context.Background(),
		&auth.AuthContext{WorkspaceID: "wsPoolA", AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(chatQuestion)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "wsPoolA")
	done := make(chan struct{})
	go func() { p.HandleAnthropic(newFlushRecorder(), req); close(done) }()
	<-sent
	cancel() // the user pressed Stop
	<-done

	chatAsk(t, p, "wsPoolA")
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("the model was asked %d times, want 2 — a cancelled stream's partial answer was served from the cache", got)
	}
}

func TestChatCache_AStreamCutOffBeforeItsEndIsNeverStored(t *testing.T) {
	p, _, calls := chatCacheProxy(t, chatSSE("The capital of the UK is", false)) // no message_stop: the connection dropped
	chatAsk(t, p, "wsPoolA")
	chatAsk(t, p, "wsPoolA")
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("the model was asked %d times, want 2 — a cut-off answer was served from the cache", got)
	}
}

func TestChatCache_AnAnswerFailingTheOutputGuardrailsIsNeverStored(t *testing.T) {
	p, _, calls := chatCacheProxy(t, chatSSE("Write to jane.doe@example.com for the answer.", true))
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	eng.SetOutputEnabled(true)
	_ = eng.SetPolicy(context.Background(), "wsPoolA", guardrails.GuardrailPolicy{
		BufferStreamForOutput: false, // streamed, so the check cannot run until the answer is complete
		OutputPIIAction:       guardrails.ActionBlock,
	})
	p.guardrails = eng
	chatAsk(t, p, "wsPoolA")
	chatAsk(t, p, "wsPoolA")
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("the model was asked %d times, want 2 — an answer the output guardrails block was served from the cache", got)
	}
}

// Regenerate: X-Talyvor-Cache: bypass always asks the model, even with the answer cached.
func TestChatCache_TheBypassHeaderAlwaysAsksTheModel(t *testing.T) {
	p, _, calls := chatCacheProxy(t, chatSSE("The capital of the UK is London.", true))
	chatAsk(t, p, "wsPoolA")
	w := chatAsk(t, p, "wsPoolA", CacheBypassHeader, "bypass")
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("the model was asked %d times, want 2 — the bypass header was served from the cache", got)
	}
	if got := w.Header().Get(CacheBypassHeader); got != "bypassed" {
		t.Errorf("%s response header = %q, want bypassed", CacheBypassHeader, got)
	}
}

// B17.151 — a temporary chat (X-Talyvor-Cache: bypass + X-Talyvor-Cache-Store: off) keeps nothing: its answer is
// served to no later question, in its own workspace (wsPoolA) or through the pool (wsPoolB), streamed or buffered.
// The repeat is the control: the same question asked plainly is stored and replayed, so the harness sees a stored answer.
func TestChatCache_ATemporaryChatsAnswerIsKeptNowhere(t *testing.T) {
	const answer = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"The capital of the UK is London."}],"stop_reason":"end_turn","usage":{"input_tokens":16,"output_tokens":9}}`
	for _, streamed := range []bool{true, false} {
		for _, later := range []string{"wsPoolA", "wsPoolB"} {
			t.Run(map[bool]string{true: "streamed", false: "buffered"}[streamed]+"/"+later, func(t *testing.T) {
				p, _, calls := chatCacheProxy(t, chatSSE("The capital of the UK is London.", true))
				question := chatQuestion
				if !streamed {
					question = strings.Replace(chatQuestion, `"stream":true`, `"stream":false`, 1)
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						atomic.AddInt64(calls, 1)
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, answer)
					}))
					t.Cleanup(up.Close)
					p.anthropicURL = up.URL
				}
				ask := func(ws string, header ...string) {
					req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(question))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Talyvor-Workspace", ws)
					for i := 0; i+1 < len(header); i += 2 {
						req.Header.Set(header[i], header[i+1])
					}
					req = req.WithContext(auth.WithAuthContext(req.Context(),
						&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
					w := newFlushRecorder()
					p.HandleAnthropic(w, req)
					if w.Code != http.StatusOK {
						t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
					}
				}
				ask("wsPoolA", CacheBypassHeader, "bypass", CacheStoreHeader, "off")
				ask(later)
				if got := atomic.LoadInt64(calls); got != 2 {
					t.Fatalf("the model was asked %d times, want 2 — %s was served the temporary chat's answer", got, later)
				}
				ask(later)
				if got := atomic.LoadInt64(calls); got != 2 {
					t.Fatalf("the model was asked %d times, want 2 — a plainly asked answer was not stored, so this test sees nothing", got)
				}
			})
		}
	}
}
