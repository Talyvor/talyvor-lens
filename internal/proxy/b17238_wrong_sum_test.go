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

// B17.238 — the testers' agent-model-limit asked "What is 5312 + 8686?" on three nights, in three new workspaces,
// and was served "14998" from the pool each time. A wrong sum is never stored, and one already stored is never
// served — the request goes to the provider instead, and its right answer replaces it.
func TestServe_AWrongSumIsNeitherStoredNorServed(t *testing.T) {
	const question = "What is 5312 + 8686? Reply with the number only."
	const wrong = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"14998"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":3}}`
	const good = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"13998"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":3}}`

	hits := 0
	answer := wrong
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(upstream.Close)

	exact, _ := newExactCacheForTest(t)
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	pointEveryProviderAt(p, upstream.URL)

	body := `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"` + question + `"}]}`
	ask := func() string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	key := cache.FingerprintedKey(question, cache.RequestFingerprint([]byte(body)))
	ask()
	if stored, _ := exact.Get(context.Background(), "anthropic", "claude-sonnet-5", key); stored != nil {
		t.Fatalf("the wrong sum was stored: %s", stored)
	}
	answer = good
	if got := ask(); hits != 2 || !strings.Contains(got, "13998") {
		t.Fatalf("after a wrong sum the same question reached the provider %d time(s) and got %s; want 2 and 13998", hits, got)
	}
	if ask(); hits != 2 {
		t.Fatalf("the right sum was not stored: the provider was asked %d times, want 2", hits)
	}

	// A wrong sum stored before this fix is not served.
	if err := exact.Set(context.Background(), "anthropic", "claude-sonnet-5", key, []byte(wrong)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := ask(); hits != 3 || !strings.Contains(got, "13998") {
		t.Fatalf("a stored wrong sum: the provider was asked %d times and the asker got %s; want 3 and 13998", hits, got)
	}
}

// Only a lone number against the one sum a question asks is judged.
func TestWrongSum_JudgesOnlyALoneNumberAgainstOneSum(t *testing.T) {
	oa := func(text string) []byte { return []byte(`{"choices":[{"message":{"content":"` + text + `"}}]}`) }
	for _, c := range []struct {
		q, a string
		want bool
	}{
		{"What is 5312 + 8686? Reply with the number only.", "14998", true},
		{"What is 5312 + 8686? Reply with the number only.", "13998", false},
		{"What is 5312 + 8686?", "13,998.", false},
		{"What is 40 - 3?", "36", true},
		{"What is 12 × 7?", "84", false},
		{"What is 5312 + 8686?", "It is 14998", false},
		{"In 2026, what is 5 + 3?", "9", false},
		{"Name a prime.", "7", false},
	} {
		if got := wrongSum(c.q, oa(c.a)); got != c.want {
			t.Errorf("wrongSum(%q, %q) = %v, want %v", c.q, c.a, got, c.want)
		}
	}
}
