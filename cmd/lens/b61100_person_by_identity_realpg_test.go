package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
)

// B61.100 — the BFF asks, at a Google sign-in, which person holds that Google identity: a person who moved it to a
// number is found (with whether they have a passcode, never their number), an identity nobody holds is 404, and each
// ask is a row in the door's audit trail.
func TestPersonByIdentity(t *testing.T) {
	const secret = "provision-secret"
	const identity = "https://accounts.google.com\x00108234567890100100100"
	const phone = "+447700901001"
	db := phoneRoutesDB(t)
	reg := partners.NewRegistry(nil)
	store := &phonesignin.Store{DB: db, Sender: reg.TextMessages, Key: []byte(secret), PerAddressHour: 20,
		PasscodeSecret: []byte("passcode-secret-for-tests-0123456789"), Now: time.Now}
	r := chi.NewRouter()
	mountPhoneSignInRoutes(r, secret, store)
	call := func(path string, body any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set(provisionSecretHeader, secret)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	decode := func(raw string) map[string]any {
		var out map[string]any
		_ = json.Unmarshal([]byte(raw), &out)
		return out
	}

	// The Google identity moves to a number: the door makes its person holding it.
	_, raw := call("/v1/auth/phone/start", map[string]string{"phone": phone, "caller_address": "203.0.113.70"})
	_, raw = call("/v1/auth/phone/finish", map[string]string{"phone": phone, "code": decode(raw)["sandbox_code"].(string), "caller_address": "203.0.113.70"})
	status, raw := call("/v1/auth/phone/person", map[string]string{"ticket": decode(raw)["ticket"].(string), "identity": identity,
		"passcode": "583920", "caller_address": "203.0.113.70"})
	if status != http.StatusOK {
		t.Fatalf("moving the identity to a number: %d %s", status, raw)
	}
	personID := decode(raw)["person"].(map[string]any)["id"].(string)

	status, raw = call("/v1/auth/person/by-identity", map[string]string{"identity": identity, "caller_address": "203.0.113.71"})
	if status != http.StatusOK {
		t.Fatalf("a moved identity: %d %s, want 200", status, raw)
	}
	found := decode(raw)
	if found["person_id"] != personID || found["has_passcode"] != true {
		t.Fatalf("a moved identity answered %s, want person %s with a passcode", raw, personID)
	}
	if strings.Contains(raw, "7700901001") || len(found) != 2 {
		t.Fatalf("the answer %s holds more than the person and their passcode", raw)
	}

	status, raw = call("/v1/auth/person/by-identity", map[string]string{"identity": "https://accounts.google.com\x00nobody", "caller_address": "203.0.113.71"})
	if status != http.StatusNotFound || decode(raw)["reason"] != "no_person" {
		t.Fatalf("an unknown identity: %d %s, want 404 no_person", status, raw)
	}

	var foundRows, missRows int
	if err := db.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE detail = 'a person holds it' AND phone = $1),
		count(*) FILTER (WHERE detail = 'no person holds it' AND phone = '')
		FROM phone_signin_events WHERE event = 'identity_looked_up' AND caller_address = '203.0.113.71'`, phone).
		Scan(&foundRows, &missRows); err != nil {
		t.Fatal(err)
	}
	if foundRows != 1 || missRows != 1 {
		t.Fatalf("audit rows: %d found, %d missed; want one of each", foundRows, missRows)
	}

	if status, _ := call("/v1/auth/person/by-identity", map[string]string{"caller_address": "203.0.113.71"}); status != http.StatusBadRequest {
		t.Fatalf("no identity: %d, want 400", status)
	}
}
