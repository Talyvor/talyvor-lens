package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/auth"
)

// B50.60 — key recovery's "signed in within 10 minutes" is the token's own signed sign-in time, never only the
// X-Talyvor-Signed-In-At header the caller writes. The owner's real JWT goes through Lens's own authentication: one
// that signed in three hours ago, sending the header set to now and the right recovery code, is refused and adds no
// key; the same, refreshed through /v1/auth/refresh, is refused too; a token minted now is let in.
func TestB5060_AnOldSessionCannotWriteItsOwnSignInTime(t *testing.T) {
	e, store := b5548NewEnv(t)
	ctx := context.Background()
	const ws = "ws-b5060-signin"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	am := auth.NewManager("", key, auth.New(nil), nil)
	ownerOf := func(token string) *auth.AuthContext {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		actx, err := am.Authenticate(req)
		if err != nil {
			t.Fatalf("authenticate the owner's token: %v", err)
		}
		return actx
	}
	// A sign-in three hours ago: what a stolen session's token looks like.
	then := time.Now().Add(-3 * time.Hour)
	old := jwt.NewWithClaims(jwt.SigningMethodES256, auth.TokenClaims{WorkspaceID: ws, UserID: "ada", Scopes: []string{auth.ScopeKeys},
		RegisteredClaims: jwt.RegisteredClaims{Issuer: auth.JWTIssuer, Subject: "ada", ID: uuid.NewString(),
			IssuedAt: jwt.NewNumericDate(then), NotBefore: jwt.NewNumericDate(then), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	old.Header["kid"] = auth.JWTKid
	oldToken, err := old.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	rc := e.lockedOut(ctx, store, ws)
	recover := func(actx *auth.AuthContext) (int, map[string]any) {
		code, out, _ := e.call(actx, "203.0.113.10", 0, http.MethodPost, "/v1/workspaces/"+ws+"/keys/recover",
			map[string]any{"code": rc, "passkey": e.passkeyRegistration(ws)})
		return code, out
	}

	if code, out := recover(ownerOf(oldToken)); code != http.StatusForbidden || out["error"] != b5548SignInAgain {
		t.Fatalf("recover on a session signed in 3 hours ago, header set to now = %d %v, want 403 %q", code, out, b5548SignInAgain)
	}
	refreshed, err := auth.RefreshToken(ownerOf(oldToken), key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := recover(ownerOf(refreshed)); code != http.StatusForbidden || out["error"] != b5548SignInAgain {
		t.Fatalf("recover on that session refreshed just now = %d %v, want 403 %q", code, out, b5548SignInAgain)
	}
	if n := b5548LiveKeys(t, store, ws); n != 0 {
		t.Fatalf("live keys after the refused recoveries = %d, want 0", n)
	}

	fresh, err := auth.GenerateToken(ws, "ada", []string{auth.ScopeKeys}, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := recover(ownerOf(fresh)); code != http.StatusCreated {
		t.Fatalf("recover on a session signed in now = %d %v, want 201", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 1 {
		t.Fatalf("live keys after the recovery = %d, want 1", n)
	}
}
