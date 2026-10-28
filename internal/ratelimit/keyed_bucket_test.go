package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

type wsRules map[string]RateRule

func (m wsRules) WorkspaceRule(_ context.Context, wsID string) (RateRule, bool) {
	r, ok := m[wsID]
	return r, ok
}

// B50.17: a key holder is bucketed, and given its limit, by its own key's workspace; changing
// X-Talyvor-Workspace neither opens a fresh bucket nor borrows another workspace's limit.
func TestKeyHolderIsBucketedByItsKeysWorkspaceNotByHeader(t *testing.T) {
	l, _ := setupLimiter(t, []RateRule{{RequestsPerMinute: 3}})
	l.AttachWorkspaceLimits(wsRules{"ws-generous": {RequestsPerMinute: 1000}})
	h := RateLimitMiddleware(l)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	key := &auth.APIKey{ID: "key-1", WorkspaceID: "ws-own"}
	call := func(ws string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("X-Talyvor-Workspace", ws)
		req = req.WithContext(auth.WithAPIKey(req.Context(), key))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	for i := range 3 {
		if code := call("ws-" + strconv.Itoa(i)); code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, code)
		}
	}
	if code := call("ws-new"); code != http.StatusTooManyRequests {
		t.Fatalf("a fourth request under a new workspace header = %d, want 429", code)
	}
	if code := call("ws-generous"); code != http.StatusTooManyRequests {
		t.Fatalf("naming a workspace with a generous limit = %d, want 429", code)
	}
}
