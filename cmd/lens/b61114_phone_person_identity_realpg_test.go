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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
)

// B61.114 — the BFF's Google identity (issuer + a zero byte + sub) makes its person through the route, and comes back
// as it was sent.

func TestPhonePersonGoogleIdentity(t *testing.T) {
	const secret = "provision-secret"
	const identity = "https://accounts.google.com\x00108234567890123456789"
	reg := partners.NewRegistry(nil)
	store := &phonesignin.Store{DB: phoneRoutesDB(t), Sender: reg.TextMessages, Key: []byte(secret), PerAddressHour: 20,
		PasscodeSecret: []byte("passcode-secret-for-tests-0123456789"), Now: time.Now}
	r := chi.NewRouter()
	mountPhoneSignInRoutes(r, secret, store)
	call := func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set(provisionSecretHeader, secret)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	const phone = "+447700901144"
	_, st := call("/v1/auth/phone/start", map[string]string{"phone": phone, "caller_address": "203.0.113.40"})
	_, fin := call("/v1/auth/phone/finish", map[string]string{"phone": phone, "code": st["sandbox_code"].(string), "caller_address": "203.0.113.40"})
	status, out := call("/v1/auth/phone/person", map[string]string{"ticket": fin["ticket"].(string), "identity": identity,
		"passcode": "583920", "caller_address": "203.0.113.40"})
	if status != http.StatusOK || out["created"] != true {
		t.Fatalf("a Google identity on a new number: %d %v", status, out)
	}
	if got := out["person"].(map[string]any)["identity"]; got != identity {
		t.Fatalf("identity answered %q, want %q", got, identity)
	}
}

// failingPerson answers every redeem with a database error.
type failingPerson struct{ phoneSignIn }

func (failingPerson) RedeemWithPasscode(context.Context, string, string, string, string) (phonesignin.Person, bool, error) {
	return phonesignin.Person{}, false, &pgconn.PgError{Code: "22021", Message: "invalid byte sequence"}
}

// The route's 503 keeps its words and logs the cause's SQLSTATE, never the identity or the ticket.
func TestPhonePersonUnavailableLogsCause(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ticket":"tkt-secret","identity":"issuer-sub-secret"}`))
	phonePerson(rec, req, failingPerson{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	line := logs.String()
	if !strings.Contains(line, "SQLSTATE 22021") || strings.Contains(line, "tkt-secret") || strings.Contains(line, "issuer-sub-secret") {
		t.Fatalf("log line %q: want the SQLSTATE and neither the ticket nor the identity", line)
	}
}
