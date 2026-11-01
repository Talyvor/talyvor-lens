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

	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B41.204 — one passkey assertion over five approval ids approves all five, each exactly once with the owner's closed
// mandate; replaying it decides nothing, an assertion over a different set is refused, and a set where one is no
// longer pending is 409 with none decided.
func TestB41204_OnePasskeyAssertionDecidesSeveralApprovals(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-agents", "app.talyvor.com", "https://app.talyvor.com"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetMandateSigner(ap2.NewSigner([]byte("b41.204 test seed, thirty-two b."), "https://lens.test"))
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m
	}
	b64 := base64.RawURLEncoding.EncodeToString
	base := "/v1/workspaces/" + ws + "/agents"

	// The owner registers a passkey; this test is the authenticator.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	rpHash := sha256.Sum256([]byte(rpID))
	authData := func(flags byte, count uint32, attested bool) []byte {
		d := append(append([]byte{}, rpHash[:]...), flags)
		d = binary.BigEndian.AppendUint32(d, count)
		if attested {
			d = append(d, make([]byte, 16)...)
			d = binary.BigEndian.AppendUint16(d, uint16(len(credID)))
			d = append(append(d, credID...), 0xa0)
		}
		return d
	}
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `","crossOrigin":false}`)
	}
	_, ch := call(base+"/passkeys/challenge", nil)
	if code, body := call(base+"/passkeys", map[string]string{"credential_id": b64(credID), "name": "phone", "public_key": b64(spki),
		"client_data_json": b64(clientData("webauthn.create", ch["challenge"].(string))), "authenticator_data": b64(authData(0x45, 0, true))}); code != http.StatusCreated {
		t.Fatalf("register passkey = %d %v", code, body)
	}
	count := uint32(0)
	sign := func(challenge string) map[string]string {
		count++
		cdj := clientData("webauthn.get", challenge)
		ad := authData(0x05, count, false)
		h := sha256.Sum256(cdj)
		signed := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, signed[:])
		return map[string]string{"credential_id": b64(credID), "client_data_json": b64(cdj), "authenticator_data": b64(ad), "signature": b64(sig)}
	}

	agent, err := store.CreateAgent(ctx, ws, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "vendor", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range 7 {
		a, err := store.RequestPaymentApproval(ctx, ws, agent.ID, payee.ID, int64(2_000_000+i), "invoice", "hosting")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	five, others := ids[:5], ids[5:]
	status := func(id string) (s string, mandate bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, owner_mandate IS NOT NULL AND owner_mandate <> '' FROM agent_approvals WHERE id = $1`, id).Scan(&s, &mandate); err != nil {
			t.Fatal(err)
		}
		return s, mandate
	}

	// No assertion: refused.
	if code, body := call(base+"/approvals/decide", map[string]any{"approval_ids": five, "decision": "approve"}); code != http.StatusForbidden {
		t.Fatalf("decide with no assertion = %d %v, want 403", code, body)
	}
	code, ch := call(base+"/approvals/challenge", map[string]any{"approval_ids": five, "decision": "approve"})
	if code != http.StatusOK || ch["user_verification"] != "required" {
		t.Fatalf("challenge = %d %v", code, ch)
	}
	assertion := sign(ch["challenge"].(string))

	// The same assertion offered for a different set — one id swapped — is refused and decides nothing.
	changed := append(append([]string{}, five[:4]...), others[0])
	if code, body := call(base+"/approvals/decide", map[string]any{"approval_ids": changed, "decision": "approve", "assertion": assertion}); code != http.StatusForbidden {
		t.Fatalf("decide over a changed set = %d %v, want 403", code, body)
	}
	// …and for the other decision.
	if code, body := call(base+"/approvals/decide", map[string]any{"approval_ids": five, "decision": "deny", "assertion": assertion}); code != http.StatusForbidden {
		t.Fatalf("deny with an approve assertion = %d %v, want 403", code, body)
	}
	for _, id := range ids {
		if s, _ := status(id); s != "pending" {
			t.Fatalf("approval %s is %s after refused decisions, want pending", id, s)
		}
	}

	// The five, in another order: all approved, each once, each with the owner's closed mandate.
	reversed := []string{five[4], five[3], five[2], five[1], five[0]}
	code, out := call(base+"/approvals/decide", map[string]any{"approval_ids": reversed, "decision": "approve", "assertion": assertion})
	if code != http.StatusOK {
		t.Fatalf("decide = %d %v", code, out)
	}
	if got, _ := out["approvals"].([]any); len(got) != 5 {
		t.Fatalf("decide returned %d approvals, want 5: %v", len(got), out)
	}
	for _, id := range five {
		if s, m := status(id); s != "approved" || !m {
			t.Errorf("approval %s: status %s, mandate %v; want approved with the owner's mandate", id, s, m)
		}
	}

	// Replaying the assertion decides nothing.
	if code, body := call(base+"/approvals/decide", map[string]any{"approval_ids": five, "decision": "approve", "assertion": assertion}); code != http.StatusForbidden {
		t.Fatalf("replay = %d %v, want 403", code, body)
	}

	// A challenge naming an approval already decided is 404.
	if code, body := call(base+"/approvals/challenge", map[string]any{"approval_ids": []string{five[0], others[0]}, "decision": "deny"}); code != http.StatusNotFound {
		t.Fatalf("challenge naming a decided approval = %d %v, want 404", code, body)
	}
	// One of a signed set decided in between: 409, and the other is left pending.
	_, ch = call(base+"/approvals/challenge", map[string]any{"approval_ids": others, "decision": "deny"})
	a := sign(ch["challenge"].(string))
	if _, err := pool.Exec(ctx, `UPDATE agent_approvals SET status = 'denied' WHERE id = $1`, others[0]); err != nil {
		t.Fatal(err)
	}
	if code, body := call(base+"/approvals/decide", map[string]any{"approval_ids": others, "decision": "deny", "assertion": a}); code != http.StatusConflict {
		t.Fatalf("decide with one no longer pending = %d %v, want 409", code, body)
	}
	if s, _ := status(others[1]); s != "pending" {
		t.Errorf("approval %s is %s after a 409, want pending", others[1], s)
	}
}
