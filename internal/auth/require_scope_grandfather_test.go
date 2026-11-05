package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Scope enforcement, wired onto the proxy path. Until now all four scopes were
// DEAD — RequireScope had zero production callers, so a key created with
// scopes:["analytics"] (or none) could call the proxy. Wiring RequireScope onto
// the proxy path makes the proxy scope real. Its two back-compat carve-outs (an
// EMPTY scope set and a fast-path DB api_keys credential, both grandfathered as
// all-scopes) are gone since B50.33: a credential with no scopes passes nothing,
// and migration 0324 gave the pre-scope workspace keys {proxy}.
// Admin (the global key) is unchanged: IsAdmin short-circuits inside HasScope.

// withFastPathAPIKey injects an APIKey the way AuthMiddleware's DB fast path
// does (the apiKeyContextKey slot, no AuthContext) — the credential shape a
// key created via POST /v1/api/keys presents.
func withFastPathAPIKey(r *http.Request, k *APIKey) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, k))
}

func runProxyScopeGuard(t *testing.T, r *http.Request) int {
	t.Helper()
	h := RequireScope(ScopeProxy)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func proxyReq() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
}

// CONTROL: a workspace key that HAS the proxy scope passes the proxy path.
func TestRequireScope_ProxyScopePresent_Allows(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{Scopes: []string{ScopeProxy}})
	if code := runProxyScopeGuard(t, r); code != http.StatusOK {
		t.Fatalf("proxy-scoped key on proxy path: want 200, got %d", code)
	}
}

// HEADLINE: a workspace key WITHOUT the proxy scope is refused on the proxy
// path — 403 (it authenticated fine; it is forbidden), never 401.
func TestRequireScope_ProxyScopeMissing_Forbids(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{Scopes: []string{ScopeAnalytics}})
	if code := runProxyScopeGuard(t, r); code != http.StatusForbidden {
		t.Fatalf("analytics-only key on proxy path: want 403, got %d", code)
	}
}

// B50.33: an empty scope set passes nothing — it used to be grandfathered as all-scopes.
func TestRequireScope_EmptyScopesPassNothing(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{Scopes: nil})
	if code := runProxyScopeGuard(t, r); code != http.StatusForbidden {
		t.Fatalf("empty-scope key on proxy path: want 403, got %d", code)
	}
}

// B50.33: a fast-path api_keys credential has no scope field, so it passes no scope gate.
func TestRequireScope_FastPathAPIKeyPassesNothing(t *testing.T) {
	r := withFastPathAPIKey(proxyReq(), &APIKey{ID: "k1", WorkspaceID: "ws1", Active: true})
	if code := runProxyScopeGuard(t, r); code != http.StatusForbidden {
		t.Fatalf("fast-path api_keys credential on proxy path: want 403, got %d", code)
	}
}

// CONTROL: admin (global key) is unchanged — passes via IsAdmin.
func TestRequireScope_AdminUnchanged(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{
		IsAdmin: true,
		Scopes:  []string{ScopeProxy, ScopeAnalytics, ScopeAdmin, ScopeKeys},
	})
	if code := runProxyScopeGuard(t, r); code != http.StatusOK {
		t.Fatalf("admin key on proxy path: want 200, got %d", code)
	}
}

// CONTROL: a request with no credential at all (not behind AuthMiddleware) still
// 401s — the guard must not silently pass an unauthenticated request.
func TestRequireScope_NoCredential_Unauthorized(t *testing.T) {
	if code := runProxyScopeGuard(t, proxyReq()); code != http.StatusUnauthorized {
		t.Fatalf("no credential on proxy path: want 401, got %d", code)
	}
}
