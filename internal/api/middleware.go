package api

// middleware.go — cross-cutting HTTP middleware that the chi
// router installs once. Each middleware here is small, opt-in
// at registration time, and has no dependency on the existing
// Server struct so cmd/lens/main.go can wire them up wherever
// it likes.
//
// Pieces:
//   - RequestIDMiddleware   — X-Request-ID propagation
//   - APIVersionMiddleware  — X-API-Version on every response
//   - GzipMiddleware        — opt-in gzip for large JSON bodies
//   - RateLimitHeaders      — emits X-RateLimit-* on responses
//   - SecurityHeadersMiddleware — X-Content-Type-Options, X-Frame-Options,
//     Referrer-Policy, Content-Security-Policy, Strict-Transport-Security on every response
//   - CORSMiddleware        — allowlist-based CORS, off by default
//   - APIError / WriteError — standardised error envelope

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ─── constants ───────────────────────────────────

// APIVersion is the version string emitted on every response.
// Bumped together with breaking API changes.
const APIVersion = "1.0.0"

// GzipMinBytes is the threshold below which gzip would just
// add overhead. 1KB matches the spec.
const GzipMinBytes = 1024

// Standard error codes — SCREAMING_SNAKE_CASE per spec.
const (
	ErrCodeUnauthorized        = "UNAUTHORIZED"
	ErrCodeForbidden           = "FORBIDDEN"
	ErrCodeNotFound            = "NOT_FOUND"
	ErrCodeRateLimited         = "RATE_LIMITED"
	ErrCodeSpendCapExceeded    = "SPEND_CAP_EXCEEDED"
	ErrCodeInvalidRequest      = "INVALID_REQUEST"
	ErrCodeInternalError       = "INTERNAL_ERROR"
	ErrCodeModelNotAllowed     = "MODEL_NOT_ALLOWED"
	ErrCodeProviderUnavailable = "PROVIDER_UNAVAILABLE"
)

// ─── request ID middleware ───────────────────────

// requestIDKey is the context key under which the chosen
// request ID is stored. Loggers can pluck it back out via
// RequestIDFromContext.
type ctxKey string

const requestIDKey ctxKey = "talyvor.request_id"

// requestIDMaxLen is the maximum length (in bytes) we accept from a
// client-supplied X-Request-ID. Any excess is silently truncated.
// 64 chars is generous for distributed tracing IDs (UUID = 36 chars)
// while preventing a client from storing an arbitrarily large string
// in the DB via PoVI receipt RequestID (ISO 27001 A.12).
const requestIDMaxLen = 64

// sanitizeRequestID keeps only alphanumeric characters, hyphens, and
// underscores, then caps the result at requestIDMaxLen. These are the
// characters used by UUID, ULID, and common trace-ID formats; anything
// else (newlines, colons, spaces …) would be a footgun if the ID ever
// reaches a structured log or a downstream service that parses headers.
// Returns "" when the sanitised value is empty so the caller can fall
// back to a minted UUID.
func sanitizeRequestID(raw string) string {
	var b strings.Builder
	for _, c := range raw {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		}
		if b.Len() >= requestIDMaxLen {
			break
		}
	}
	return b.String()
}

// RequestIDMiddleware sets X-Request-ID. If the client already
// supplied one it is sanitised (alphanumeric + hyphens + underscores,
// max 64 chars) and honoured so distributed tracing works. A value
// that is empty after sanitisation, or absent entirely, causes a fresh
// UUID to be minted.
type RequestIDMiddleware struct{}

func (m *RequestIDMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the ID stamped onto this request
// by RequestIDMiddleware. Empty string when no middleware ran
// (handy for unit-testing helpers in isolation).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// ─── API-version middleware ──────────────────────

// APIVersionMiddleware tags every response with the current
// X-API-Version header. Clients can use it to detect upgrades.
func APIVersionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-API-Version", APIVersion)
		next.ServeHTTP(w, r)
	})
}

// ─── gzip middleware ─────────────────────────────

// gzipResponseWriter buffers the response so we can decide
// post-hoc whether to compress. Small bodies fall through
// untouched, large bodies get gzipped before flush.
type gzipResponseWriter struct {
	http.ResponseWriter
	buf         bytes.Buffer
	status      int
	wroteStatus bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	g.status = status
	g.wroteStatus = true
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.buf.Write(b)
}

// flush is called by the middleware once the handler returns —
// it decides between passthrough and gzip based on size + the
// Accept-Encoding negotiation captured at entry.
func (g *gzipResponseWriter) flush(allowGzip bool) {
	if !g.wroteStatus {
		g.status = http.StatusOK
	}
	body := g.buf.Bytes()
	if allowGzip && len(body) >= GzipMinBytes {
		g.ResponseWriter.Header().Set("Content-Encoding", "gzip")
		g.ResponseWriter.Header().Del("Content-Length")
		g.ResponseWriter.WriteHeader(g.status)
		gw := gzip.NewWriter(g.ResponseWriter)
		_, _ = gw.Write(body)
		_ = gw.Close()
		return
	}
	g.ResponseWriter.WriteHeader(g.status)
	_, _ = g.ResponseWriter.Write(body)
}

// GzipMiddleware compresses JSON-ish responses larger than
// 1KB when the client says it accepts gzip. We deliberately
// skip compression on websocket / SSE / already-encoded
// streams — those handlers write to w directly and the
// buffered wrapper short-circuits via the Hijacker path.
func GzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
		// SSE / event-stream MUST stream — never buffer.
		if strings.HasPrefix(r.URL.Path, "/v1/proxy/") ||
			strings.HasPrefix(r.URL.Path, "/oai/") ||
			strings.HasPrefix(r.URL.Path, "/anthropic/") ||
			strings.HasPrefix(r.URL.Path, "/mcp") ||
			(strings.HasPrefix(r.URL.Path, "/v1/rooms/") && strings.HasSuffix(r.URL.Path, "/events")) { // B32.30
			next.ServeHTTP(w, r)
			return
		}
		gz := &gzipResponseWriter{ResponseWriter: w}
		next.ServeHTTP(gz, r)
		gz.flush(acceptsGzip)
	})
}

// ─── rate-limit-headers middleware ──────────────

// RateLimitInfo is what the existing rate limiter stashes onto
// the request context. We don't ship a duplicate limiter here —
// just a header writer that picks the values up if they're
// present and emits the standard X-RateLimit-* triple.
type RateLimitInfo struct {
	Limit     int   // requests/window
	Remaining int   // requests left in current window
	Reset     int64 // unix seconds when the window resets
}

// rateLimitCtxKey lives in the same scope as requestIDKey so
// the limiter and the header middleware can hand state across
// without exporting a separate package.
const rateLimitCtxKey ctxKey = "talyvor.rate_limit_info"

// WithRateLimit stamps RateLimitInfo onto the context. The
// existing limiter can call this from its own middleware so
// we don't have to refactor ratelimit/.
func WithRateLimit(ctx context.Context, info RateLimitInfo) context.Context {
	return context.WithValue(ctx, rateLimitCtxKey, info)
}

// RateLimitHeadersMiddleware reads the info that an upstream
// limiter has stamped and emits the standard triple. Silent
// no-op when no limiter ran for this route.
func RateLimitHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &headerCaptureWriter{ResponseWriter: w, req: r}
		next.ServeHTTP(rw, r)
	})
}

// headerCaptureWriter delays the WriteHeader call until we've
// had a chance to look up the rate-limit info that the handler
// (or a deeper middleware) might have stashed. Once the handler
// returns we copy the values onto the response.
type headerCaptureWriter struct {
	http.ResponseWriter
	req     *http.Request
	written bool
}

func (h *headerCaptureWriter) WriteHeader(status int) {
	if !h.written {
		if info, ok := h.req.Context().Value(rateLimitCtxKey).(RateLimitInfo); ok {
			if info.Limit > 0 {
				h.ResponseWriter.Header().Set("X-RateLimit-Limit", itoa(info.Limit))
			}
			if info.Limit > 0 || info.Remaining > 0 {
				h.ResponseWriter.Header().Set("X-RateLimit-Remaining", itoa(info.Remaining))
			}
			if info.Reset > 0 {
				h.ResponseWriter.Header().Set("X-RateLimit-Reset", i64toa(info.Reset))
			}
		}
		h.written = true
	}
	h.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the writer beneath, so a stream it carries can be flushed (B32.30).
func (h *headerCaptureWriter) Unwrap() http.ResponseWriter { return h.ResponseWriter }

func (h *headerCaptureWriter) Write(b []byte) (int, error) {
	if !h.written {
		h.WriteHeader(http.StatusOK)
	}
	return h.ResponseWriter.Write(b)
}

// itoa / i64toa avoid pulling fmt for trivial numeric formats.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func i64toa(n int64) string { return itoa(int(n)) }

// ─── security headers middleware ─────────────────

// SecurityHeadersMiddleware sets defensive HTTP headers on every response.
//
//   - X-Content-Type-Options: nosniff — stops browsers from MIME-sniffing a
//     response away from the declared Content-Type.
//
//   - X-Frame-Options: DENY — legacy clickjacking protection for browsers that
//     don't support CSP frame-ancestors.
//
//   - Referrer-Policy: strict-origin-when-cross-origin — sends the full URL as
//     referrer for same-origin requests (useful for internal analytics) but
//     strips the path/query on cross-origin navigation.
//
//   - Content-Security-Policy — restrict what the browser may load.
//     The policy is tuned for the Lens dashboard, whose only external resources
//     are the IBM Plex fonts served by Google Fonts.  All JavaScript and CSS
//     are inline (no external scripts; nonces are out of scope for now, so
//     'unsafe-inline' is needed for those two directives).
//
//   - Strict-Transport-Security: max-age=31536000 — a browser that reached Lens
//     over HTTPS (Caddy in front) keeps to HTTPS for a year. Neither
//     includeSubDomains nor preload (B37.3); the self-TLS server's own
//     hstsMiddleware value is kept where it already set one.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
		"font-src 'self' https://fonts.gstatic.com; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", csp)
		if w.Header().Get("Strict-Transport-Security") == "" {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ─── CORS middleware ──────────────────────────────

// CORSMiddleware sets Access-Control-* headers based on an explicit origin
// allowlist. Designed for API-gateway deployments where browser clients may
// call Lens directly from a different origin.
//
// allowedOrigins is a comma-separated list of exact origin strings, e.g.
// "https://app.example.com,https://admin.example.com". Use "*" to allow any
// origin (public-API mode). An empty string disables CORS entirely — no
// Access-Control-Allow-Origin header is sent and browsers enforce the default
// same-origin policy, which is the safest default.
//
// Preflight OPTIONS requests are answered with 204 No Content so the browser
// does not block them on auth middleware that runs later in the chain.
//
// The Vary: Origin header is always added when an origin matches, so that
// CDNs and caches do not serve one origin's CORS-annotated response to a
// request from a different origin.
func CORSMiddleware(allowedOrigins string) func(http.Handler) http.Handler {
	if allowedOrigins == "" {
		// Fast path: no CORS configured — return identity middleware.
		return func(next http.Handler) http.Handler { return next }
	}

	wildcard := allowedOrigins == "*"
	allowed := make(map[string]struct{})
	if !wildcard {
		for _, o := range strings.Split(allowedOrigins, ",") {
			if o = strings.TrimSpace(o); o != "" {
				allowed[o] = struct{}{}
			}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			_, originAllowed := allowed[origin]
			if origin != "" && (wildcard || originAllowed) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers",
					"Authorization, Content-Type, X-Request-ID, X-Talyvor-Key")
				// Expose the response headers that browser SDK clients need to
				// read: request tracing ID, rate-limit state, and API version.
				// Without this, browsers hide all non-safelisted headers from
				// cross-origin JS even when Access-Control-Allow-Origin is set.
				w.Header().Set("Access-Control-Expose-Headers",
					"X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, X-API-Version")
				w.Header().Set("Access-Control-Max-Age", "86400")
				// Vary: Origin tells caches the response differs per origin.
				w.Header().Add("Vary", "Origin")
			}
			// Absorb preflight requests before they reach auth middleware.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ─── error envelope ──────────────────────────────

// APIError is the standardised error shape every handler should
// emit via WriteError / WriteErrorDetails.
type APIError struct {
	Code      string      `json:"code"`
	Message   string      `json:"message"`
	Details   interface{} `json:"details,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
}

// WriteError writes the canonical error envelope with no details.
func WriteError(w http.ResponseWriter, code, message string, status int, requestID string) {
	WriteErrorDetails(w, code, message, nil, status, requestID)
}

// WriteErrorDetails is the full-fat version when there's
// structured context worth surfacing (validation errors etc.).
func WriteErrorDetails(w http.ResponseWriter, code, message string, details interface{}, status int, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{
		Code:      code,
		Message:   message,
		Details:   details,
		RequestID: requestID,
	})
}

// Drain is the small helper used by middleware tests to read
// a response body without leaking the reader.
func Drain(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}

// MaxRequestBody is the most any request may send: the largest upload Lens takes, speech audio (25 MiB) and
// its form (1 MiB). Routes that expect less cap their own body lower.
const MaxRequestBody = 26 << 20

// BodyLimitMiddleware refuses a body larger than limit: 413 at once when Content-Length says so, and a read
// error past limit bytes when it does not (chunked), so no handler can be made to buffer more.
func BodyLimitMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte(`{"error":"request body too large"}`))
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
