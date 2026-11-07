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

// B55.13 — loosening an agent's rules takes the same signature as resuming it. With a phone key in the workspace,
// raising the daily limit unsigned is refused and the rules stay; signed over its challenge it saves, and that
// signature saves nothing else; lowering a limit saves unsigned; a rollback to a looser version and a boost need the
// signature; a workspace with no key saves as before.
func TestB5513_LooseningRulesIsSignedTighteningIsNot(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	callAs := func(ws string) func(method, path, body string) (int, map[string]any) {
		owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
		return func(method, path, body string) (int, map[string]any) {
			t.Helper()
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			var out map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			return w.Code, out
		}
	}

	// A workspace with no key saves a looser change unsigned, as before.
	{
		const ws = "ws-b5513-nokey"
		call := callAs(ws)
		a, err := store.CreateAgent(ctx, ws, "free", "user-ada")
		if err != nil {
			t.Fatal(err)
		}
		rules := "/v1/workspaces/" + ws + "/agents/" + a.ID + "/rules"
		if code, out := call(http.MethodPut, rules, `{"daily_limit_ulxc":1000000}`); code != http.StatusOK {
			t.Fatalf("set with no key = %d %v", code, out)
		}
		if code, out := call(http.MethodPut, rules, `{"daily_limit_ulxc":9000000}`); code != http.StatusOK {
			t.Fatalf("raise with no key = %d %v, want 200 unsigned", code, out)
		}
	}

	const ws = "ws-b5513-phone"
	call := callAs(ws)
	a, err := store.CreateAgent(ctx, ws, "spender", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + a.ID
	daily := func() int64 {
		t.Helper()
		got, err := store.GetAgentRules(ctx, ws, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.DailyLimitULXC
	}
	// Rules set before the phone is added are version 1.
	if code, out := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":5000000}`); code != http.StatusOK {
		t.Fatalf("first rules = %d %v", code, out)
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
	// assertion asks the challenge for proposal, checks it is looser, and signs it.
	assertion := func(proposal string) map[string]string {
		t.Helper()
		code, c := call(http.MethodPost, base+"/rules/challenge", proposal)
		if code != http.StatusOK || c["looser"] != true || c["challenge"] == "" {
			t.Fatalf("challenge for %s = %d %v, want looser with a challenge", proposal, code, c)
		}
		challenge := c["challenge"].(string)
		return map[string]string{"device_id": dev["id"].(string), "challenge": challenge, "signature": sign(challenge)}
	}

	// Raising the daily limit unsigned is refused, and the rules stay.
	code, out := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":9000000}`)
	if code != http.StatusForbidden || !strings.Contains(out["error"].(string), "sign this change's challenge") || daily() != 5_000_000 {
		t.Fatalf("unsigned raise = %d %v, daily %d; want 403 and 5000000 kept", code, out, daily())
	}
	// The challenge says what changes, in rules history's words.
	if code, c := call(http.MethodPost, base+"/rules/challenge", `{"rules":{"daily_limit_ulxc":9000000}}`); code != http.StatusOK ||
		jsonOf(c["changes"]) != `["Daily limit: was 5 LXC, now 9 LXC"]` {
		t.Fatalf("challenge = %d %v, want the daily limit's change in words", code, c)
	}
	// A signature over raising it to 9 does not save raising it to 20.
	nine := assertion(`{"rules":{"daily_limit_ulxc":9000000}}`)
	code, out = call(http.MethodPut, base+"/rules", jsonOf(map[string]any{"rules": map[string]int64{"daily_limit_ulxc": 20_000_000}, "assertion": nine}))
	if code != http.StatusForbidden || daily() != 5_000_000 {
		t.Fatalf("a different change on that signature = %d %v, daily %d; want 403 and 5000000 kept", code, out, daily())
	}
	// Signed over its own challenge, the raise saves.
	nine = assertion(`{"rules":{"daily_limit_ulxc":9000000}}`)
	code, out = call(http.MethodPut, base+"/rules", jsonOf(map[string]any{"rules": map[string]int64{"daily_limit_ulxc": 9_000_000}, "assertion": nine}))
	if code != http.StatusOK || daily() != 9_000_000 {
		t.Fatalf("signed raise = %d %v, daily %d; want 200 and 9000000", code, out, daily())
	}
	// Lowering a limit saves at once, unsigned.
	if code, out := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":2000000}`); code != http.StatusOK || daily() != 2_000_000 {
		t.Fatalf("unsigned lowering = %d %v, daily %d; want 200 and 2000000", code, out, daily())
	}
	if code, c := call(http.MethodPost, base+"/rules/challenge", `{"rules":{"daily_limit_ulxc":1000000}}`); code != http.StatusOK || c["looser"] != false {
		t.Fatalf("challenge for a lowering = %d %v, want looser false", code, c)
	}

	// Rolling back to version 1 (5 LXC a day, above today's 2) loosens the rules: refused unsigned, saved signed.
	if code, out := call(http.MethodPost, base+"/rules/rollback", `{"version":1}`); code != http.StatusForbidden || daily() != 2_000_000 {
		t.Fatalf("unsigned looser rollback = %d %v, daily %d; want 403 and 2000000 kept", code, out, daily())
	}
	back := assertion(`{"rollback_to":1}`)
	if code, out := call(http.MethodPost, base+"/rules/rollback", jsonOf(map[string]any{"version": 1, "assertion": back})); code != http.StatusOK || daily() != 5_000_000 {
		t.Fatalf("signed rollback = %d %v, daily %d; want 200 and 5000000", code, out, daily())
	}

	// A boost always loosens: refused unsigned, saved signed over exactly itself.
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	boost := map[string]any{"rule": "daily_limit_ulxc", "value": 50_000_000, "until": until}
	if code, out := call(http.MethodPost, base+"/rules/boosts", jsonOf(boost)); code != http.StatusForbidden {
		t.Fatalf("unsigned boost = %d %v, want 403", code, out)
	}
	boost["assertion"] = assertion(jsonOf(map[string]any{"boost": map[string]any{"rule": "daily_limit_ulxc", "value": 50_000_000, "until": until}}))
	if code, out := call(http.MethodPost, base+"/rules/boosts", jsonOf(boost)); code != http.StatusCreated {
		t.Fatalf("signed boost = %d %v, want 201", code, out)
	}
	// 20 under a boost to 50 is still 20 once the boost ends, so it is signed; and while the boost lasts, putting the
	// limit back where it was raised from brings the 50 back, which is signed too.
	if code, out := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":20000000}`); code != http.StatusForbidden || daily() != 5_000_000 {
		t.Fatalf("unsigned 20 under a boost to 50 = %d %v, daily %d; want 403 and 5000000 kept", code, out, daily())
	}
	twenty := assertion(`{"rules":{"daily_limit_ulxc":20000000}}`)
	if code, out := call(http.MethodPut, base+"/rules", jsonOf(map[string]any{"rules": map[string]int64{"daily_limit_ulxc": 20_000_000}, "assertion": twenty})); code != http.StatusOK {
		t.Fatalf("signed 20 = %d %v, want 200", code, out)
	}
	if code, out := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":5000000}`); code != http.StatusForbidden || daily() != 20_000_000 {
		t.Fatalf("unsigned return to the boosted limit = %d %v, daily %d; want 403 and 20000000 kept", code, out, daily())
	}
	// Ending it never loosens.
	if code, out := call(http.MethodDelete, base+"/rules/boosts/daily_limit_ulxc", ""); code != http.StatusNoContent {
		t.Fatalf("ending a boost = %d %v, want 204 unsigned", code, out)
	}
}
