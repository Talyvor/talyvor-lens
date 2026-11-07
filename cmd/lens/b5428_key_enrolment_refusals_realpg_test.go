package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// B54.28 — a passkey already registered is refused in words at once, never kept waiting to fail on approval with a 500;
// and a workspace whose every key is revoked gets no code no key could approve, but words pointing to recovery.
func TestKeyEnrolment_AKnownPasskeyIsRefusedAtOnce(t *testing.T) {
	h := newB551Harness(t, "ws-b5428-known")
	phone, laptop := newB551Phone(), newB551Passkey()
	if code, out := h.registerPhone(phone); code != http.StatusCreated {
		t.Fatalf("first key = %d %v", code, out)
	}
	id := h.waiting(func() (int, map[string]any) { return h.registerPasskey(laptop) }, laptop.spki)
	if code, out := h.approveEnrolment(id, phone.sign); code != http.StatusOK {
		t.Fatalf("letting the passkey in = %d %v", code, out)
	}
	code, out := h.registerPasskey(laptop)
	if msg, _ := out["error"].(string); code != http.StatusConflict || !strings.Contains(msg, "already registered") {
		t.Fatalf("registering a passkey this workspace already has = %d %v, want 409 saying it is already registered", code, out)
	}
	if n := h.countOf("key_enrolments"); n != 1 {
		t.Fatalf("%d enrolments, want only the one approved: a known passkey was kept waiting", n)
	}

	// Kept waiting here, then registered as another workspace's first key: approving it here is refused in words.
	desk := newB551Passkey()
	idDesk := h.waiting(func() (int, map[string]any) { return h.registerPasskey(desk) }, desk.spki)
	h.ws = "ws-b5428-known-other"
	if code, out := h.registerPasskey(desk); code != http.StatusCreated {
		t.Fatalf("the other workspace's first key = %d %v", code, out)
	}
	h.ws = "ws-b5428-known"
	code, out = h.approveEnrolment(idDesk, phone.sign)
	if msg, _ := out["error"].(string); code != http.StatusConflict || !strings.Contains(msg, "already registered") {
		t.Fatalf("approving a passkey registered meanwhile elsewhere = %d %v, want 409 saying it is already registered", code, out)
	}
	if n := h.countOf("workspace_passkeys"); n != 1 {
		t.Fatalf("%d passkeys here, want 1", n)
	}
}

func TestKeyEnrolment_OnlyRevokedKeysGetNoCode(t *testing.T) {
	h := newB551Harness(t, "ws-b5428-revoked")
	phone := newB551Phone()
	if code, out := h.registerPhone(phone); code != http.StatusCreated {
		t.Fatalf("first key = %d %v", code, out)
	}
	if code, out := h.call(http.MethodPost, "/v1/money/devices/"+phone.id+"/revoke", nil); code != http.StatusOK {
		t.Fatalf("revoking the phone = %d %v", code, out)
	}
	laptop, newPhone := newB551Passkey(), newB551Phone()
	for name, reg := range map[string]func() (int, map[string]any){
		"a passkey":   func() (int, map[string]any) { return h.registerPasskey(laptop) },
		"a phone key": func() (int, map[string]any) { return h.registerPhone(newPhone) },
	} {
		code, out := reg()
		if msg, _ := out["error"].(string); code != http.StatusConflict || !strings.Contains(msg, "recover") {
			t.Fatalf("%s registered where every key is revoked = %d %v, want 409 pointing to recovery", name, code, out)
		}
	}
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM key_enrolments WHERE workspace_id = $1`, h.ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || h.countOf("workspace_passkeys") != 0 || h.countOf("workspace_device_keys") != 1 {
		t.Fatalf("%d enrolments, %d passkeys, %d phone keys; want none waiting and nothing added", n,
			h.countOf("workspace_passkeys"), h.countOf("workspace_device_keys"))
	}
}
