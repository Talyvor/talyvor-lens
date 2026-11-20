package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// B28.250 — a provider that echoes the key it refused hands it to the tenant through Lens, buffered and streamed.
func TestB28250ProviderUnauthorizedNeverShowsTheKey(t *testing.T) {
	const key = "zq-operator-key-0123456789abcdef" // no known key shape: only the sent credential can cut it
	global := false
	p, _, _, _, _ := newPoolingProxy(t, &global)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided: `+
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+` for `+r.Host+`"}}`)
	}))
	defer srv.Close()
	p.openAIURL, p.openAIKey = srv.URL, key

	for _, stream := range []bool{false, true} {
		body := `{"model":"gpt-4o","stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "wsA")
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("stream=%v: status %d, want the provider's 401: %s", stream, w.Code, w.Body.String())
		}
		got := w.Body.String()
		if strings.Contains(got, key) || strings.Contains(got, strings.TrimPrefix(srv.URL, "http://")) {
			t.Fatalf("stream=%v: the tenant is shown the provider key or address: %s", stream, got)
		}
		if !strings.Contains(got, "Incorrect API key provided") {
			t.Fatalf("stream=%v: the provider's reason is lost: %s", stream, got)
		}
	}
}

// B28.250 — an answer another workspace contributed comes back no faster than a live call: the clock no longer tells
// a tenant that someone else asked the same question.
func TestB28250PooledAnswerTakesAsLongAsALiveCall(t *testing.T) {
	global := true
	p, wsm, _, _, _ := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okResp)
	}))
	defer srv.Close()
	p.openAIURL = srv.URL

	timed := func(ws, q string) time.Duration {
		start := time.Now()
		dispatchWS(t, p, ws, q)
		return time.Since(start)
	}
	var live, pooled []time.Duration
	for i := 0; i < 8; i++ {
		q := "what is " + strconv.Itoa(i) + " plus " + strconv.Itoa(i)
		live = append(live, timed("wsA", q))
		before := atomic.LoadInt64(&calls)
		pooled = append(pooled, timed("wsB", q))
		if atomic.LoadInt64(&calls) != before {
			t.Fatalf("%q: wsB went upstream, so this measured no pooled answer", q)
		}
	}
	median := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	ml, mp := median(live), median(pooled)
	if mp < ml*8/10 || mp > ml*3/2 {
		t.Fatalf("pooled answers take %v and live ones %v: a tenant can tell them apart by the clock", mp, ml)
	}
}

// B28.250 — a workspace looking in the pool more often than the limit allows is probing it, and its pooled answers
// stop carrying the saving headers.
func TestB28250ProbingWorkspaceIsNotToldItWasPooled(t *testing.T) {
	global := true
	p, wsm, _, _, _ := newPoolingProxy(t, &global)
	_ = wsm.SetCachePoolable(context.Background(), "wsA", true)
	_ = wsm.SetCachePoolable(context.Background(), "wsB", true)
	p.SetPoolProbeLimit(1)
	for _, q := range []string{"name a prime", "name a colour"} {
		dispatchWS(t, p, "wsA", q)
	}
	if h := dispatchWS(t, p, "wsB", "name a prime").Header().Get("X-Talyvor-Pool-Saved-ULXC"); h == "" {
		t.Fatal("the first pooled answer, within the limit, should say what it saved")
	}
	if h := dispatchWS(t, p, "wsB", "name a colour").Header().Get("X-Talyvor-Pool-Saved-ULXC"); h != "" {
		t.Fatalf("a workspace over the probe limit is still told its answer was pooled: saved=%s", h)
	}
}
