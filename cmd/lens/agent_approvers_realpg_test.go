package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/a2acard"
	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/tenant"
)

// B45.28 — the DONE line. The owner, on the Talyvor app (a device key), lets an approver agent approve a worker agent's
// requests; the approver approves one within its own limits, and the payment it approved is paid; one above them it
// cannot approve, so it waits for the owner, who approves it. The agent's approval is Talyvor-signed evidence naming
// both agents and the open mandate it ran under; the owner's is an AP2 v0.2 closed Payment Mandate. Both verify with
// the published key.
func TestAgentApprovers_ApproveWithinTheirLimitsAndAboveThemTheOwnerSignsAMandate(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4528-approvers"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 50000000, 50000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	signer := ap2.NewSigner([]byte("b45.28 test seed, thirty-two by."), "https://lens.test")
	store.SetMandateSigner(signer)
	key, _, err := kya.ParseKey("")
	if err != nil {
		t.Fatal(err)
	}
	cards := a2acard.New(store, key.Derive, "https://lens.test")
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentApproverRoutes(r, store, cards)
	mountAP2PublicRoutes(r, signer)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	keyOf := func(id string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: id, Scopes: []string{auth.ScopeProxy}}
	}
	call := func(who *auth.AuthContext, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	b64 := base64.RawURLEncoding.EncodeToString

	// The owner's phone: a device key this test holds.
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	sign := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return b64(sig)
	}
	_, ch := call(owner, http.MethodPost, "/v1/money/devices/challenge", "")
	regChallenge, _ := ch["challenge"].(string)
	code, dev := call(owner, http.MethodPost, "/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Ada's phone",
		"public_key": b64(spki), "challenge": regChallenge, "signature": sign(regChallenge), "attestation": ""}))
	if code != http.StatusCreated {
		t.Fatalf("register device = %d %v", code, dev)
	}
	deviceID := dev["id"].(string)
	assertion := func(challenge string) map[string]string {
		return map[string]string{"device_id": deviceID, "challenge": challenge, "signature": sign(challenge)}
	}

	newAgent := func(name, apiKey string, rules economy.AgentRules) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-ada")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AttachAgentKey(ctx, ws, a.ID, apiKey); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetAgentRules(ctx, ws, a.ID, rules); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	worker := newAgent("worker", "key-worker", economy.AgentRules{ApprovalAboveULXC: 1_000_000})
	approver := newAgent("approver", "key-approver", economy.AgentRules{MaxPerRequestULXC: 5_000_000})
	vendor := newAgent("vendor", "key-vendor", economy.AgentRules{})
	if _, err := store.FundAgent(ctx, ws, worker, 20_000_000); err != nil {
		t.Fatal(err)
	}

	// The owner lets the approver approve the worker's requests, signing on the phone: an open mandate.
	base := "/v1/workspaces/" + ws + "/agents/"
	if code, out := call(owner, http.MethodPut, base+worker+"/approver", jsonOf(map[string]any{"approver_agent_id": approver})); code != http.StatusForbidden {
		t.Errorf("an approver set with no signature = %d %v, want 403", code, out)
	}
	code, c := call(owner, http.MethodPost, base+worker+"/approver/challenge", jsonOf(map[string]any{"approver_agent_id": approver}))
	if code != http.StatusOK || c["limit_ulxc"] != float64(5_000_000) {
		t.Fatalf("approver challenge = %d %v, want the approver's own limit, 5 LXC", code, c)
	}
	code, set := call(owner, http.MethodPut, base+worker+"/approver", jsonOf(map[string]any{"approver_agent_id": approver,
		"assertion": assertion(c["challenge"].(string))}))
	if code != http.StatusCreated || set["approver_agent_id"] != approver || set["mandate"] == "" {
		t.Fatalf("approver set, signed = %d %v", code, set)
	}
	openMandate := set["mandate"].(string)

	// The published key, as a verifier reads it.
	code, jwks := call(nil, http.MethodGet, "/.well-known/talyvor-ap2/jwks.json", "")
	raw, _ := json.Marshal(jwks)
	var keys ap2.JWKS
	if err := json.Unmarshal(raw, &keys); code != http.StatusOK || err != nil || len(keys.Keys) != 1 {
		t.Fatalf("jwks = %d %s", code, raw)
	}
	pub := keys.Keys[0]
	open, err := ap2.Verify(openMandate, pub)
	if err != nil || open["vct"] != ap2.VCTOpen || open["cnf"] == nil {
		t.Fatalf("the open mandate = %v, %v; want an AP2 open Payment Mandate bound to the approver's key", open, err)
	}

	approve := func(who *auth.AuthContext, approvalID string) (int, map[string]any) {
		return call(who, http.MethodPost, base+approver+"/approvals/"+approvalID+"/approve", "")
	}
	status := func(id string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	paid := func() int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = $1
			AND account = 'agent:' || $2 AND kind = 'pay'`, ws, vendor).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Within the approver's limits: 3 LXC. Only the approver's own key may approve as it.
	small, err := store.RequestPaymentApproval(ctx, ws, worker, vendor, 3_000_000, "inv 1", "hosting")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := approve(keyOf("key-vendor"), small.ID); code != http.StatusForbidden || status(small.ID) != "pending" {
		t.Errorf("another agent's key approving as the approver = %d %v, want 403 and still pending", code, out)
	}
	code, got := approve(keyOf("key-approver"), small.ID)
	if code != http.StatusOK || status(small.ID) != "approved" || got["approved_by_agent_id"] != approver {
		t.Fatalf("the approver within its limits = %d %v, status %q, want 200 approved", code, got, status(small.ID))
	}
	if _, err := store.PayAgent(ctx, ws, worker, vendor, 3_000_000, "inv 1"); err != nil || paid() != 3_000_000 {
		t.Fatalf("the payment the approver approved: %v, the vendor was paid %d µLXC, want 3000000", err, paid())
	}

	// Its evidence: Talyvor-signed, naming both agents and the mandate it ran under.
	code, ev := call(owner, http.MethodGet, base+"approvals/"+small.ID+"/evidence", "")
	if code != http.StatusOK {
		t.Fatalf("evidence = %d %v", code, ev)
	}
	claims, err := ap2.VerifyJWT(ev["agent_evidence"].(string), ap2.TypEvidence, pub)
	if err != nil {
		t.Fatalf("the agent's approval evidence: %v", err)
	}
	named := func(k string) any { m, _ := claims[k].(map[string]any); return m["agent_id"] }
	mandate, _ := claims["mandate"].(map[string]any)
	if named("approver") != approver || named("requester") != worker || mandate["hash"] != ap2.Hash(openMandate) ||
		ev["approver_mandate"] != openMandate {
		t.Errorf("evidence claims = %v; want the approver, the worker and the open mandate's hash", claims)
	}

	// Above the approver's limits: 8 LXC goes to the owner.
	big, err := store.RequestPaymentApproval(ctx, ws, worker, vendor, 8_000_000, "inv 2", "hardware")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := approve(keyOf("key-approver"), big.ID); code != http.StatusConflict || status(big.ID) != "pending" {
		t.Fatalf("the approver above its limits = %d %v, status %q, want 409 and pending for the owner", code, out, status(big.ID))
	}
	if _, err := store.PayAgent(ctx, ws, worker, vendor, 8_000_000, "inv 2"); err == nil || paid() != 3_000_000 {
		t.Fatalf("the payment above the approver's limits was paid before the owner approved it: %v, %d", err, paid())
	}
	_, oc := call(owner, http.MethodPost, base+"approvals/"+big.ID+"/challenge", "")
	forged := assertion(oc["challenge"].(string))
	forged["signature"] = sign("another challenge")
	if code, _ := call(owner, http.MethodPost, base+"approvals/"+big.ID+"/approve", jsonOf(map[string]any{"assertion": forged})); code != http.StatusForbidden || status(big.ID) != "pending" {
		t.Fatalf("the owner's approval with a signature over something else = %d, status %q; want 403 and pending, no mandate", code, status(big.ID))
	}
	if code, out := call(owner, http.MethodPost, base+"approvals/"+big.ID+"/approve", jsonOf(map[string]any{
		"assertion": assertion(oc["challenge"].(string))})); code != http.StatusOK || status(big.ID) != "approved" {
		t.Fatalf("the owner approves, signed = %d %v", code, out)
	}
	code, ev = call(owner, http.MethodGet, base+"approvals/"+big.ID+"/evidence", "")
	closed, err := ap2.Verify(ev["owner_mandate"].(string), pub)
	if code != http.StatusOK || err != nil {
		t.Fatalf("the owner's mandate = %d %v: %v", code, ev, err)
	}
	amount, _ := closed["payment_amount"].(map[string]any)
	surface, _ := closed["trusted_surface"].(map[string]any)
	if closed["vct"] != ap2.VCTClosed || closed["transaction_id"] != big.ID || amount["amount"] != json.Number("8000000") ||
		surface["device_id"] != deviceID {
		t.Errorf("owner mandate claims = %v; want a closed Payment Mandate for 8 LXC, signed on the owner's phone", closed)
	}
	if _, err := store.PayAgent(ctx, ws, worker, vendor, 8_000_000, "inv 2"); err != nil || paid() != 11_000_000 {
		t.Fatalf("the payment the owner approved: %v, paid %d, want 11000000", err, paid())
	}

	// Once the owner takes the approver away, it approves nothing.
	if code, out := call(owner, http.MethodDelete, base+worker+"/approver", ""); code != http.StatusNoContent {
		t.Fatalf("remove the approver = %d %v", code, out)
	}
	third, err := store.RequestPaymentApproval(ctx, ws, worker, vendor, 2_000_000, "inv 3", "")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := approve(keyOf("key-approver"), third.ID); code != http.StatusForbidden || status(third.ID) != "pending" {
		t.Errorf("a removed approver = %d, status %q; want 403 and pending", code, status(third.ID))
	}
}
