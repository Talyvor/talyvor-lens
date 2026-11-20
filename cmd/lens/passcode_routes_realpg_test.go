package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
)

// B61.23 — the passcode's routes, mounted as main.go mounts them: set at sign-up with the ticket, needed with every
// ticket after, checked for a session of the person, changed with the old one; 503 with no secret.
func TestPasscodeRoutes(t *testing.T) {
	const secret, passcode = "provision-secret", "583920"
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	reg := partners.NewRegistry(nil)
	clock := time.Now()
	pool := phoneRoutesDB(t)
	mount := func(passcodeSecret string) chi.Router {
		r := chi.NewRouter()
		mountPhoneSignInRoutes(r, secret, &phonesignin.Store{DB: pool, Sender: reg.TextMessages, Key: []byte(secret), PerAddressHour: 100,
			PasscodeSecret: []byte(passcodeSecret), Now: func() time.Time { return clock }})
		return r
	}
	r, unset := mount("passcode-secret-for-tests-0123456789"), mount("")
	call := func(r chi.Router, path string, body any) (int, map[string]any, http.Header) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set(provisionSecretHeader, secret)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Header()
	}
	const phone = "+447700900501"
	ticket := func() string {
		clock = clock.Add(time.Minute)
		_, st, _ := call(r, "/v1/auth/phone/start", map[string]string{"phone": phone, "caller_address": "203.0.113.50"})
		_, fin, _ := call(r, "/v1/auth/phone/finish", map[string]string{"phone": phone, "code": st["sandbox_code"].(string), "caller_address": "203.0.113.50"})
		return fin["ticket"].(string)
	}

	// Sign-up sets it with the ticket; a weak one is refused in words and leaves the ticket.
	first := ticket()
	if status, out, _ := call(r, "/v1/auth/phone/person", map[string]string{"ticket": first, "passcode": "123456"}); status != http.StatusBadRequest ||
		out["reason"] != "weak_passcode" || !strings.Contains(out["error"].(string), "123456") {
		t.Fatalf("a weak passcode at sign-up: %d %v", status, out)
	}
	status, joined, _ := call(r, "/v1/auth/phone/person", map[string]string{"ticket": first, "passcode": passcode})
	person, _ := joined["person"].(map[string]any)
	if status != http.StatusOK || joined["created"] != true || person["has_passcode"] != true {
		t.Fatalf("sign-up with a passcode: %d %v", status, joined)
	}
	id := person["id"].(string)

	// After that, a ticket alone is refused, and a wrong passcode says only that.
	next := ticket()
	if status, out, _ := call(r, "/v1/auth/phone/person", map[string]string{"ticket": next}); status != http.StatusBadRequest || out["reason"] != "passcode_needed" {
		t.Fatalf("a ticket alone: %d %v", status, out)
	}
	if status, out, _ := call(r, "/v1/auth/phone/person", map[string]string{"ticket": next, "passcode": "583921"}); status != http.StatusBadRequest ||
		out["reason"] != "wrong_passcode" || len(out) != 2 {
		t.Fatalf("a wrong passcode with the ticket: %d %v", status, out)
	}
	if status, out, _ := call(r, "/v1/auth/phone/person", map[string]string{"ticket": next, "passcode": passcode}); status != http.StatusOK || out["created"] != false {
		t.Fatalf("the ticket with the passcode: %d %v", status, out)
	}

	// The lock: right, then wrong until the wait, which answers when.
	if status, out, _ := call(r, "/v1/auth/passcode/check", map[string]string{"person_id": id, "passcode": passcode}); status != http.StatusOK || out["ok"] != true {
		t.Fatalf("the right passcode: %d %v", status, out)
	}
	for i := 1; i < phonesignin.PasscodeWaitAfter; i++ {
		if status, out, _ := call(r, "/v1/auth/passcode/check", map[string]string{"person_id": id, "passcode": "000000"}); status != http.StatusBadRequest || out["reason"] != "wrong_passcode" {
			t.Fatalf("wrong try %d: %d %v", i, status, out)
		}
	}
	status, out, h := call(r, "/v1/auth/passcode/check", map[string]string{"person_id": id, "passcode": "000000"})
	if status != http.StatusTooManyRequests || out["reason"] != "passcode_wait" || out["retry_at"] == nil || h.Get("Retry-After") == "" {
		t.Fatalf("the fifth wrong try: %d %v %v", status, out, h)
	}
	if status, out, _ := call(r, "/v1/auth/passcode/change", map[string]string{"person_id": id, "old_passcode": passcode, "passcode": "314159"}); status != http.StatusTooManyRequests {
		t.Fatalf("a change during the wait: %d %v", status, out)
	}
	clock = clock.Add(phonesignin.PasscodeWait)
	if status, out, _ := call(r, "/v1/auth/passcode/change", map[string]string{"person_id": id, "old_passcode": passcode, "passcode": "314159"}); status != http.StatusOK {
		t.Fatalf("a change with the old passcode: %d %v", status, out)
	}

	// No secret: setting and checking answer 503.
	if status, _, _ := call(unset, "/v1/auth/passcode/check", map[string]string{"person_id": id, "passcode": "314159"}); status != http.StatusServiceUnavailable {
		t.Fatalf("checking with no secret: %d", status)
	}
	if status, _, _ := call(unset, "/v1/auth/phone/person", map[string]string{"ticket": ticket(), "passcode": "314159"}); status != http.StatusServiceUnavailable {
		t.Fatalf("signing in with no secret: %d", status)
	}

	for _, p := range []string{passcode, "314159"} {
		if strings.Contains(logs.String(), p) {
			t.Fatalf("a passcode is in the logs: %s", logs.String())
		}
	}
}
