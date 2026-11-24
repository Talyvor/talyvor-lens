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
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B28.339 — once a workspace has a passkey or the phone's key, an owner session alone cashes nothing out: with no
// signature, or one over another cash-out's challenge, the request is refused and no posting is written. Signed by the
// passkey or the phone over that cash-out's own challenge, the credits are held to be paid, and that signature is spent.
func TestB28339_CashOutTakesAPasskeyOrThePhonesKey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b28339", "app.talyvor.test", "https://app.talyvor.test"
	const lxc = int64(1_000_000)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	bank.SetOwnerVerifier(earnverify.New(false))
	bank.SetCashOutPartner(economy.TestCashOutPartner{})
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	confirmedAccounts(t, pool, ws) // B61.52
	agent, err := bank.CreateAgent(ctx, ws, "earner", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bank.CreditLXC(ctx, ws, 100*lxc, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
		t.Fatal(err)
	}
	if _, err := bank.FundAgent(ctx, ws, agent.ID, 100*lxc); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountCashOutRoutes(r, bank)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	path := "/v1/workspaces/" + ws + "/agents/" + agent.ID + "/cash-outs"
	challenge := func(body string) string {
		t.Helper()
		code, c := call(path+"/challenge", body)
		if code != http.StatusOK {
			t.Fatalf("challenge %s = %d %v", body, code, c)
		}
		return c["challenge"].(string)
	}
	postings := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind = 'cash_out' AND account = 'agent:' || $2`,
			ws, agent.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	signPasskey := b5018Passkey(t, bank, ws, rpID, origin)
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation) VALUES ('dev-b28339', $1, 'test', $2, '')`, ws, spki); err != nil {
		t.Fatal(err)
	}
	signPhone := func(challenge string) *passkey.Assertion {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return &passkey.Assertion{DeviceID: "dev-b28339", Challenge: challenge, Signature: base64.RawURLEncoding.EncodeToString(sig)}
	}

	const forty = `"amount_ulxc":40000000,"destination":"test bank ••1234"`
	refused := func(why, body string) {
		t.Helper()
		if code, out := call(path, body); code != http.StatusForbidden || postings() != 0 {
			t.Fatalf("%s = %d %v with %d cash-out postings; want 403 and none", why, code, out, postings())
		}
	}
	refused("an owner session, no signature", `{`+forty+`}`)
	refused("the passkey's signature over a cash-out of 1 LXC, asking for 40",
		`{`+forty+`,"assertion":`+jsonOf(signPasskey(challenge(`{"amount_ulxc":1000000,"destination":"test bank ••1234"}`)))+`}`)
	refused("the phone's signature over a cash-out to another account",
		`{`+forty+`,"assertion":`+jsonOf(signPhone(challenge(`{"amount_ulxc":40000000,"destination":"bank ••9999"}`)))+`}`)

	signed := `{` + forty + `,"assertion":` + jsonOf(signPasskey(challenge(`{`+forty+`}`))) + `}`
	if code, out := call(path, signed); code != http.StatusCreated || out["status"] == nil || postings() != 1 {
		t.Fatalf("signed by the passkey = %d %v with %d postings; want 201 and one", code, out, postings())
	}
	var held int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = $1 AND account = 'agent:' || $2`,
		ws, agent.ID).Scan(&held); err != nil || held != 60*lxc {
		t.Fatalf("agent holds %d %v, want 60 LXC after 40 cashed out", held, err)
	}
	if code, out := call(path, signed); code != http.StatusForbidden || postings() != 1 {
		t.Fatalf("the same signature again = %d %v with %d postings; want 403 and still one", code, out, postings())
	}
	if code, out := call(path, `{"amount_ulxc":10000000,"destination":"test bank ••1234","assertion":`+
		jsonOf(signPhone(challenge(`{"amount_ulxc":10000000,"destination":"test bank ••1234"}`)))+`}`); code != http.StatusCreated || postings() != 2 {
		t.Fatalf("signed by the phone = %d %v with %d postings; want 201 and two", code, out, postings())
	}
}
