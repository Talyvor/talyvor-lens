package ratelimit

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/metrics"
)

// RateLimitMiddleware fronts a Limiter on the request path. The wsID and
// keyID come from the validated APIKey on the context, never from a header
// the caller can change (B50.17). A request
// with no key (public and webhook routes) is bucketed by its client IP and
// never by the header, which only the caller set there.
//
// On rejection: 429 with Retry-After + X-RateLimit-Remaining headers and
// a structured JSON body. On acceptance: X-RateLimit-Remaining is set so
// clients see their budget.
func RateLimitMiddleware(l *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wsID, keyID := "", "ip:"+anonymousIP(r)
			if k := auth.GetAPIKey(r.Context()); k != nil {
				wsID, keyID = k.WorkspaceID, k.ID
			}
			result := l.Check(r.Context(), wsID, keyID)
			if !result.Allowed {
				metrics.RateLimitRejected(wsID)
				w.Header().Set("Retry-After", strconv.Itoa(result.RetryAfterSecs))
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":               "rate limit exceeded",
					"limit_type":          result.LimitType,
					"retry_after_seconds": result.RetryAfterSecs,
				})
				return
			}
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))
			next.ServeHTTP(w, r)
		})
	}
}

// anonymousIP is the address a keyless caller is bucketed by. The forwarded client IP is believed only when
// the TCP peer is our own proxy (a private or loopback address); a caller that reaches Lens directly is its
// TCP peer, whatever X-Forwarded-For it sends, and an unresolved forwarded IP falls back to the peer too.
func anonymousIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if ip := net.ParseIP(peer); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		if fwd := middleware.GetClientIP(r.Context()); fwd != "" {
			return fwd
		}
	}
	return peer
}
