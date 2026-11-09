package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

// B28.79: a slow model's answer streams to its end. Lens serves behind Caddy on plain HTTP, whose server cuts
// every connection 30 s in (WriteTimeout, and ReadTimeout cancels the request), and its upstream client gives up
// at 120 s; a 90 s answer ended blank. Here the server's timeouts and the client cap are scaled down to 300 ms
// and 500 ms, and the provider takes about 1.2 s to finish, so the answer outlives both.
func TestB2879_SlowStreamOutlivesServerAndClientTimeouts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, word := range []string{"one ", "two ", "three "} {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+word+`"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, compressor.New(), router.New(), pii.New(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())), "openai-key", "anthropic-key", "")
	p.openAIURL = upstream.URL
	p.httpClient.Timeout = 500 * time.Millisecond

	lens := httptest.NewUnstartedServer(http.HandlerFunc(p.HandleOpenAI))
	lens.Config.ReadTimeout = 300 * time.Millisecond
	lens.Config.WriteTimeout = 300 * time.Millisecond
	lens.Start()
	t.Cleanup(lens.Close)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(lens.URL+"/v1/proxy/openai/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream cut: %v; got %q", err, got)
	}
	for _, want := range []string{"one ", "two ", "three ", "[DONE]"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("stream ended without %q; got %q", want, got)
		}
	}
}
