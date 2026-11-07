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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B55.1 — a key registered with no signature in a workspace that already has one waits, signs nothing, and is let in
// by an existing key of the workspace (passkey or phone) over its enrolment's own challenge; declined, expired, a second
// approval, a revoked key or another workspace's key let nothing in.
type b551Harness struct {
	t        *testing.T
	ws       string
	call     func(method, path string, body any) (int, map[string]any)
	store    *economy.DualTokenStore
	pool     *pgxpool.Pool
	agentID  string
	vendorID string
	countOf  func(table string) int
}

type b551Passkey struct {
	register func(challenge string) map[string]any
	sign     func(challenge string) *passkey.Assertion
	spki     []byte
}

type b551Phone struct {
	key  *ecdsa.PrivateKey
	spki []byte
	id   string
}

const b551RP, b551Origin = "app.talyvor.com", "https://app.talyvor.com"

func newB551Harness(t *testing.T, ws string) *b551Harness {
	pool := agentRoutesDB(t)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: b551RP, Origins: []string{b551Origin}}, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountDeviceKeyRoutes(r, store)
	mountKeyEnrolmentRoutes(r, store)
	h := &b551Harness{t: t, ws: ws, store: store, pool: pool}
	h.call = func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		owner := &auth.AuthContext{WorkspaceID: h.ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	h.countOf = func(table string) (n int) {
		t.Helper()
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, h.ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return h
}

func (h *b551Harness) base() string { return "/v1/workspaces/" + h.ws + "/agents" }

func (h *b551Harness) challenge(path string) string {
	h.t.Helper()
	code, c := h.call(http.MethodPost, path, nil)
	if code != http.StatusOK {
		h.t.Fatalf("%s = %d %v", path, code, c)
	}
	return c["challenge"].(string)
}

func newB551Passkey() b551Passkey {
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(b551RP))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + b551Origin + `"}`)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	counter := uint32(0)
	return b551Passkey{
		spki: spki,
		register: func(challenge string) map[string]any {
			ad := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
			ad = append(append(binary.BigEndian.AppendUint16(append(ad, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
			return map[string]any{"credential_id": b64(credID), "name": "Ada's laptop", "public_key": b64(spki),
				"client_data_json": b64(clientData("webauthn.create", challenge)), "authenticator_data": b64(ad)}
		},
		sign: func(challenge string) *passkey.Assertion {
			counter++
			cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
			hc := sha256.Sum256(cdj)
			digest := sha256.Sum256(append(append([]byte{}, ad...), hc[:]...))
			sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
			return &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
		},
	}
}

func newB551Phone() *b551Phone {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return &b551Phone{key: key, spki: spki}
}

func (p *b551Phone) sig(challenge string) string {
	h := sha256.Sum256(devicekey.Message(challenge))
	s, _ := ecdsa.SignASN1(rand.Reader, p.key, h[:])
	return base64.RawURLEncoding.EncodeToString(s)
}

func (p *b551Phone) sign(challenge string) *passkey.Assertion {
	return &passkey.Assertion{DeviceID: p.id, Challenge: challenge, Signature: p.sig(challenge)}
}

// registerPasskey registers k over a fresh challenge, unsigned.
func (h *b551Harness) registerPasskey(k b551Passkey) (int, map[string]any) {
	return h.call(http.MethodPost, h.base()+"/passkeys", k.register(h.challenge(h.base()+"/passkeys/challenge")))
}

// registerPhone registers p over a fresh challenge, unsigned; a 201 sets p.id.
func (h *b551Harness) registerPhone(p *b551Phone) (int, map[string]any) {
	c := h.challenge("/v1/money/devices/challenge")
	code, out := h.call(http.MethodPost, "/v1/money/devices", map[string]any{"platform": "test", "name": "Ada's phone",
		"public_key": base64.RawURLEncoding.EncodeToString(p.spki), "challenge": c, "signature": p.sig(c), "attestation": ""})
	if code == http.StatusCreated {
		p.id = out["id"].(string)
	}
	return code, out
}

// waiting registers through reg and wants a 202 waiting enrolment whose code is the one the new key's public key gives.
func (h *b551Harness) waiting(reg func() (int, map[string]any), spki []byte) string {
	h.t.Helper()
	code, out := reg()
	enr, _ := out["enrolment"].(map[string]any)
	if code != http.StatusAccepted || enr == nil {
		h.t.Fatalf("registering a key unsigned in a workspace with a key = %d %v, want 202 with an enrolment", code, out)
	}
	sum := sha256.Sum256(spki)
	if want := fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1_000_000); enr["code"] != want {
		h.t.Fatalf("enrolment code = %v, want %s from the new key's public key", enr["code"], want)
	}
	return enr["id"].(string)
}

func (h *b551Harness) approveEnrolment(id string, signer func(string) *passkey.Assertion) (int, map[string]any) {
	h.t.Helper()
	path := h.base() + "/keys/enrolments/" + id
	return h.call(http.MethodPost, path+"/approve", map[string]any{"assertion": signer(h.challenge(path + "/challenge"))})
}

// approvalSignedBy files a payment needing approval and approves it signed by signer; the status Lens answers and the
// approval's row after.
func (h *b551Harness) approvalSignedBy(signer func(string) *passkey.Assertion) (int, string) {
	h.t.Helper()
	ctx := context.Background()
	if h.agentID == "" {
		agent, err := h.store.CreateAgent(ctx, h.ws, "researcher", "user-ada")
		if err != nil {
			h.t.Fatal(err)
		}
		vendor, err := h.store.CreateAgent(ctx, h.ws, "vendor", "user-ada")
		if err != nil {
			h.t.Fatal(err)
		}
		if _, err := h.store.SetAgentRules(ctx, h.ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
			h.t.Fatal(err)
		}
		h.agentID, h.vendorID = agent.ID, vendor.ID
	}
	held, err := h.store.RequestPaymentApproval(ctx, h.ws, h.agentID, h.vendorID, 2_000_000, "invoice 7", "October hosting")
	if err != nil {
		h.t.Fatal(err)
	}
	path := h.base() + "/approvals/" + held.ID
	code, _ := h.call(http.MethodPost, path+"/approve", map[string]any{"assertion": signer(h.challenge(path + "/challenge"))})
	var status string
	if err := h.pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, held.ID).Scan(&status); err != nil {
		h.t.Fatal(err)
	}
	return code, status
}

// (1) Only a phone key: a passkey registered unsigned waits, cannot sign an approval, is let in by the phone over its
// enrolment's challenge, then signs one.
func TestKeyEnrolment_PasskeyJoinsFromThePhone(t *testing.T) {
	h := newB551Harness(t, "ws-b551-passkey-joins")
	phone := newB551Phone()
	if code, out := h.registerPhone(phone); code != http.StatusCreated {
		t.Fatalf("the workspace's first key, a phone = %d %v, want 201", code, out)
	}
	laptop := newB551Passkey()
	id := h.waiting(func() (int, map[string]any) { return h.registerPasskey(laptop) }, laptop.spki)
	if h.countOf("workspace_passkeys") != 0 {
		t.Fatal("a waiting passkey is a key of the workspace")
	}
	if code, out := h.registerPasskey(laptop); code != http.StatusConflict {
		t.Fatalf("a second waiting key with the same code = %d %v, want 409", code, out)
	}
	if code, out := h.call(http.MethodGet, h.base()+"/keys/enrolments", nil); code != http.StatusOK || len(out["enrolments"].([]any)) != 1 {
		t.Fatalf("listing waiting enrolments = %d %v, want the one", code, out)
	}
	if code, status := h.approvalSignedBy(laptop.sign); code == http.StatusOK || status != "pending" {
		t.Fatalf("approving a payment with the waiting passkey = %d %q, want refused and still pending", code, status)
	}
	if code, out := h.approveEnrolment(id, phone.sign); code != http.StatusOK || h.countOf("workspace_passkeys") != 1 {
		t.Fatalf("the phone letting the passkey in = %d %v, %d passkeys; want 200 and one", code, out, h.countOf("workspace_passkeys"))
	}
	if code, status := h.approvalSignedBy(laptop.sign); code != http.StatusOK || status != "approved" {
		t.Fatalf("approving a payment with the passkey once let in = %d %q, want 200 approved", code, status)
	}
}

// (2) The mirror: a passkey first, and a phone key waiting until the passkey lets it in.
func TestKeyEnrolment_PhoneJoinsFromThePasskey(t *testing.T) {
	h := newB551Harness(t, "ws-b551-phone-joins")
	laptop := newB551Passkey()
	if code, out := h.registerPasskey(laptop); code != http.StatusCreated {
		t.Fatalf("the workspace's first key, a passkey = %d %v, want 201", code, out)
	}
	phone := newB551Phone()
	id := h.waiting(func() (int, map[string]any) { return h.registerPhone(phone) }, phone.spki)
	if h.countOf("workspace_device_keys") != 0 {
		t.Fatal("a waiting phone key is a key of the workspace")
	}
	if code, out := h.approveEnrolment(id, laptop.sign); code != http.StatusOK || h.countOf("workspace_device_keys") != 1 {
		t.Fatalf("the passkey letting the phone in = %d %v, %d phone keys; want 200 and one", code, out, h.countOf("workspace_device_keys"))
	}
	_, out := h.call(http.MethodGet, "/v1/money/devices", nil)
	phone.id = out["devices"].([]any)[0].(map[string]any)["id"].(string)
	if code, status := h.approvalSignedBy(phone.sign); code != http.StatusOK || status != "approved" {
		t.Fatalf("approving a payment with the phone once let in = %d %q, want 200 approved", code, status)
	}
}

// (3) A revoked key, another workspace's key, an expired enrolment and a second approval let nothing in; (4) a declined
// or expired one never signs. At most 3 wait at once.
func TestKeyEnrolment_RefusedApprovals(t *testing.T) {
	h := newB551Harness(t, "ws-b551-refused")
	phone, spare := newB551Phone(), newB551Passkey()
	if code, out := h.registerPhone(phone); code != http.StatusCreated {
		t.Fatalf("first key = %d %v", code, out)
	}
	// Another workspace's passkey, its own first key there.
	other := newB551Passkey()
	h.ws = "ws-b551-other"
	if code, out := h.registerPasskey(other); code != http.StatusCreated {
		t.Fatalf("the other workspace's first key = %d %v", code, out)
	}
	h.ws = "ws-b551-refused"

	byOther, revokedFor, expired := newB551Passkey(), newB551Passkey(), newB551Passkey()
	idOther := h.waiting(func() (int, map[string]any) { return h.registerPasskey(byOther) }, byOther.spki)
	if code, _ := h.approveEnrolment(idOther, other.sign); code == http.StatusOK {
		t.Fatal("another workspace's passkey let a key in")
	}
	idExpired := h.waiting(func() (int, map[string]any) { return h.registerPasskey(expired) }, expired.spki)
	idRevoked := h.waiting(func() (int, map[string]any) { return h.registerPasskey(revokedFor) }, revokedFor.spki)
	if code, out := h.registerPasskey(spare); code != http.StatusConflict {
		t.Fatalf("a fourth waiting enrolment = %d %v, want 409", code, out)
	}
	// Each challenge issued while the enrolment still waited, so the refusal is the approval's own.
	path := func(id string) string { return h.base() + "/keys/enrolments/" + id }
	cExpired, cDeclined, cTwice := h.challenge(path(idExpired)+"/challenge"), h.challenge(path(idOther)+"/challenge"), h.challenge(path(idRevoked)+"/challenge")
	if _, err := h.pool.Exec(context.Background(), `UPDATE key_enrolments SET expires_at = now() - interval '1 second' WHERE id = $1`, idExpired); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.call(http.MethodPost, path(idExpired)+"/approve", map[string]any{"assertion": phone.sign(cExpired)}); code != http.StatusNotFound {
		t.Fatalf("approving an expired enrolment = %d, want 404", code)
	}
	if code, _ := h.call(http.MethodPost, path(idOther)+"/decline", nil); code != http.StatusOK {
		t.Fatalf("declining = %d, want 200", code)
	}
	if code, _ := h.call(http.MethodPost, path(idOther)+"/approve", map[string]any{"assertion": phone.sign(cDeclined)}); code != http.StatusNotFound {
		t.Fatalf("approving a declined enrolment = %d, want 404", code)
	}
	if code, out := h.approveEnrolment(idRevoked, phone.sign); code != http.StatusOK {
		t.Fatalf("approving = %d %v, want 200", code, out)
	}
	if code, _ := h.call(http.MethodPost, path(idRevoked)+"/approve", map[string]any{"assertion": phone.sign(cTwice)}); code != http.StatusNotFound {
		t.Fatalf("approving an enrolment a second time = %d, want 404", code)
	}
	// The phone revoked: it lets in no one.
	late := newB551Passkey()
	idLate := h.waiting(func() (int, map[string]any) { return h.registerPasskey(late) }, late.spki)
	c := h.challenge(path(idLate) + "/challenge")
	if code, out := h.call(http.MethodPost, "/v1/money/devices/"+phone.id+"/revoke", nil); code != http.StatusOK {
		t.Fatalf("revoking the phone = %d %v", code, out)
	}
	if code, _ := h.call(http.MethodPost, path(idLate)+"/approve", map[string]any{"assertion": phone.sign(c)}); code == http.StatusOK {
		t.Fatal("a revoked phone let a key in")
	}
	if n := h.countOf("workspace_passkeys"); n != 1 {
		t.Fatalf("%d passkeys, want only the one approved", n)
	}
	for _, k := range []b551Passkey{byOther, expired, late} {
		if code, status := h.approvalSignedBy(k.sign); code == http.StatusOK || status != "pending" {
			t.Fatalf("a key never let in approved a payment = %d %q", code, status)
		}
	}
}
