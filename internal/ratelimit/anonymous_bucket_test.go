package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

// B50.2: a caller with no key cannot open a fresh bucket by changing X-Talyvor-Workspace; it is
// bucketed by its client IP, and a second IP has its own bucket.
func TestAnonymousCallerIsBucketedByIPNotByWorkspaceHeader(t *testing.T) {
	l, _ := setupLimiter(t, []RateRule{{RequestsPerMinute: 3}})
	h := middleware.ClientIPFromXFF()(RateLimitMiddleware(l)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})))
	call := func(ip, ws string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", nil)
		req.RemoteAddr = "10.0.0.2:4000"
		req.Header.Set("X-Forwarded-For", ip)
		req.Header.Set("X-Talyvor-Workspace", ws)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	for i := range 3 {
		if code := call("203.0.113.7", "ws-"+strconv.Itoa(i)); code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, code)
		}
	}
	if code := call("203.0.113.7", "ws-new"); code != http.StatusTooManyRequests {
		t.Fatalf("a fourth request under a new workspace header = %d, want 429", code)
	}
	if code := call("198.51.100.9", "ws-new"); code != http.StatusOK {
		t.Fatalf("another IP = %d, want 200 (its own bucket)", code)
	}

	// A caller reaching Lens directly (no proxy in front) is its TCP peer: a forged X-Forwarded-For opens nothing.
	direct := func(xff string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", nil)
		req.RemoteAddr = "192.0.2.50:5000"
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	for i := range 3 {
		_ = direct("203.0.113." + strconv.Itoa(100+i))
	}
	if code := direct("203.0.113.200"); code != http.StatusTooManyRequests {
		t.Fatalf("a direct caller forging a fourth X-Forwarded-For = %d, want 429", code)
	}
}
