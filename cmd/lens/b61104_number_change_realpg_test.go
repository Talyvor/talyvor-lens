package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
	"github.com/talyvor/lens/internal/workspace"
)

type b61104Accounts struct{ ids []string }

func (a b61104Accounts) PersonAccounts(context.Context, string, string) ([]workspace.PersonAccount, error) {
	out := make([]workspace.PersonAccount, 0, len(a.ids))
	for _, id := range a.ids {
		out = append(out, workspace.PersonAccount{ID: id})
	}
	return out, nil
}

type b61104Teller struct{ told [][]string }

func (t *b61104Teller) TellNumberChanged(_ context.Context, accounts []string) { t.told = append(t.told, accounts) }

// B61.104 — a person's own number change, mounted as main.go mounts it beside the phone door: the new number's code
// and the passcode move them onto it, the door then signs them in by the new number and not the old, and their
// accounts' devices are told; another person's number and a wrong passcode are refused in the door's words.
func TestPersonChangesTheirNumberThroughTheDoor(t *testing.T) {
	const secret, passcode = "provision-secret", "583920"
	clock := time.Now()
	pool := phoneRoutesDB(t)
	teller := &b61104Teller{}
	prevTell := tellInBackground
	tellInBackground = func(tell func(ctx context.Context)) { tell(context.Background()) }
	t.Cleanup(func() { tellInBackground = prevTell })
	door := &phonesignin.Store{DB: pool, Sender: partners.NewRegistry(nil).TextMessages, Key: []byte(secret), PerAddressHour: 100,
		PasscodeSecret: []byte("passcode-secret-for-tests-0123456789"), Now: func() time.Time { return clock }}
	r := chi.NewRouter()
	mountPhoneSignInRoutes(r, secret, door)
	mountNumberChangeRoute(r, secret, door, b61104Accounts{ids: []string{"u-b61104"}}, teller)
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
	ticket := func(phone string) string {
		t.Helper()
		clock = clock.Add(time.Minute)
		_, st := call("/v1/auth/phone/start", map[string]string{"phone": phone, "caller_address": "203.0.113.50"})
		_, fin := call("/v1/auth/phone/finish", map[string]string{"phone": phone, "code": st["sandbox_code"].(string), "caller_address": "203.0.113.50"})
		return fin["ticket"].(string)
	}
	join := func(phone string) string {
		t.Helper()
		status, out := call("/v1/auth/phone/person", map[string]string{"ticket": ticket(phone), "passcode": passcode})
		if status != http.StatusOK {
			t.Fatalf("join %s: %d %v", phone, status, out)
		}
		return out["person"].(map[string]any)["id"].(string)
	}
	const old, fresh, other = "+447700900611", "+447700900612", "+447700900613"
	id := join(old)
	join(other)
	change := func(tk, pc string) (int, map[string]any) {
		return call("/v1/auth/phone/change-number", map[string]string{"person_id": id, "ticket": tk, "passcode": pc, "caller_address": "203.0.113.50"})
	}

	if status, out := change(ticket(other), passcode); status != http.StatusConflict || out["reason"] != "held" ||
		out["error"] != "That number or identity belongs to another person." {
		t.Fatalf("onto another person's number: %d %v", status, out)
	}
	tk := ticket(fresh)
	if status, out := change(tk, "290471"); status != http.StatusBadRequest || out["reason"] != "wrong_passcode" {
		t.Fatalf("a wrong passcode: %d %v", status, out)
	}
	status, out := change(tk, passcode)
	person, _ := out["person"].(map[string]any)
	if status != http.StatusOK || person["id"] != id || person["phone"] != fresh {
		t.Fatalf("the change: %d %v", status, out)
	}
	if len(teller.told) != 1 || len(teller.told[0]) != 1 || teller.told[0][0] != "u-b61104" {
		t.Fatalf("devices told for %v; want the person's one account", teller.told)
	}

	// The door signs them in by the new number, and the old number finds somebody new, never them.
	if status, out := call("/v1/auth/phone/person", map[string]string{"ticket": ticket(fresh), "passcode": passcode}); status != http.StatusOK ||
		out["created"] != false || out["person"].(map[string]any)["id"] != id {
		t.Fatalf("sign-in by the new number: %d %v", status, out)
	}
	if status, out := call("/v1/auth/phone/person", map[string]string{"ticket": ticket(old), "passcode": "381946"}); status != http.StatusOK ||
		out["created"] != true || out["person"].(map[string]any)["id"] == id {
		t.Fatalf("sign-in by the old number: %d %v", status, out)
	}
}
