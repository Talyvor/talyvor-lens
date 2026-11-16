package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
)

// B61.113 — while LENS_PASSCODE_SECRET is missing the phone door sends no code: start answers 503
// passcode_unavailable before anyone confirms a number, and /healthz says the door is unready.

const b61113Secret = "b61113-passcode-secret-0123456789abcdef"

func TestB61113PhoneDoorStartWaitsForThePasscodeSecret(t *testing.T) {
	const provision = "provision-secret"
	pool := phoneRoutesDB(t)
	store := &phonesignin.Store{DB: pool, Sender: partners.NewRegistry(nil).TextMessages, Key: []byte(provision), PerAddressHour: 20}
	r := chi.NewRouter()
	mountPhoneSignInRoutes(r, provision, store)
	health := api.NewHealthHandler("test", nil).AddPublicSection("phone_signin", phoneSignInHealth(store))
	r.Get("/healthz", health.ServeHTTP)

	var bodies strings.Builder
	start := func(phone string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/phone/start",
			strings.NewReader(`{"phone":"`+phone+`","caller_address":"203.0.113.90"}`))
		req.Header.Set(provisionSecretHeader, provision)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		bodies.Write(rec.Body.Bytes())
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	healthz := func() (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		bodies.Write(rec.Body.Bytes())
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	checks := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM phone_checks`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, secret := range []string{"", b61113Secret[:31]} {
		store.PasscodeSecret = []byte(secret)
		status, out := start("+447700900390")
		if status != http.StatusServiceUnavailable || out["reason"] != "passcode_unavailable" ||
			out["error"] != "Sign-up by phone isn't ready yet. Try again soon." || out["sandbox_code"] != nil {
			t.Fatalf("start with a %d-character secret: %d %v, want 503 passcode_unavailable and no code", len(secret), status, out)
		}
		if n := checks(); n != 0 {
			t.Fatalf("start with a %d-character secret wrote %d code rows, want none", len(secret), n)
		}
		status, out = healthz()
		door, _ := out["phone_signin"].(map[string]any)
		if status != http.StatusOK || out["status"] != "healthy" || door["status"] != "unready" || door["reason"] != "passcode secret missing" {
			t.Fatalf("/healthz with a %d-character secret: %d %v, want phone_signin unready and the overall status unchanged", len(secret), status, out)
		}
	}

	store.PasscodeSecret = []byte(b61113Secret[:32])
	status, out := start("+447700900390")
	if status != http.StatusOK || out["sandbox_code"] == nil {
		t.Fatalf("start with a 32-character secret: %d %v, want the code as today", status, out)
	}
	if n := checks(); n != 1 {
		t.Fatalf("start with a 32-character secret wrote %d code rows, want 1", n)
	}
	if _, out := healthz(); out["phone_signin"].(map[string]any)["status"] != "healthy" {
		t.Fatalf("/healthz with the secret: %v, want phone_signin healthy", out)
	}
	if strings.Contains(bodies.String(), b61113Secret[:31]) {
		t.Fatal("an answer carries the passcode secret")
	}
}

func TestB61113OneLogLineNamesTheMissingVariable(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	store := &phonesignin.Store{}
	logPhoneSignInReadiness(logger, store)
	if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "LENS_PASSCODE_SECRET is not set") {
		t.Fatalf("without the secret, want one line naming LENS_PASSCODE_SECRET, got %q", logs.String())
	}
	logs.Reset()
	store.PasscodeSecret = []byte(b61113Secret)
	logPhoneSignInReadiness(logger, store)
	if logs.Len() != 0 {
		t.Fatalf("with the secret, want no line, got %q", logs.String())
	}
}
