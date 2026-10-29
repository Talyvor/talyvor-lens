package main

import (
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
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B50.20 — once a workspace has passkey A, a session alone cannot enrol passkey B (or a phone key) and then sign with it:
// B is refused without A's signature, refused with A's signature over any other challenge, and cannot approve a payment;
// signed by A over B's own registration challenge, B is registered and approves.
func TestPasskeyEnrol_SecondKeyNeedsTheFirstKeysSignature(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b5020-enrol", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountDeviceKeyRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	// An authenticator this test holds: the body that registers it over a challenge, and its assertion over one.
	type authenticator struct {
		register func(challenge string) map[string]any
		sign     func(challenge string) *passkey.Assertion
	}
	newAuthenticator := func(name string) authenticator {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		credID := make([]byte, 16)
		_, _ = rand.Read(credID)
		counter := uint32(0)
		return authenticator{
			register: func(challenge string) map[string]any {
				ad := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
				ad = append(append(binary.BigEndian.AppendUint16(append(ad, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
				return map[string]any{"credential_id": b64(credID), "name": name, "public_key": b64(spki),
					"client_data_json": b64(clientData("webauthn.create", challenge)), "authenticator_data": b64(ad)}
			},
			sign: func(challenge string) *passkey.Assertion {
				counter++
				cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
				h := sha256.Sum256(cdj)
				digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
				sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
				return &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
			},
		}
	}
	base := "/v1/workspaces/" + ws + "/agents"
	challenge := func(path string) string {
		t.Helper()
		code, c := call(path, nil)
		if code != http.StatusOK {
			t.Fatalf("%s = %d %v", path, code, c)
		}
		return c["challenge"].(string)
	}
	enrol := func(a authenticator, signer *authenticator, signOver string) (int, map[string]any) {
		t.Helper()
		c := challenge(base + "/passkeys/challenge")
		body := a.register(c)
		if signer != nil {
			if signOver == "" {
				signOver = c
			}
			body["assertion"] = signer.sign(signOver)
		}
		return call(base+"/passkeys", body)
	}
	passkeys := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	agent, err := store.CreateAgent(ctx, ws, "researcher", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	vendor, err := store.CreateAgent(ctx, ws, "vendor", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	held, err := store.RequestPaymentApproval(ctx, ws, agent.ID, vendor.ID, 2_000_000, "invoice 7", "October hosting")
	if err != nil {
		t.Fatal(err)
	}
	approveSignedBy := func(a authenticator) (int, string) {
		t.Helper()
		code, _ := call(base+"/approvals/"+held.ID+"/approve", map[string]any{"assertion": a.sign(challenge(base + "/approvals/" + held.ID + "/challenge"))})
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, held.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return code, status
	}

	// The first passkey is free to register.
	a, b := newAuthenticator("Ada's phone"), newAuthenticator("someone else's key")
	if code, out := enrol(a, nil, ""); code != http.StatusCreated {
		t.Fatalf("registering the first passkey = %d %v, want 201", code, out)
	}
	// B with no signature, and B signed by A over some other challenge: both refused, B kept nowhere.
	if code, out := enrol(b, nil, ""); code != http.StatusForbidden || passkeys() != 1 {
		t.Fatalf("registering B with no signature = %d %v, %d passkeys; want 403 and only A", code, out, passkeys())
	}
	if code, out := enrol(b, &a, challenge(base+"/approvals/"+held.ID+"/challenge")); code != http.StatusBadRequest || passkeys() != 1 {
		t.Fatalf("registering B signed by A over an approval's challenge = %d %v, %d passkeys; want 400 and only A", code, out, passkeys())
	}
	// Nor a phone key with no signature.
	dev := challenge("/v1/money/devices/challenge")
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	phoneSPKI, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	h := sha256.Sum256(devicekey.Message(dev))
	phoneSig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
	device := map[string]any{"platform": "test", "name": "a phone", "public_key": b64(phoneSPKI), "challenge": dev, "signature": b64(phoneSig), "attestation": ""}
	if code, out := call("/v1/money/devices", device); code != http.StatusForbidden {
		t.Fatalf("registering a phone key with no signature = %d %v, want 403", code, out)
	}
	// So B cannot approve the payment.
	if code, status := approveSignedBy(b); code != http.StatusForbidden || status != "pending" {
		t.Fatalf("approving with the refused key B = %d, approval %q; want 403 and still pending", code, status)
	}

	// Signed by A over B's own challenge: B is registered, and approves.
	if code, out := enrol(b, &a, ""); code != http.StatusCreated || passkeys() != 2 {
		t.Fatalf("registering B signed by A = %d %v, %d passkeys; want 201 and two", code, out, passkeys())
	}
	device["assertion"] = a.sign(dev)
	if code, out := call("/v1/money/devices", device); code != http.StatusCreated {
		t.Fatalf("registering a phone key signed by A = %d %v, want 201", code, out)
	}
	if code, status := approveSignedBy(b); code != http.StatusOK || status != "approved" {
		t.Fatalf("approving with B once A enrolled it = %d, approval %q; want 200 approved", code, status)
	}
}
