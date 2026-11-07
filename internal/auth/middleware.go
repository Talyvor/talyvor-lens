package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// apiKeyContextKey is the request-context slot for the validated APIKey.
// Use GetAPIKey to extract it in downstream handlers.
type apiKeyContextKey struct{}

// AuthMiddleware returns a chi-compatible middleware that requires every
// request to carry a valid credential.
//
// Validation order:
//  1. DB keystore (hot path for normal workspace/team keys).
//  2. Manager fallback — handles the global admin key (LENS_API_KEY) and
//     JWT bearer tokens, which are never in the DB and were silently
//     blocked before this fix.
//
// When Manager validates the credential it also stamps an AuthContext onto
// the request context (via authContextCtxKey), so downstream handlers can
// call GetAuthContext() without a second Authenticate() round-trip.
//
// The validated APIKey is attached to the request context so the rate-limiter
// (and any other GetAPIKey consumer) always sees a non-nil value.
// Workspace/team headers are overwritten with authoritative values from the
// resolved identity so handlers cannot be spoofed by client-supplied headers.
func AuthMiddleware(ks *KeyStore, m *Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractKey(r)
			if raw == "" {
				writeAuthError(w, http.StatusUnauthorized, "API key required")
				return
			}

			// ── Fast path: DB keystore ────────────────────────────────────
			// Handles normal workspace/team keys (tlv_ prefix, stored in
			// api_keys). Unchanged from the original implementation.
			result := ks.Validate(r.Context(), raw)
			if result.Valid {
				r.Header.Set("X-Talyvor-Workspace", result.APIKey.WorkspaceID)
				if result.APIKey.Team != "" {
					r.Header.Set("X-Talyvor-Team", result.APIKey.Team)
				}
				ctx := context.WithValue(r.Context(), apiKeyContextKey{}, result.APIKey)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// ── Manager fallback: global key + JWT ────────────────────────
			// The global admin key (LENS_API_KEY) and JWT bearer tokens are
			// never persisted to api_keys, so the DB lookup above always
			// misses for them. Delegate to Manager.Authenticate which knows
			// all four credential shapes.
			if m != nil {
				actx, err := m.Authenticate(r)
				if errors.Is(err, ErrAuthUnavailable) {
					writeAuthUnavailable(w)
					return
				}
				if errors.Is(err, ErrConnectRoute) {
					writeAuthError(w, http.StatusForbidden, "a Connect token may only pay from its own agent, inside the agent's rules, or accept a money request made of it")
					return
				}
				if err == nil && actx.AuthMethod == MethodModeratorKey {
					// B20.13: a moderator key reaches the marketplace review queue and nothing else.
					writeAuthError(w, http.StatusForbidden, "a moderator key may only use the marketplace review queue")
					return
				}
				if err == nil {
					// Synthesise a minimal APIKey so the rate-limiter and any
					// other GetAPIKey consumer gets a non-nil value.
					synthetic := &APIKey{
						ID:          "global",
						WorkspaceID: actx.WorkspaceID,
						Name:        actx.AuthMethod,
						Active:      true,
						CreatedAt:   time.Now().UTC(),
					}
					r.Header.Set("X-Talyvor-Workspace", actx.WorkspaceID)
					// Stamp both context slots so downstream code can use
					// either GetAPIKey or GetAuthContext without a second
					// Authenticate call.
					ctx := context.WithValue(r.Context(), apiKeyContextKey{}, synthetic)
					ctx = context.WithValue(ctx, authContextCtxKey{}, actx)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			writeAuthError(w, http.StatusUnauthorized, result.Reason)
		})
	}
}

// GetAPIKey extracts the validated APIKey from the request context.
// Returns nil if the request bypassed the middleware.
func GetAPIKey(ctx context.Context) *APIKey {
	if v, ok := ctx.Value(apiKeyContextKey{}).(*APIKey); ok {
		return v
	}
	return nil
}

// WithAPIKey returns a child context carrying ak, readable by GetAPIKey. The
// middleware stamps the validated credential via the same (unexported) key on
// every authed path; this exported companion lets other entrypoints and tests
// set it without reaching the key. Read-only helper — no behavior change.
func WithAPIKey(ctx context.Context, ak *APIKey) context.Context {
	return context.WithValue(ctx, apiKeyContextKey{}, ak)
}

// extractKey looks at Authorization: Bearer first, then X-Talyvor-Key.
func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(h, "Bearer ") {
			return strings.TrimPrefix(h, "Bearer ")
		}
	}
	return r.Header.Get("X-Talyvor-Key")
}

// authUnavailableCode is the machine-readable half of an ErrAuthUnavailable 503. The suite's Chat
// keys on it to retry quietly, and it is what tells this 503 apart from a provider's
// "not configured" 503 on the same route.
const authUnavailableCode = "auth_unavailable"

// writeAuthUnavailable answers B27.5's "we could not check your credential" — a 503 with
// Retry-After, never a 401, so a client does not take a database hiccup for a sign-out.
func writeAuthUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", AuthUnavailableRetryAfter)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "your session could not be checked just now; try again",
		"code":  authUnavailableCode,
	})
}

func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
