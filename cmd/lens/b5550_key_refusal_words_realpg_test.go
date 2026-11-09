package main

import (
	"net/http"
	"strings"
	"testing"
)

// B55.50 — a key waiting to join, or one Lens refuses, is answered in a sentence a person can act on: never the
// package's name, never a dash.
func TestKeyRefusals_AreSentences(t *testing.T) {
	h := newB551Harness(t, "ws-b5550-words")
	says := func(what string, code int, out map[string]any, wantCode int, want string) {
		t.Helper()
		msg, _ := out["error"].(string)
		if code != wantCode || msg != want {
			t.Fatalf("%s = %d %q, want %d %q", what, code, msg, wantCode, want)
		}
		if strings.Contains(msg, "economy:") || strings.ContainsAny(msg, "—–") {
			t.Fatalf("%s says %q: machine text or a dash", what, msg)
		}
	}
	phone := newB551Phone()
	if code, out := h.registerPhone(phone); code != http.StatusCreated {
		t.Fatalf("first key = %d %v", code, out)
	}

	laptop := newB551Passkey()
	id := h.waiting(func() (int, map[string]any) { return h.registerPasskey(laptop) }, laptop.spki)
	code, out := h.registerPasskey(laptop)
	says("a second key with a waiting key's code", code, out, http.StatusConflict,
		"A key with the same code already waits to join this workspace. Answer that one first.")

	code, out = h.call(http.MethodPost, h.base()+"/keys/enrolments/"+id+"/approve", map[string]any{})
	says("approving a waiting key unsigned", code, out, http.StatusForbidden,
		"This workspace approves with a passkey or the phone's key, so this needs one of them.")

	for _, k := range []b551Passkey{newB551Passkey(), newB551Passkey()} {
		h.waiting(func() (int, map[string]any) { return h.registerPasskey(k) }, k.spki)
	}
	code, out = h.registerPasskey(newB551Passkey())
	says("a fourth key waiting", code, out, http.StatusConflict,
		"Three keys already wait to join this workspace. Answer one of them first.")

	if code, out := h.approveEnrolment(id, phone.sign); code != http.StatusOK {
		t.Fatalf("letting the passkey in = %d %v", code, out)
	}
	code, out = h.registerPasskey(laptop)
	says("a passkey already added", code, out, http.StatusConflict,
		"This passkey is already added. Use it as it is, or make a new passkey on this device.")

	h.ws = "ws-b5550-removed"
	only := newB551Phone()
	if code, out := h.registerPhone(only); code != http.StatusCreated {
		t.Fatalf("first key = %d %v", code, out)
	}
	if code, out := h.call(http.MethodPost, "/v1/money/devices/"+only.id+"/revoke", nil); code != http.StatusOK {
		t.Fatalf("revoking the phone = %d %v", code, out)
	}
	code, out = h.registerPhone(newB551Phone())
	says("a key where every key was removed", code, out, http.StatusConflict,
		"Every key of this workspace was removed, so no key can let a new one in.")
}
