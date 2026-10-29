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

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B41.108 — B41.8's DONE line: pausing an agent needs no passkey; resuming it without a passkey signature is refused
// by Lens and the agent stays paused. The owner's phone holds a device key; one tap pauses an agent, and only the
// phone's signature over the resume challenge starts it again. The same for pause-all and resume-all.
func TestB41108_PauseIsOneTapResumeTakesTheOwnersSignature(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b41108-resume"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
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
	signed := func(challengePath string) string {
		t.Helper()
		code, c := call(http.MethodPost, challengePath, "")
		if code != http.StatusOK {
			t.Fatalf("%s = %d %v", challengePath, code, c)
		}
		challenge := c["challenge"].(string)
		return jsonOf(map[string]any{"assertion": map[string]string{"device_id": dev["id"].(string), "challenge": challenge, "signature": sign(challenge)}})
	}

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
	base := "/v1/workspaces/" + ws + "/agents"
	pay := `{"to_agent_id":"` + ids[1] + `","amount_ulxc":1000}`
	payerPaused := func() bool {
		var paused bool
		if err := pool.QueryRow(ctx, `SELECT paused_at IS NOT NULL FROM agent_accounts WHERE id = $1`, ids[0]).Scan(&paused); err != nil {
			t.Fatal(err)
		}
		return paused
	}

	// One agent: pause with no signature, a bare resume refused, a signed one lets it pay again.
	if code, out := call(http.MethodPost, base+"/"+ids[0]+"/pause", `{"reason":"looks wrong"}`); code != http.StatusOK {
		t.Fatalf("pause = %d %v, want 200 with no signature", code, out)
	}
	if code, out := call(http.MethodPost, base+"/"+ids[0]+"/resume", ``); code != http.StatusForbidden || !payerPaused() {
		t.Fatalf("resume with no signature = %d %v, paused %v; want 403 and still paused", code, out, payerPaused())
	}
	// A signature over the resume-all challenge does not resume one agent.
	if code, out := call(http.MethodPost, base+"/"+ids[0]+"/resume", signed(base+"/resume-all/challenge")); code != http.StatusForbidden || !payerPaused() {
		t.Fatalf("resume signed over another challenge = %d %v, want 403 and still paused", code, out)
	}
	if code, _ := call(http.MethodPost, base+"/"+ids[0]+"/pay", pay); code != http.StatusForbidden {
		t.Errorf("a paused agent paid: %d, want 403", code)
	}
	if code, out := call(http.MethodPost, base+"/"+ids[0]+"/resume", signed(base+"/"+ids[0]+"/resume/challenge")); code != http.StatusOK || payerPaused() {
		t.Fatalf("signed resume = %d %v, want 200 and resumed", code, out)
	}
	if code, out := call(http.MethodPost, base+"/"+ids[0]+"/pay", pay); code != http.StatusOK {
		t.Errorf("a payment after the signed resume = %d %v, want 200", code, out)
	}

	// The whole fleet: the same asymmetry.
	if code, out := call(http.MethodPost, base+"/pause-all", `{"reason":"incident"}`); code != http.StatusOK {
		t.Fatalf("pause-all = %d %v, want 200 with no signature", code, out)
	}
	if code, out := call(http.MethodPost, base+"/resume-all", ``); code != http.StatusForbidden {
		t.Fatalf("resume-all with no signature = %d %v, want 403", code, out)
	}
	if code, _ := call(http.MethodPost, base+"/"+ids[0]+"/pay", pay); code != http.StatusForbidden {
		t.Errorf("a payment while every agent stays paused = %d, want 403", code)
	}
	if code, out := call(http.MethodPost, base+"/resume-all", signed(base+"/resume-all/challenge")); code != http.StatusOK {
		t.Fatalf("signed resume-all = %d %v, want 200", code, out)
	}
	if code, out := call(http.MethodPost, base+"/"+ids[0]+"/pay", pay); code != http.StatusOK {
		t.Errorf("a payment after the signed resume-all = %d %v, want 200", code, out)
	}
}
