package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKey generates a fresh EC P-256 key pair for the calling test.
// It fails the test immediately on any error — callers never see nil.
func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := GenerateECKey()
	if err != nil {
		t.Fatalf("GenerateECKey: %v", err)
	}
	return k
}

// ─── token generation + validation ──────────────

func TestGenerateToken_ProducesValidJWT(t *testing.T) {
	key := testKey(t)
	tok, err := GenerateToken("ws_1", "user_1", []string{ScopeProxy}, key, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(tok) == 0 {
		t.Fatal("expected non-empty token")
	}
	// Round-trip parse with the public key.
	parsed, err := jwt.ParseWithClaims(tok, &TokenClaims{}, func(_ *jwt.Token) (interface{}, error) {
		return &key.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claims := parsed.Claims.(*TokenClaims)
	if claims.WorkspaceID != "ws_1" || claims.UserID != "user_1" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.Issuer != JWTIssuer {
		t.Fatalf("expected issuer %q, got %q", JWTIssuer, claims.Issuer)
	}
	// kid must be set.
	if kid, _ := parsed.Header["kid"].(string); kid != JWTKid {
		t.Fatalf("expected kid %q, got %q", JWTKid, kid)
	}
}

func TestValidateToken_AcceptsValid(t *testing.T) {
	key := testKey(t)
	tok, _ := GenerateToken("ws_v", "user_v", []string{ScopeProxy, ScopeAnalytics}, key, time.Hour)
	claims, err := ValidateToken(tok, &key.PublicKey)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.WorkspaceID != "ws_v" {
		t.Fatalf("unexpected workspace: %s", claims.WorkspaceID)
	}
	if len(claims.Scopes) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(claims.Scopes))
	}
}

func TestValidateToken_RejectsExpired(t *testing.T) {
	key := testKey(t)
	// Negative TTL → already-expired.
	tok, _ := GenerateToken("ws_e", "u", []string{ScopeProxy}, key, -time.Minute)
	_, err := ValidateToken(tok, &key.PublicKey)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("expected ErrInvalidAuth, got %v", err)
	}
}

func TestValidateToken_RejectsWrongKey(t *testing.T) {
	signingKey := testKey(t)
	verifyKey := testKey(t) // different key pair
	tok, _ := GenerateToken("ws_w", "u", []string{ScopeProxy}, signingKey, time.Hour)
	_, err := ValidateToken(tok, &verifyKey.PublicKey)
	if err == nil {
		t.Fatal("expected error for wrong verification key")
	}
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("expected ErrInvalidAuth, got %v", err)
	}
}

func TestValidateToken_RejectsWrongIssuer(t *testing.T) {
	key := testKey(t)
	// Sign a token claiming a different issuer.
	claims := TokenClaims{
		WorkspaceID: "ws",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "evil-issuer",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(key)
	_, err := ValidateToken(tok, &key.PublicKey)
	if err == nil {
		t.Fatal("expected issuer mismatch to fail")
	}
}

// ─── Authenticate priority chain ────────────────

func newReq(authHeader string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", "Bearer "+authHeader)
	}
	return r
}

func TestAuthenticate_AcceptsGlobalKey(t *testing.T) {
	mgr := NewManager("super-secret-admin-key", nil, nil, nil)
	ctx, err := mgr.Authenticate(newReq("super-secret-admin-key"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ctx.IsAdmin || ctx.AuthMethod != MethodGlobalKey {
		t.Fatalf("expected admin / global_key, got %+v", ctx)
	}
}

func TestAuthenticate_AcceptsValidJWT(t *testing.T) {
	key := testKey(t)
	mgr := NewManager("", key, nil, nil)
	tok, _ := GenerateToken("ws_a", "user_a", []string{ScopeProxy}, key, time.Hour)
	ctx, err := mgr.Authenticate(newReq(tok))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ctx.AuthMethod != MethodJWT || ctx.WorkspaceID != "ws_a" {
		t.Fatalf("unexpected ctx: %+v", ctx)
	}
}

func TestAuthenticate_RejectsInvalidJWT(t *testing.T) {
	key := testKey(t)
	mgr := NewManager("", key, nil, nil)
	_, err := mgr.Authenticate(newReq("a.b.c"))
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("expected ErrInvalidAuth, got %v", err)
	}
}

func TestAuthenticate_RejectsMissingCredentials(t *testing.T) {
	mgr := NewManager("x", nil, nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	_, err := mgr.Authenticate(r)
	if !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("expected ErrMissingCredentials, got %v", err)
	}
}

func TestAuthenticate_LegacyXAPIKeyHeader(t *testing.T) {
	mgr := NewManager("legacy-key", nil, nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("X-API-Key", "legacy-key")
	ctx, err := mgr.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ctx.IsAdmin {
		t.Fatal("expected admin via legacy header")
	}
}

func TestAuthenticate_WorkspaceKeyOpaqueOnError(t *testing.T) {
	// Without a tenantStore wired, a tlv_ws_ key must be
	// rejected with ErrInvalidAuth — same shape as "wrong key".
	mgr := NewManager("", nil, nil, nil)
	_, err := mgr.Authenticate(newReq("tlv_ws_aabbccdd"))
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("expected ErrInvalidAuth, got %v", err)
	}
}

// ─── JWT cache ──────────────────────────────────

func TestAuthenticate_CachesJWTValidation(t *testing.T) {
	key := testKey(t)
	mgr := NewManager("", key, nil, nil)
	tok, _ := GenerateToken("ws_c", "u", []string{ScopeProxy}, key, time.Hour)
	// First call seeds the cache, second call should hit it.
	_, _ = mgr.Authenticate(newReq(tok))
	mgr.mu.RLock()
	_, cached := mgr.jwtCache[tok]
	mgr.mu.RUnlock()
	if !cached {
		t.Fatal("expected JWT cache to be populated")
	}
	_, err := mgr.Authenticate(newReq(tok))
	if err != nil {
		t.Fatalf("second authenticate: %v", err)
	}
}

// ─── RequireScope ───────────────────────────────

// withAuthContext is the test-only helper that lifts an
// AuthContext onto the request's ctx using the package-private
// key (same key Manager.Middleware writes to).
func withAuthContext(r *http.Request, c *AuthContext) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authContextCtxKey{}, c))
}

func TestRequireScope_AllowsMatchingScope(t *testing.T) {
	called := false
	handler := RequireScope(ScopeProxy)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r := withAuthContext(httptest.NewRequest(http.MethodGet, "/x", nil),
		&AuthContext{Scopes: []string{ScopeProxy}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("expected handler called + 200, got called=%v code=%d", called, rec.Code)
	}
}

func TestRequireScope_RejectsMissingScope(t *testing.T) {
	handler := RequireScope(ScopeAdmin)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	}))
	r := withAuthContext(httptest.NewRequest(http.MethodGet, "/x", nil),
		&AuthContext{Scopes: []string{ScopeProxy}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRequireScope_AdminBypassesCheck(t *testing.T) {
	handler := RequireScope(ScopeAdmin)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := withAuthContext(httptest.NewRequest(http.MethodGet, "/x", nil),
		&AuthContext{IsAdmin: true})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected admin to bypass scope check, got %d", rec.Code)
	}
}

func TestRequireScope_NoAuthCtx401(t *testing.T) {
	handler := RequireScope(ScopeProxy)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// ─── ClampTTL ───────────────────────────────────

func TestClampTTL(t *testing.T) {
	if got := ClampTTL(0); got != DefaultTokenTTL {
		t.Fatalf("zero → default, got %v", got)
	}
	if got := ClampTTL(2 * MaxTokenTTL); got != MaxTokenTTL {
		t.Fatalf("over-cap → MaxTokenTTL, got %v", got)
	}
	if got := ClampTTL(time.Hour); got != time.Hour {
		t.Fatalf("under-cap → echo, got %v", got)
	}
}

// B50.3 — the algorithm is pinned to ES256: a token naming "none", an HMAC (signed with the public key's bytes, the
// classic key-confusion forgery) or another ECDSA curve is refused.
func TestValidateToken_RefusesEveryAlgorithmButES256(t *testing.T) {
	key := testKey(t)
	claims := TokenClaims{WorkspaceID: "ws", RegisteredClaims: jwt.RegisteredClaims{
		Issuer: JWTIssuer, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(pub)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	es384, _ := jwt.NewWithClaims(jwt.SigningMethodES384, claims).SignedString(p384)
	for name, tok := range map[string]string{"none": none, "HS256": hs, "ES384": es384} {
		if _, err := ValidateToken(tok, &key.PublicKey); !errors.Is(err, ErrInvalidAuth) {
			t.Errorf("%s token: want ErrInvalidAuth, got %v", name, err)
		}
	}
}
