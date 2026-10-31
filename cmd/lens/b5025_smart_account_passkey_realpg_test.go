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
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B50.25 — an owner session alone cannot name who controls an agent's crypto wallet: making its smart account takes the
// named owner key's signature over a challenge naming the agent, that key and the recovery address. With no signature,
// a signature over another recovery address's challenge, or one by another of the workspace's keys, nothing is made.
func TestB5025_MakingAnAgentsSmartAccountTakesTheOwnerKeysSignature(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b5025", "app.talyvor.test", "https://app.talyvor.test"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	cdp := cdpfake.New(t)
	store.SetSmartAccounts(cdp.Signers(t), "coinbase", smartaccount.CoinbaseV11)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
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

	signPasskey := b5018Passkey(t, store, ws, rpID, origin)
	credID := signPasskey("warm-up").CredentialID
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation) VALUES ('dev-b5025', $1, 'test', $2, '')`, ws, spki); err != nil {
		t.Fatal(err)
	}
	signPhone := func(challenge string) *passkey.Assertion {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return &passkey.Assertion{DeviceID: "dev-b5025", Challenge: challenge, Signature: base64.RawURLEncoding.EncodeToString(sig)}
	}

	var agentIDs []string
	for _, name := range []string{"Trader", "Ops"} {
		code, a := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents", `{"name":"`+name+`","wallet_kind":"crypto"}`)
		if code != http.StatusCreated {
			t.Fatalf("create %s = %d %v", name, code, a)
		}
		agentIDs = append(agentIDs, a["id"].(string))
	}
	path := "/v1/workspaces/" + ws + "/agents/" + agentIDs[0] + "/smart-account"
	const mine, thief = "0x00000000000000000000000000000000000Ec0de", "0x000000000000000000000000000000000000BAd1"
	owners := `"credential_id":"` + credID + `","recovery_address":"` + mine + `"`
	challenge := func(path, body string) string {
		t.Helper()
		code, c := call(http.MethodPost, path+"/challenge", body)
		if code != http.StatusOK {
			t.Fatalf("challenge %s = %d %v", body, code, c)
		}
		return c["challenge"].(string)
	}
	made := func(agentID string) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_smart_accounts WHERE agent_id = $1`, agentID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	refused := func(why, body string) {
		t.Helper()
		if code, out := call(http.MethodPost, path, body); code != http.StatusForbidden || made(agentIDs[0]) {
			t.Fatalf("%s = %d %v, made %v; want 403 and no smart account", why, code, out, made(agentIDs[0]))
		}
	}

	refused("an owner session naming its own recovery address, no signature", `{"credential_id":"`+credID+`","recovery_address":"`+thief+`"}`)
	refused("no signature", `{`+owners+`}`)
	signedFor := challenge(path, `{`+owners+`}`)
	refused("the passkey's signature over these owners' challenge, with the thief's recovery address",
		`{"credential_id":"`+credID+`","recovery_address":"`+thief+`","assertion":`+jsonOf(signPasskey(signedFor))+`}`)
	refused("the phone's signature naming the passkey as owner", `{`+owners+`,"assertion":`+jsonOf(signPhone(challenge(path, `{`+owners+`}`)))+`}`)
	refused("a signature over another agent's challenge",
		`{`+owners+`,"assertion":`+jsonOf(signPasskey(challenge("/v1/workspaces/"+ws+"/agents/"+agentIDs[1]+"/smart-account", `{`+owners+`}`)))+`}`)

	code, out := call(http.MethodPost, path, `{`+owners+`,"assertion":`+jsonOf(signPasskey(challenge(path, `{`+owners+`}`)))+`}`)
	if code != http.StatusOK || !made(agentIDs[0]) {
		t.Fatalf("signed by the named passkey = %d %v, want 200 and the account made", code, out)
	}
	var recovery string
	if err := pool.QueryRow(ctx, `SELECT recovery_address FROM agent_smart_accounts WHERE agent_id = $1`, agentIDs[0]).Scan(&recovery); err != nil || !strings.EqualFold(recovery, mine) {
		t.Fatalf("recovery owner = %q %v, want %s", recovery, err, mine)
	}
	// The phone's key, named as owner, signs for its own account.
	opsPath := "/v1/workspaces/" + ws + "/agents/" + agentIDs[1] + "/smart-account"
	if code, out := call(http.MethodPost, opsPath, `{"device_id":"dev-b5025","assertion":`+jsonOf(signPhone(challenge(opsPath, `{"device_id":"dev-b5025"}`)))+`}`); code != http.StatusOK || !made(agentIDs[1]) {
		t.Fatalf("signed by the named phone = %d %v, want 200 and the account made", code, out)
	}
}
