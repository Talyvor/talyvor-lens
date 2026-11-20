package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/phonesignin"
)

type fakeReplacer struct {
	err      error
	got      passcodeReplaceRequest
	replaced int
}

func (f *fakeReplacer) ReplaceForgottenPasscode(_ context.Context, phoneTicket, keyTicket, passcode, address string) (phonesignin.Person, error) {
	f.got = passcodeReplaceRequest{Ticket: phoneTicket, KeyTicket: keyTicket, Passcode: passcode, Address: address}
	if f.err != nil {
		return phonesignin.Person{}, f.err
	}
	f.replaced++
	return phonesignin.Person{ID: "per_f", HasPasscode: true}, nil
}

// B61.107 — the forgot route on the provision secret: both tickets reach the store, and each refusal is said in words.
func TestB61107PasscodeReplaceRoute(t *testing.T) {
	const secret = "provision-secret"
	f := &fakeReplacer{}
	r := chi.NewRouter()
	mountPasscodeReplaceRoute(r, secret, f)
	call := func(auth string) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"ticket": "pt", "key_ticket": "kt", "passcode": "314159", "caller_address": " 203.0.113.9 "})
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/passcode/replace", bytes.NewReader(b))
		req.Header.Set(provisionSecretHeader, auth)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := call("wrong"); code != http.StatusUnauthorized || f.got.Ticket != "" {
		t.Fatalf("without the secret: %d, store called with %+v", code, f.got)
	}
	code, out := call(secret)
	want := passcodeReplaceRequest{Ticket: "pt", KeyTicket: "kt", Passcode: "314159", Address: "203.0.113.9"}
	if code != http.StatusOK || out["ok"] != true || f.got != want || f.replaced != 1 {
		t.Fatalf("both tickets: %d %v, store got %+v", code, out, f.got)
	}
	for _, c := range []struct {
		err    error
		status int
		reason string
	}{
		{phonesignin.ErrTicket, http.StatusBadRequest, "ticket_ended"},
		{phonesignin.ErrKeyTicket, http.StatusForbidden, "key_needed"},
		{phonesignin.ErrTwoPeople, http.StatusForbidden, "not_same_person"},
		{phonesignin.ErrNoPerson, http.StatusNotFound, "no_person"},
		{&phonesignin.WeakPasscodeError{Reason: "A passcode is 6 digits."}, http.StatusBadRequest, "weak_passcode"},
	} {
		f.err = c.err
		if code, out := call(secret); code != c.status || out["reason"] != c.reason || out["error"] == "" {
			t.Fatalf("%v: %d %v, want %d %s in words", c.err, code, out, c.status, c.reason)
		}
	}
}
