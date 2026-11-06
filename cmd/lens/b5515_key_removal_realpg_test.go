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
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

const b5515LastKey = "This is your last key. With none, nobody could approve, so add another first."

// b5515Workspace is a workspace with one passkey and one phone key, each held by the test, mounted on its own router.
type b5515Workspace struct {
	call        func(method, path string, body any) (int, map[string]any)
	signPasskey func(challenge string) *passkey.Assertion
	signPhone   func(challenge string) *passkey.Assertion
	credID      string
	deviceID    string
}

func b5515NewWorkspace(t *testing.T, store *economy.DualTokenStore, r chi.Router, ws string) b5515Workspace {
	t.Helper()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	var w b5515Workspace
	w.call = func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}

	// The passkey: free to register, as the workspace's first key.
	pk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pkSPKI, _ := x509.MarshalPKIXPublicKey(&pk.PublicKey)
	cred := make([]byte, 16)
	_, _ = rand.Read(cred)
	w.credID = b64(cred)
	counter := uint32(0)
	w.signPasskey = func(challenge string) *passkey.Assertion {
		counter++
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, pk, digest[:])
		return &passkey.Assertion{CredentialID: w.credID, ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	}
	_, c := w.call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/passkeys/challenge", nil)
	ad := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	ad = append(append(binary.BigEndian.AppendUint16(append(ad, make([]byte, 16)...), uint16(len(cred))), cred...), 0xa0)
	if code, out := w.call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/passkeys", map[string]any{"credential_id": w.credID, "name": "This Mac",
		"public_key": b64(pkSPKI), "client_data_json": b64(clientData("webauthn.create", c["challenge"].(string))), "authenticator_data": b64(ad)}); code != http.StatusCreated {
		t.Fatalf("register passkey = %d %v", code, out)
	}

	// The phone key: signed in by the passkey over its own registration challenge (B50.20).
	dk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dkSPKI, _ := x509.MarshalPKIXPublicKey(&dk.PublicKey)
	signDevice := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, dk, h[:])
		return b64(sig)
	}
	_, c = w.call(http.MethodPost, "/v1/money/devices/challenge", nil)
	reg := c["challenge"].(string)
	code, dev := w.call(http.MethodPost, "/v1/money/devices", map[string]any{"platform": "test", "name": "Ada's phone", "public_key": b64(dkSPKI),
		"challenge": reg, "signature": signDevice(reg), "attestation": "", "assertion": w.signPasskey(reg)})
	if code != http.StatusCreated {
		t.Fatalf("register phone key = %d %v", code, dev)
	}
	w.deviceID = dev["id"].(string)
	w.signPhone = func(challenge string) *passkey.Assertion {
		return &passkey.Assertion{DeviceID: w.deviceID, Challenge: challenge, Signature: signDevice(challenge)}
	}
	return w
}

// B55.15 — with one passkey and one phone key, removing the passkey works, is in the audit trail, and the passkey then
// signs nothing and is no longer offered; revoking the phone key next is refused with the last-key sentence and the phone
// still signs; GET …/agents/keys lists the active keys only; two removals at once in a two-key workspace leave one key.
func TestB5515_AnyKeyButTheLastCanBeRemoved(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5515-keys"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: "app.talyvor.com", Origins: []string{"https://app.talyvor.com"}}, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountDeviceKeyRoutes(r, store)
	w := b5515NewWorkspace(t, store, r, ws)
	base := "/v1/workspaces/" + ws + "/agents"

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
	approve := func(sign func(string) *passkey.Assertion) (int, map[string]any, string) {
		t.Helper()
		held, err := store.RequestPaymentApproval(ctx, ws, agent.ID, vendor.ID, 2_000_000, "invoice", "hosting")
		if err != nil {
			t.Fatal(err)
		}
		_, c := w.call(http.MethodPost, base+"/approvals/"+held.ID+"/challenge", nil)
		code, out := w.call(http.MethodPost, base+"/approvals/"+held.ID+"/approve", map[string]any{"assertion": sign(c["challenge"].(string))})
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, held.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return code, out, status
	}

	// The passkey goes.
	if code, out := w.call(http.MethodDelete, base+"/passkeys/"+w.credID, nil); code != http.StatusOK {
		t.Fatalf("remove the passkey beside a phone key = %d %v, want 200", code, out)
	}
	var by string
	if err := pool.QueryRow(ctx, `SELECT body->>'by' FROM audit_chain WHERE workspace_id = $1 AND subject = $2 AND kind = 'key_removed'`,
		ws, w.credID).Scan(&by); err != nil || by != "jwt:user:ada" {
		t.Errorf("audit entry for the removed passkey: by %q, err %v; want jwt:user:ada", by, err)
	}
	if code, out, status := approve(w.signPasskey); code == http.StatusOK || status != "pending" {
		t.Errorf("approve signed by the removed passkey = %d %v, %s; want refused and pending", code, out, status)
	}
	if _, out := w.call(http.MethodGet, base+"/passkeys", nil); len(out["passkeys"].([]any)) != 0 {
		t.Errorf("GET passkeys after removal = %v, want none", out)
	}

	// The phone key is now the last: refused with the sentence, and it still signs.
	code, out := w.call(http.MethodPost, "/v1/money/devices/"+w.deviceID+"/revoke", nil)
	if code != http.StatusConflict || out["error"] != b5515LastKey {
		t.Errorf("revoke the last key = %d %v, want 409 %q", code, out, b5515LastKey)
	}
	if code, out, status := approve(w.signPhone); code != http.StatusOK || status != "approved" {
		t.Errorf("approve signed by the kept phone key = %d %v, %s; want 200 approved", code, out, status)
	}

	// Only active keys are listed.
	_, out = w.call(http.MethodGet, base+"/keys", nil)
	keys, _ := out["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("GET keys = %v, want the phone key alone", out)
	}
	if k := keys[0].(map[string]any); k["kind"] != "phone" || k["id"] != w.deviceID || k["name"] != "Ada's phone" || k["platform"] != "test" || k["created_at"] == nil {
		t.Errorf("GET keys[0] = %v, want the phone key", k)
	}

	// Two removals at once in a two-key workspace: exactly one passes.
	const ws2 = "ws-b5515-race"
	w2 := b5515NewWorkspace(t, store, r, ws2)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		codes[0], _ = w2.call(http.MethodDelete, "/v1/workspaces/"+ws2+"/agents/passkeys/"+w2.credID, nil)
	}()
	go func() {
		defer wg.Done()
		codes[1], _ = w2.call(http.MethodPost, "/v1/money/devices/"+w2.deviceID+"/revoke", nil)
	}()
	wg.Wait()
	var active int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1 AND removed_at IS NULL)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL)`, ws2).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 || codes[0]+codes[1] != http.StatusOK+http.StatusConflict {
		t.Errorf("two removals at once = %v, %d active keys; want one 200, one 409 and one key left", codes, active)
	}
}
