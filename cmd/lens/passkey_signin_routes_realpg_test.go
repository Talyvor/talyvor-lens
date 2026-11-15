package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/phonesignin"
)

// B61.24 — the key routes, mounted as main.go mounts them, on a migrated schema: sign-up's one key ceremony keeps
// the key for the person over the account's approval challenge, and the key then signs the person in with no number.

type sharedApprovalChallenge struct{ issued []string }

func (s *sharedApprovalChallenge) PasskeyRegistrationChallenge(context.Context, string) (string, string, error) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	c := base64.RawURLEncoding.EncodeToString(b)
	s.issued = append(s.issued, c)
	return c, "app.talyvor.test", nil
}
func (s *sharedApprovalChallenge) DeviceRegistrationChallenge(context.Context, string) (string, error) {
	return "", nil
}

func TestKeySignInRoutes(t *testing.T) {
	const (
		secret = "provision-secret"
		rpID   = "app.talyvor.test"
		origin = "https://app.talyvor.test"
	)
	pool := phoneRoutesDB(t)
	reg := partners.NewRegistry(nil)
	store := &phonesignin.Store{DB: pool, Sender: reg.TextMessages, Key: []byte(secret),
		RP: &passkey.RelyingParty{ID: rpID, Origins: []string{origin}}}
	approval := &sharedApprovalChallenge{}
	r := chi.NewRouter()
	mountKeySignInRoutes(r, secret, store, approval)
	call := func(path string, body any, auth string) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		if auth != "" {
			req.Header.Set(provisionSecretHeader, auth)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := call("/v1/auth/passkey/challenge", map[string]string{"origin": origin}, ""); code != http.StatusUnauthorized {
		t.Fatalf("no secret: %d, want 401", code)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at)
		VALUES ('per_r', '+447700900401', 'person:per_r', true, now())`); err != nil {
		t.Fatal(err)
	}

	// Sign-up's key step: one challenge, shared with the account's approval key.
	code, rc := call("/v1/auth/passkey/register/challenge", map[string]string{"person_id": "per_r", "workspace_id": "ws_r"}, secret)
	if code != http.StatusOK || len(approval.issued) != 1 || rc["challenge"] != approval.issued[0] || rc["user_handle"] == "" {
		t.Fatalf("register challenge: %d %v (approval issued %v)", code, rc, approval.issued)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := []byte("credential-per-r")
	rpHash := sha256.Sum256([]byte(rpID))
	authData := func(flags byte, count uint32, cred []byte) []byte {
		out := append(append([]byte{}, rpHash[:]...), flags)
		out = binary.BigEndian.AppendUint32(out, count)
		if cred != nil {
			out = append(out, make([]byte, 16)...)
			out = binary.BigEndian.AppendUint16(out, uint16(len(cred)))
			out = append(out, cred...)
		}
		return out
	}
	cd := func(typ, c string) []byte {
		b, _ := json.Marshal(map[string]string{"type": typ, "challenge": c, "origin": origin})
		return b
	}
	enc := base64.RawURLEncoding.EncodeToString
	code, kept := call("/v1/auth/passkey/register", map[string]any{"person_id": "per_r", "name": "This Mac", "passkey": map[string]string{
		"credential_id": enc(credID), "public_key": enc(spki), "client_data_json": enc(cd("webauthn.create", rc["challenge"].(string))),
		"authenticator_data": enc(authData(0x45, 0, credID))}}, secret)
	if code != http.StatusCreated || kept["kind"] != "passkey" {
		t.Fatalf("sign-up's key: %d %v", code, kept)
	}

	// One touch: a challenge naming nobody, answered by the key.
	code, ch := call("/v1/auth/passkey/challenge", map[string]string{"origin": origin}, secret)
	if code != http.StatusOK || ch["user_verification"] != "required" {
		t.Fatalf("sign-in challenge: %d %v", code, ch)
	}
	cdj := cd("webauthn.get", ch["challenge"].(string))
	ad := authData(0x05, 1, nil)
	h := sha256.Sum256(cdj)
	signed := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, signed[:])
	answer := map[string]string{"credential_id": enc(credID), "client_data_json": enc(cdj), "authenticator_data": enc(ad), "signature": enc(sig)}
	code, in := call("/v1/auth/passkey/finish", answer, secret)
	if code != http.StatusOK || in["person"].(map[string]any)["id"] != "per_r" || in["ticket"] == "" {
		t.Fatalf("sign in: %d %v", code, in)
	}
	if code, again := call("/v1/auth/passkey/finish", answer, secret); code != http.StatusBadRequest || again["reason"] != "challenge_ended" {
		t.Fatalf("the same answer twice: %d %v", code, again)
	}

	// The person lists the key and cannot remove their last way in.
	_, list := call("/v1/auth/passkey/keys", map[string]string{"person_id": "per_r"}, secret)
	keys := list["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keys: %v", list)
	}
	id := keys[0].(map[string]any)["id"]
	if code, out := call("/v1/auth/passkey/keys/remove", map[string]any{"person_id": "per_r", "key_id": id}, secret); code != http.StatusConflict || out["reason"] != "last_way_in" {
		t.Fatalf("removing the last way in: %d %v", code, out)
	}
}
