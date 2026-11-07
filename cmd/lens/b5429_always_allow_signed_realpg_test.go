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

// B54.29 — with a phone key in the workspace, approving with always_allow is signed over the allowance itself: a
// signature over the approval alone, or over a smaller allowance, approves nothing and saves no rule; signed over that
// allowance's own challenge, both are saved. And a boost the rules refuse leaves its signature unspent, so the same
// signed boost saves once the rules let it.
func TestB5429_AlwaysAllowIsSignedAndAFailedBoostKeepsItsSignature(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5429"
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
	payer, err := store.CreateAgent(ctx, ws, "payer", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := store.CreateAgent(ctx, ws, "supplier", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, payer.ID, 15_000_000); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/"
	if code, out := call(http.MethodPut, base+payer.ID+"/rules", `{"approval_above_ulxc":2000000,"daily_limit_ulxc":5000000}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %v", code, out)
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
	signed := func(path, body string) map[string]string {
		t.Helper()
		code, c := call(http.MethodPost, path, body)
		challenge, _ := c["challenge"].(string)
		if code != http.StatusOK || challenge == "" {
			t.Fatalf("challenge %s %s = %d %v", path, body, code, c)
		}
		return map[string]string{"device_id": dev["id"].(string), "challenge": challenge, "signature": sign(challenge)}
	}

	// A payment above the approval amount waits for approval.
	if code, out := call(http.MethodPost, base+payer.ID+"/pay", `{"to_agent_id":"`+supplier.ID+`","amount_ulxc":3000000}`); code != http.StatusForbidden {
		t.Fatalf("pay = %d %v, want 403 for approval", code, out)
	}
	var approvalID string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE workspace_id = $1 AND status = 'pending'`, ws).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	approve := base + "approvals/" + approvalID + "/approve"
	challenge := base + "approvals/" + approvalID + "/challenge"
	tenMonth := map[string]any{"amount_ulxc": 10_000_000, "per": "month"}
	// state is the approval's status and the supplier's allowance as saved.
	state := func() (string, economy.PayeeAllowance) {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, approvalID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		rules, err := store.GetAgentRules(ctx, ws, payer.ID)
		if err != nil {
			t.Fatal(err)
		}
		return status, rules.PayeeAllowances[supplier.ID]
	}

	// Signed over the approval alone, always_allow approves nothing and saves no rule.
	code, out := call(http.MethodPost, approve, jsonOf(map[string]any{"assertion": signed(challenge, ""), "always_allow": tenMonth}))
	if status, allow := state(); code != http.StatusForbidden || status != "pending" || allow.ULXC != 0 {
		t.Fatalf("always_allow on the approval's own signature = %d %v, approval %s, allowance %+v; want 403, pending, none", code, out, status, allow)
	}
	// Signed over 1 LXC a month, it does not save 10.
	one := signed(challenge, `{"always_allow":{"amount_ulxc":1000000,"per":"month"}}`)
	code, out = call(http.MethodPost, approve, jsonOf(map[string]any{"assertion": one, "always_allow": tenMonth}))
	if status, allow := state(); code != http.StatusForbidden || status != "pending" || allow.ULXC != 0 {
		t.Fatalf("10 a month on a signature over 1 = %d %v, approval %s, allowance %+v; want 403, pending, none", code, out, status, allow)
	}
	// Signed over exactly 10 a month, the approval and the allowance are both saved.
	ten := signed(challenge, jsonOf(map[string]any{"always_allow": tenMonth}))
	code, out = call(http.MethodPost, approve, jsonOf(map[string]any{"assertion": ten, "always_allow": tenMonth}))
	if status, allow := state(); code != http.StatusOK || status != "approved" || allow != (economy.PayeeAllowance{ULXC: 10_000_000, Per: "month"}) {
		t.Fatalf("signed always_allow = %d %v, approval %s, allowance %+v; want 200, approved, 10 LXC a month", code, out, status, allow)
	}

	// A boost to 4 LXC under a daily limit of 5 is refused, and its signature stays unspent: once the limit is lowered
	// to 2, the same signed boost saves.
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	boost := map[string]any{"rule": "daily_limit_ulxc", "value": 4_000_000, "until": until}
	boost["assertion"] = signed(base+payer.ID+"/rules/challenge", jsonOf(map[string]any{"boost": map[string]any{"rule": "daily_limit_ulxc", "value": 4_000_000, "until": until}}))
	if code, out := call(http.MethodPost, base+payer.ID+"/rules/boosts", jsonOf(boost)); code != http.StatusBadRequest {
		t.Fatalf("boost below the limit = %d %v, want 400", code, out)
	}
	if code, out := call(http.MethodPut, base+payer.ID+"/rules", `{"approval_above_ulxc":2000000,"daily_limit_ulxc":2000000}`); code != http.StatusOK {
		t.Fatalf("lowering the daily limit = %d %v, want 200", code, out)
	}
	if code, out := call(http.MethodPost, base+payer.ID+"/rules/boosts", jsonOf(boost)); code != http.StatusCreated {
		t.Fatalf("the same signed boost once the rules allow it = %d %v, want 201", code, out)
	}
}
