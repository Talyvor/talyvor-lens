package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// B28.95 — a replay says what it saved, X-Talyvor-Cache-Saved-ULXC: the answer's price had it been asked live, on
// both paths. Chat's "Saved in this chat" totals it (talyvor-suite B28.358).
func TestCacheReplay_SaysWhatItSaved_BothPaths(t *testing.T) {
	usd, _ := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, 1000, 0, 0, 500)
	want := strconv.FormatInt(economy.ChargeULXC(usd), 10)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			exact, _ := newExactCacheForTest(t)
			p := New(
				exact, nil, nil,
				compressor.New(), router.New(), pii.New(),
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
				"openai-key", "anthropic-key", "",
			)
			cached := []byte(`{"choices":[{"message":{"role":"assistant","content":"cached hello"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
			if err := exact.Set(context.Background(), "openai", "gpt-4o", cache.FingerprintedKey("hi", plainFP), cached); err != nil {
				t.Fatalf("seed cache: %v", err)
			}
			body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":%v}`, stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := newFlushRecorder()

			p.HandleOpenAI(w, req)

			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "cached hello") {
				t.Fatalf("status = %d, body=%s; want the cached answer", w.Code, w.Body.String())
			}
			if w.Header().Get("X-Talyvor-Cache-Replay") != "true" {
				t.Fatal("X-Talyvor-Cache-Replay missing on a cache hit")
			}
			if got := w.Header().Get("X-Talyvor-Cache-Saved-ULXC"); got != want || want == "0" {
				t.Errorf("X-Talyvor-Cache-Saved-ULXC = %q, want %s µLXC (gpt-4o, 1000 in, 500 out)", got, want)
			}
		})
	}
}

// B28.95 — a trimmed request says what Tare saved, X-Talyvor-Tare-Tokens-Saved: the tokens it took off, as metered on the
// spend row, on both paths.
func TestTare_SaysTheTokensItSaved_BothPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			p, _, sink := newTareProxy(t, workspace.TareOptIn)
			w := dispatchTare(t, p, stream, map[string]string{"X-Talyvor-Tare": "true"})
			if len(sink.spends) != 1 {
				t.Fatalf("%d spend rows, want 1", len(sink.spends))
			}
			m := sink.spends[0].tare
			want := strconv.Itoa(m.TokensIn - m.TokensOut)
			if got := w.Header().Get("X-Talyvor-Tare-Tokens-Saved"); got != want || m.TokensIn <= m.TokensOut {
				t.Errorf("X-Talyvor-Tare-Tokens-Saved = %q, want %s (spend row: %d in, %d out)", got, want, m.TokensIn, m.TokensOut)
			}
		})
	}
}
