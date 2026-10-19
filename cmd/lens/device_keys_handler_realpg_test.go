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
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B40.4 — a test device key registers, signs an agent's approval and a permission mandate that Lens accepts; once it is
// revoked, the same kind of signature on a new approval is refused, the approval still needs a signature, and the
// mandate it signed is blocked.
func TestDeviceKeys_SignApprovalsAndMandatesUntilRevoked(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b404-devices"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountPermissionMandateRoutes(r, store)
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

	// The phone: this test holds its key.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sign := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, h[:])
		return b64(sig)
	}
	_, ch := call(http.MethodPost, "/v1/money/devices/challenge", "")
	regChallenge, _ := ch["challenge"].(string)
	code, dev := call(http.MethodPost, "/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Ada's phone",
		"public_key": b64(spki), "challenge": regChallenge, "signature": sign(regChallenge), "attestation": ""}))
	if code != http.StatusCreated || dev["platform"] != "test" || dev["person"] != "jwt:user:ada" {
		t.Fatalf("register device = %d %v", code, dev)
	}
	deviceID := dev["id"].(string)
	assertion := func(challenge string) map[string]string {
		return map[string]string{"device_id": deviceID, "challenge": challenge, "signature": sign(challenge)}
	}

	agent, err := store.CreateAgent(ctx, ws, "researcher", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "vendor", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/approvals/"
	decide := func(approvalID string, signed bool) (int, map[string]any) {
		t.Helper()
		_, c := call(http.MethodPost, base+approvalID+"/challenge", "")
		body := `{}`
		if signed {
			body = jsonOf(map[string]any{"assertion": assertion(c["challenge"].(string))})
		}
		return call(http.MethodPost, base+approvalID+"/approve", body)
	}
	status := func(id string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	first, err := store.RequestPaymentApproval(ctx, ws, agent.ID, payee.ID, 2_000_000, "invoice 7", "October hosting")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := decide(first.ID, false); code != http.StatusForbidden {
		t.Errorf("approve with no signature once a device key is registered = %d %v, want 403", code, out)
	}
	if code, out := decide(first.ID, true); code != http.StatusOK || status(first.ID) != "approved" {
		t.Fatalf("approve signed by the device = %d %v, status %q, want 200 approved", code, out, status(first.ID))
	}

	// The device signs a mandate; Lens keeps it and evaluates under it.
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	code, prep := call(http.MethodPost, "/v1/money/permission-mandates/prepare", `{"principal":{"type":"person","name":"Ada Lovelace"},
		"clients":["chat"],"scope":{"payees":["payee-plumber"],"currencies":["GBP"]},"limits":{"per_payment":{"GBP":10000}},
		"new_payee":{"policy":"always_ask"},"expires_at":"`+expires+`"}`)
	if code != http.StatusOK {
		t.Fatalf("prepare with only a device key = %d %v", code, prep)
	}
	code, m := call(http.MethodPost, "/v1/money/permission-mandates", jsonOf(map[string]any{"canonical": prep["canonical"],
		"assertion": assertion(prep["challenge"].(string))}))
	if code != http.StatusCreated || m["device_id"] != deviceID {
		t.Fatalf("mandate signed by the device = %d %v", code, m)
	}
	evaluate := func() map[string]any {
		t.Helper()
		_, d := call(http.MethodPost, "/v1/money/permission-mandates/"+m["id"].(string)+"/evaluate",
			`{"client":"chat","payee_id":"payee-plumber","currency":"GBP","amount_minor":9000}`)
		return d
	}
	if d := evaluate(); d["decision"] != economy.MandateAllow {
		t.Fatalf("£90 under the device-signed mandate = %v, want allow", d)
	}

	// Revoked: from now on its signatures are refused.
	if code, out := call(http.MethodPost, "/v1/money/devices/"+deviceID+"/revoke", ""); code != http.StatusOK || out["revoked_at"] == nil {
		t.Fatalf("revoke = %d %v", code, out)
	}
	second, err := store.RequestPaymentApproval(ctx, ws, agent.ID, payee.ID, 3_000_000, "invoice 8", "November hosting")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := decide(second.ID, true); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "revoked") {
		t.Errorf("approve signed by the revoked device = %d %v, want 403", code, out)
	}
	if code, out := decide(second.ID, false); code != http.StatusForbidden {
		t.Errorf("approve with no signature after the only device was revoked = %d %v, want 403", code, out)
	}
	if s := status(second.ID); s != "pending" {
		t.Errorf("after refused decisions the approval is %q, want pending", s)
	}
	if d := evaluate(); d["decision"] != economy.MandateBlock || d["rule"] != "signature" {
		t.Errorf("the mandate the revoked device signed = %v, want block by signature", d)
	}
}
