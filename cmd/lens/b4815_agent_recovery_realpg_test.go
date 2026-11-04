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
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/sha3"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B48.15 — through the routes: the person adds a recovery signer with the passkey that owns their agent's account; the
// phone is lost, and the recovery signer's signature puts the new phone's passkey in its place, after which the spend
// permission the lost passkey signed allows nothing and one the new passkey signs does; the agent's key is rotated to
// the provider's next key; a lockdown freezes every agent of the workspace at once, and the gate refuses each. The
// calls each change answers are proven on chain in internal/money/smartaccount's devnet test.
func TestB4815_RecoveryRotationAndLockdown(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4815", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetSmartAccounts(cdpfake.New(t).Signers(t), "coinbase", smartaccount.CoinbaseV11)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path string, body any, out any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code
	}

	lostPhone := b5018Passkey(t, store, ws, rpID, origin)
	keys, err := store.ListPasskeys(ctx, ws)
	if err != nil || len(keys) != 1 {
		t.Fatalf("passkeys = %v %v", keys, err)
	}
	// The new phone's passkey, registered from a signed-in device.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, public_key) VALUES ('cred-new', $1, $2)`, ws, spki); err != nil {
		t.Fatal(err)
	}
	counter := uint32(0)
	newPhone := func(challenge string) *passkey.Assertion {
		counter++
		b64, rp := base64.RawURLEncoding.EncodeToString, sha256.Sum256([]byte(rpID))
		cdj := []byte(`{"type":"webauthn.get","challenge":"` + challenge + `","origin":"` + origin + `"}`)
		ad, h := binary.BigEndian.AppendUint32(append(rp[:], 0x05), counter), sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return &passkey.Assertion{CredentialID: "cred-new", ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	}

	newAgent := func(name, cred string, sign func(string) *passkey.Assertion) (string, economy.AgentSmartAccount) {
		t.Helper()
		var agent economy.Agent
		if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents", map[string]string{"name": name, "wallet_kind": "crypto"}, &agent); code != http.StatusCreated {
			t.Fatalf("create agent = %d", code)
		}
		base := "/v1/workspaces/" + ws + "/agents/" + agent.ID
		var ch struct{ Challenge string }
		var acct economy.AgentSmartAccount
		if code := call(http.MethodPost, base+"/smart-account/challenge", map[string]string{"credential_id": cred}, &ch); code != http.StatusOK {
			t.Fatalf("smart account challenge = %d", code)
		}
		if code := call(http.MethodPost, base+"/smart-account", map[string]any{"credential_id": cred, "assertion": sign(ch.Challenge)}, &acct); code != http.StatusOK {
			t.Fatalf("smart account = %d", code)
		}
		return agent.ID, acct
	}
	agentID, acct := newAgent("Trader", keys[0].CredentialID, lostPhone)
	base := "/v1/workspaces/" + ws + "/agents/" + agentID

	const usdc, payee = "0x036CbD53842c5426634e7929541eC2318f3dCF7e", "0x000000000000000000000000000000000000b0b0"
	permit := func(sign func(string) *passkey.Assertion) int {
		t.Helper()
		var p struct{ Canonical, Challenge string }
		if code := call(http.MethodPost, base+"/spend-permission/prepare", map[string]any{"tokens": []map[string]any{{"token": usdc,
			"allowance": 100, "period": "day", "per_payment": 50}}, "expires": time.Now().Add(time.Hour)}, &p); code != http.StatusOK {
			t.Fatalf("prepare = %d", code)
		}
		return call(http.MethodPut, base+"/spend-permission", map[string]any{"canonical": p.Canonical, "assertion": sign(p.Challenge)}, nil)
	}
	gate := func(agent string) error {
		return store.AuthoriseAgentCryptoPayment(ctx, ws, agent, smartaccount.Payment{Token: usdc, To: payee, Amount: big.NewInt(1)})
	}
	if code := permit(lostPhone); code != http.StatusOK {
		t.Fatalf("a permission signed by the account's passkey = %d", code)
	}
	if err := gate(agentID); err != nil {
		t.Fatalf("a payment inside it: %v", err)
	}

	// A recovery signer, added with the passkey that owns the account, goes in at index 2.
	recovery, _ := secp256k1.GeneratePrivateKey()
	rh := sha3.NewLegacyKeccak256()
	rh.Write(recovery.PubKey().SerializeUncompressed()[1:])
	recoveryAddr := "0x" + hex.EncodeToString(rh.Sum(nil)[12:])
	var ch struct{ Challenge string }
	if code := call(http.MethodPost, base+"/recovery-signer/challenge", map[string]string{"recovery_address": recoveryAddr}, &ch); code != http.StatusOK {
		t.Fatalf("recovery signer challenge = %d", code)
	}
	if code := call(http.MethodPut, base+"/recovery-signer", map[string]any{"recovery_address": recoveryAddr, "assertion": newPhone(ch.Challenge)}, nil); code != http.StatusForbidden {
		t.Fatalf("a recovery signer added by a passkey that does not own the account = %d, want 403", code)
	}
	var added economy.OwnerChange
	if code := call(http.MethodPut, base+"/recovery-signer", map[string]any{"recovery_address": recoveryAddr, "assertion": lostPhone(ch.Challenge)}, &added); code != http.StatusOK ||
		len(added.Smart.Owners) != 3 || added.Smart.Owners[2].Index != 2 || !strings.EqualFold(added.Smart.Owners[2].Address, recoveryAddr) ||
		added.SignedBy != "person" || !strings.HasPrefix(added.Call, "0xb61d27f6") { // execute(address,uint256,bytes)
		t.Fatalf("add the recovery signer = %d %+v", code, added)
	}

	// The phone is lost: the recovery signer puts the new phone's passkey in its place, at index 3.
	personalSign := func(k *secp256k1.PrivateKey, msg string) string {
		h := sha3.NewLegacyKeccak256()
		h.Write([]byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg)) + msg))
		c := secpecdsa.SignCompact(k, h.Sum(nil), false)
		return "0x" + hex.EncodeToString(append(c[1:], c[0]))
	}
	if code := call(http.MethodPost, base+"/recover/challenge", map[string]string{"credential_id": "cred-new"}, &ch); code != http.StatusOK {
		t.Fatalf("recover challenge = %d", code)
	}
	stranger, _ := secp256k1.GeneratePrivateKey()
	recover := func(sig string, out any) int {
		return call(http.MethodPost, base+"/recover", map[string]string{"credential_id": "cred-new", "challenge": ch.Challenge, "signature": sig}, out)
	}
	if code := recover(personalSign(stranger, ch.Challenge), nil); code != http.StatusForbidden {
		t.Fatalf("a recovery signed by a stranger's key = %d, want 403", code)
	}
	var recovered economy.OwnerChange
	if code := recover(personalSign(recovery, ch.Challenge), &recovered); code != http.StatusOK || recovered.Smart.Owners[0].CredentialID != "cred-new" ||
		recovered.Smart.Owners[0].Index != 3 || recovered.SignedBy != "recovery" || !strings.HasPrefix(recovered.Call, "0x34fcd5be") { // executeBatch
		t.Fatalf("recover = %d %+v", code, recovered)
	}
	if code := recover(personalSign(recovery, ch.Challenge), nil); code != http.StatusForbidden {
		t.Fatalf("the same recovery again = %d, want 403: its challenge is spent", code)
	}
	if err := gate(agentID); !errors.Is(err, smartaccount.ErrPermission) {
		t.Fatalf("a payment under the permission the lost passkey signed: %v, want refused", err)
	}
	if code := permit(lostPhone); code != http.StatusForbidden {
		t.Fatalf("a permission signed by the lost passkey = %d, want 403", code)
	}
	if code := permit(newPhone); code != http.StatusOK {
		t.Fatalf("a permission signed by the new passkey = %d", code)
	}
	if err := gate(agentID); err != nil {
		t.Fatalf("a payment under the new passkey's permission: %v", err)
	}

	// Rotation: the provider's next key signs for the agent from now on, at index 4.
	var rotated economy.OwnerChange
	if code := call(http.MethodPost, base+"/rotate-signer", nil, &rotated); code != http.StatusOK || rotated.RotatedFrom != acct.Owners[1].Address ||
		rotated.Smart.Owners[1].Address == acct.Owners[1].Address || rotated.Smart.Owners[1].Index != 4 || rotated.SignedBy != "agent" {
		t.Fatalf("rotate = %d %+v", code, rotated)
	}
	var made, now string
	if err := pool.QueryRow(ctx, `SELECT made_with_agent_signer, agent_signer FROM agent_smart_accounts WHERE agent_id = $1`, agentID).Scan(&made, &now); err != nil ||
		made != acct.Owners[1].Address || now != rotated.Smart.Owners[1].Address {
		t.Fatalf("made with %s, now %s (%v)", made, now, err)
	}

	// Lockdown: every agent of the workspace frozen at once.
	otherID, _ := newAgent("Buyer", "cred-new", newPhone)
	var locked struct{ Frozen []economy.AgentFreeze }
	if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/lockdown", nil, &locked); code != http.StatusOK || len(locked.Frozen) != 2 {
		t.Fatalf("lockdown = %d %+v", code, locked)
	}
	for _, id := range []string{agentID, otherID} {
		if err := gate(id); !errors.Is(err, smartaccount.ErrFrozen) {
			t.Fatalf("agent %s after the lockdown: %v, want frozen", id, err)
		}
	}
	if code := call(http.MethodPost, base+"/rotate-signer", nil, nil); code != http.StatusConflict {
		t.Fatalf("rotating a frozen agent = %d, want 409", code)
	}
}
