package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
)

// B50.61 — a token the BFF re-mints through /v1/provision's session mode keeps the session's real sign-in time when the
// BFF sends it as signed_in_at, so key recovery's 10-minute rule never trusts a re-mint as a fresh sign-in. The token
// goes through Lens's own authentication: re-minted for a session signed in three hours ago, with the header set to now
// and the right recovery code, it is refused and adds no key; the same re-mint without signed_in_at (a real sign-in)
// recovers.
func TestB5061_AReMintedTokenKeepsTheSessionsSignInTime(t *testing.T) {
	mgr, _ := provisionManager(t)
	keys := newKeyFixture(t)
	pr := chi.NewRouter()
	mountProvisionRoute(pr, provisionSecret, mgr, nil, keys.mint, nil)
	e, store := b5548NewEnv(t)
	ctx := context.Background()
	const who = "b5061-person"

	rec, first := doProvision(t, pr, provisionSecret, `{"identity":"`+who+`","country":"GB"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first sign-in = %d: %s", rec.Code, rec.Body.String())
	}
	ws := first.WorkspaceID
	reMint := func(extra string) *auth.AuthContext {
		t.Helper()
		rec, out := doProvision(t, pr, provisionSecret, `{"identity":"`+who+`","mode":"session","account_id":"`+ws+`"`+extra+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("session re-mint%s = %d: %s", extra, rec.Code, rec.Body.String())
		}
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", "Bearer "+out.Token)
		actx, err := keys.mgr.Authenticate(req)
		if err != nil {
			t.Fatalf("authenticate the re-minted token: %v", err)
		}
		return actx
	}
	rc := e.lockedOut(ctx, store, ws)
	recover := func(actx *auth.AuthContext) (int, map[string]any) {
		code, out, _ := e.call(actx, "203.0.113.10", 0, http.MethodPost, "/v1/workspaces/"+ws+"/keys/recover",
			map[string]any{"code": rc, "passkey": e.passkeyRegistration(ws)})
		return code, out
	}

	then := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	if code, out := recover(reMint(`,"signed_in_at":"` + then + `"`)); code != http.StatusForbidden || out["error"] != b5548SignInAgain {
		t.Fatalf("recover on a token re-minted for a session signed in 3 hours ago, header set to now = %d %v, want 403 %q",
			code, out, b5548SignInAgain)
	}
	if n := b5548LiveKeys(t, store, ws); n != 0 {
		t.Fatalf("live keys after the refused recovery = %d, want 0", n)
	}

	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	rec, _ = doProvision(t, pr, provisionSecret, `{"identity":"`+who+`","mode":"session","account_id":"`+ws+`","signed_in_at":"`+future+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("re-mint with signed_in_at 5 minutes ahead = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	if code, out := recover(reMint("")); code != http.StatusCreated {
		t.Fatalf("recover on a token minted without signed_in_at (a sign-in now) = %d %v, want 201", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 1 {
		t.Fatalf("live keys after the recovery = %d, want 1", n)
	}
}
