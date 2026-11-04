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
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B41.116 — B41.16's DONE line, from talyvor-lens alone, through the routes its talyvor-suite side calls: the phone is
// lost, and one lockdown from another signed-in session revokes its device key, freezes every card and pauses every
// agent. After it an approval signed with the lost phone's key is refused, every agent is paused, every card is frozen,
// and nothing restarts — no resume, no unfreeze — until an identity check of the same person has passed since.
func TestB41116_LostPhoneLockdown(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b41116-lockdown"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCardRoutes(r, store, nil)
	mountVerificationRoutes(r, store, partners.OneVerifier(&partners.TestKYCProvider{}))
	mountLockdownRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	b64 := base64.RawURLEncoding.EncodeToString

	// Ada's phone holds a device key; her identity was checked before she lost it.
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	sign := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return b64(sig)
	}
	_, ch := call(http.MethodPost, "/v1/money/devices/challenge", "")
	regChallenge, _ := ch["challenge"].(string)
	code, dev := call(http.MethodPost, "/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Ada's phone",
		"public_key": b64(spki), "challenge": regChallenge, "signature": sign(regChallenge), "attestation": ""}))
	if code != http.StatusCreated {
		t.Fatalf("register device = %d %v", code, dev)
	}
	deviceID := dev["id"].(string)
	verification := "/v1/workspaces/" + ws + "/verification"
	if code, out := call(http.MethodPost, verification+"/contact", `{"email": "ada@example.com", "phone": "+447700900123"}`); code != http.StatusCreated {
		t.Fatalf("contact check = %d %v", code, out)
	}
	ada := `{"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"}`
	if code, out := call(http.MethodPost, verification+"/identity", ada); code != http.StatusCreated {
		t.Fatalf("identity check = %d %v", code, out)
	}

	// Two agents, the payer holding a card, and an approval waiting for Ada's signature.
	var ids []string
	for _, name := range []string{"payer", "payee"} {
		a, err := store.CreateAgent(ctx, ws, name, "user-ada")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.FundAgent(ctx, ws, a.ID, 5_000_000); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	if _, err := store.SaveAgentCard(ctx, ws, economy.AgentCard{ID: "ic_b41116", AgentID: ids[0], StripeCardholderID: "ich_b41116",
		Last4: "4242", ExpMonth: 12, ExpYear: 2030, Currency: "usd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, ids[0], economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	approval, err := store.RequestPaymentApproval(ctx, ws, ids[0], ids[1], 2_000_000, "invoice 9", "December hosting")
	if err != nil {
		t.Fatal(err)
	}
	agents := "/v1/workspaces/" + ws + "/agents"
	signedBy := func(challengePath string) string {
		t.Helper()
		_, c := call(http.MethodPost, challengePath, "")
		challenge, _ := c["challenge"].(string)
		return jsonOf(map[string]any{"assertion": map[string]string{"device_id": deviceID, "challenge": challenge, "signature": sign(challenge)}})
	}

	// The phone also holds a passkey.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, public_key) VALUES ('cred-b41116-phone', $1, '\x00')`, ws); err != nil {
		t.Fatal(err)
	}

	// The lockdown, from another session: no signature.
	code, l := call(http.MethodPost, "/v1/money/lockdown", `{"device_ids": ["`+deviceID+`"], "passkey_ids": ["cred-b41116-phone"], "reason": "lost my phone"}`)
	if revoked, _ := l["revoked_devices"].([]any); code != http.StatusOK || l["active"] != true || len(revoked) != 1 || revoked[0] != deviceID || l["frozen_cards"] != float64(1) {
		t.Fatalf("lockdown = %d %v; want 200, the phone's key revoked and one card frozen", code, l)
	}
	// The access list (B41.115) shows both of the phone's signers revoked.
	access, err := store.ConnectedAccess(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range access {
		if (it.ID == deviceID || it.ID == "cred-b41116-phone") && it.Status != "revoked" {
			t.Errorf("after lockdown the access list shows %s %s as %q; want revoked", it.Kind, it.ID, it.Status)
		}
	}
	if code, out := call(http.MethodPost, "/v1/money/lockdown", `{"device_ids": ["dev_nobody"]}`); code != http.StatusNotFound {
		t.Errorf("a lockdown naming a device the workspace never had = %d %v; want 404", code, out)
	}

	// An approval signed with the lost phone's key is refused; every agent is paused; every card is frozen.
	if code, out := call(http.MethodPost, agents+"/approvals/"+approval.ID+"/approve", signedBy(agents+"/approvals/"+approval.ID+"/challenge")); code != http.StatusForbidden {
		t.Errorf("approval signed by the lost phone = %d %v; want 403", code, out)
	}
	var status string
	var allPaused, frozen bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT status FROM agent_approvals WHERE id = $1),
		EXISTS (SELECT 1 FROM agent_workspace_pauses WHERE workspace_id = $2), (SELECT frozen FROM agent_cards WHERE agent_id = $3)`,
		approval.ID, ws, ids[0]).Scan(&status, &allPaused, &frozen); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || !allPaused || !frozen {
		t.Fatalf("after lockdown: approval %q, every agent paused %v, card frozen %v; want pending, true, true", status, allPaused, frozen)
	}
	if code, _ := call(http.MethodPost, agents+"/"+ids[0]+"/pay", `{"to_agent_id":"`+ids[1]+`","amount_ulxc":1000}`); code != http.StatusForbidden {
		t.Errorf("a payment during lockdown = %d; want 403", code)
	}

	// Nothing restarts, and the lockdown does not lift, without a new identity check of Ada.
	unfreeze := agents + "/" + ids[0] + "/card/unfreeze"
	if code, out := call(http.MethodPost, unfreeze, ""); code != http.StatusForbidden {
		t.Errorf("unfreeze during lockdown = %d %v; want 403", code, out)
	}
	if code, out := call(http.MethodPost, agents+"/resume-all", ""); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "locked down") {
		t.Errorf("resume-all during lockdown = %d %v; want 403 locked down", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/lockdown/lift", ""); code != http.StatusForbidden {
		t.Errorf("lift with no new identity check = %d %v; want 403", code, out)
	}
	if code, out := call(http.MethodPost, verification+"/identity", `{"name": "Mallory Thief", "country": "GB", "date_of_birth": "1991-01-01"}`); code != http.StatusCreated {
		t.Fatalf("someone else's identity check = %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/lockdown/lift", ""); code != http.StatusForbidden {
		t.Errorf("lift after someone else's identity check = %d %v; want 403", code, out)
	}
	if code, out := call(http.MethodPost, verification+"/identity", ada); code != http.StatusCreated {
		t.Fatalf("Ada's new identity check = %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/lockdown/lift", ""); code != http.StatusOK {
		t.Fatalf("lift after Ada's new identity check = %d %v; want 200", code, out)
	}
	if code, out := call(http.MethodGet, "/v1/money/lockdown", ""); code != http.StatusOK || out["active"] != false {
		t.Errorf("lockdown after lifting = %d %v; want inactive", code, out)
	}
	if code, out := call(http.MethodPost, unfreeze, ""); code != http.StatusOK || out["frozen"] != false {
		t.Errorf("unfreeze after lifting = %d %v; want 200, unfrozen", code, out)
	}
	if code, out := call(http.MethodPost, agents+"/approvals/"+approval.ID+"/approve", signedBy(agents+"/approvals/"+approval.ID+"/challenge")); code == http.StatusOK {
		t.Errorf("the lost phone's key signs again after the lift: %d %v", code, out)
	}
}

// B41.116 review: a lockdown of every key leaves the workspace no live signer. Lifting it, after a new identity check,
// lets the owner enrol one new passkey with no other key's signature, and that passkey resumes the agents; before the
// lift no key enrols that way, and after the first one the next needs its signature again.
func TestB41116_FullLockdownReEnrolsOneKeyAfterTheLift(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b41116-reenrol", "app.talyvor.com", "https://app.talyvor.com"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountVerificationRoutes(r, store, partners.OneVerifier(&partners.TestKYCProvider{}))
	mountLockdownRoutes(r, store)
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
	agents := "/v1/workspaces/" + ws + "/agents"
	enrol := func(a authenticator, signer *authenticator) (int, map[string]any) {
		t.Helper()
		_, c := call(agents+"/passkeys/challenge", nil)
		body := a.register(c["challenge"].(string))
		if signer != nil {
			body["assertion"] = signer.sign(c["challenge"].(string))
		}
		return call(agents+"/passkeys", body)
	}
	verification := "/v1/workspaces/" + ws + "/verification"
	ada := map[string]string{"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"}
	if code, out := call(verification+"/contact", map[string]string{"email": "ada@example.com", "phone": "+447700900123"}); code != http.StatusCreated {
		t.Fatalf("contact check = %d %v", code, out)
	}
	if code, out := call(verification+"/identity", ada); code != http.StatusCreated {
		t.Fatalf("identity check = %d %v", code, out)
	}
	phone := newAuthenticator("Ada's phone")
	if code, out := enrol(phone, nil); code != http.StatusCreated {
		t.Fatalf("registering the phone's passkey = %d %v", code, out)
	}
	if _, err := store.CreateAgent(ctx, ws, "researcher", "user-ada"); err != nil {
		t.Fatal(err)
	}

	// The lockdown names no key, so it revokes every one: no live signer is left.
	if code, out := call("/v1/money/lockdown", map[string]string{"reason": "lost my phone"}); code != http.StatusOK {
		t.Fatalf("lockdown = %d %v", code, out)
	}
	laptop := newAuthenticator("Ada's laptop")
	if code, out := enrol(laptop, nil); code != http.StatusForbidden {
		t.Fatalf("enrolling a key during the lockdown with no signature = %d %v; want 403", code, out)
	}
	if code, out := call(verification+"/identity", ada); code != http.StatusCreated {
		t.Fatalf("Ada's new identity check = %d %v", code, out)
	}
	if code, out := call("/v1/money/lockdown/lift", nil); code != http.StatusOK {
		t.Fatalf("lift = %d %v", code, out)
	}

	// After the lift, one new passkey enrols with no signature, is on the audit chain, and resumes the agents.
	if code, out := enrol(laptop, nil); code != http.StatusCreated {
		t.Fatalf("enrolling the first key after the lift = %d %v; want 201", code, out)
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_chain WHERE workspace_id = $1 AND subject = $2 AND kind = 'key_enrolled_after_lockdown'`,
		ws, "lockdown:"+ws).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audit entries for the re-enrolment = %d (%v); want 1", audited, err)
	}
	if code, out := enrol(newAuthenticator("Mallory's"), nil); code != http.StatusForbidden {
		t.Fatalf("a second key with no signature after the lift = %d %v; want 403", code, out)
	}
	_, c := call(agents+"/resume-all/challenge", nil)
	if code, out := call(agents+"/resume-all", map[string]any{"assertion": laptop.sign(c["challenge"].(string))}); code != http.StatusOK {
		t.Fatalf("resume-all signed by the new passkey = %d %v; want 200", code, out)
	}
	var paused bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_workspace_pauses WHERE workspace_id = $1)`, ws).Scan(&paused); err != nil || paused {
		t.Fatalf("every agent still paused after resume-all: %v %v", paused, err)
	}
}
