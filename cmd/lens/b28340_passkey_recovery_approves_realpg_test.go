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
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.340 — B28.68's DONE line from talyvor-lens alone, through the routes its talyvor-suite side calls: a removed
// passkey can no longer approve, and a recovery passkey can. Ada's phone holds the workspace's first passkey (whose
// answer carries the recovery code) and her old laptop a second one. She removes the laptop's; it then signs no
// approval. She loses the phone and locks the workspace down, which revokes it; it signs no approval either. After the
// lift she adds a new passkey with the recovery code, and that passkey approves the held payment, which the agent then
// pays: one balanced pay entry on agent_postings, the approval used and kept as signed by the recovery passkey.
func TestB28340_ARemovedPasskeyCannotApproveAndARecoveryPasskeyCan(t *testing.T) {
	pool := agentRoutesDB(t)
	unconfirmedAccounts(t, pool) // B61.52: the lift's identity check is the account's own
	ctx := context.Background()
	const ws, rpID, origin = "ws-b28340-recovery", "app.talyvor.com", "https://app.talyvor.com"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, account_type) VALUES ($1, $1, $1, true, $2)`,
		ws, economy.AccountTypePersonal); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	r.Use(clientIPMiddleware())
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountKeyRecoveryRoutes(r, store)
	mountVerificationRoutes(r, store, partners.OneVerifier(&partners.TestKYCProvider{}))
	mountLockdownRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}, TokenID: "ada-laptop",
		AuthTime: time.Now()}
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		req.RemoteAddr = "172.18.0.3:41234"
		req.Header.Set("X-Forwarded-For", "203.0.113.10")
		req.Header.Set("X-Talyvor-Signed-In-At", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	type authenticator struct {
		credID   string
		register func() (map[string]any, string) // over a fresh registration challenge, which it answers too
		sign     func(challenge string) *passkey.Assertion
	}
	agents := "/v1/workspaces/" + ws + "/agents"
	newAuthenticator := func(name string) authenticator {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		cred := make([]byte, 16)
		_, _ = rand.Read(cred)
		counter := uint32(0)
		return authenticator{
			credID: b64(cred),
			register: func() (map[string]any, string) {
				_, c := call(http.MethodPost, agents+"/passkeys/challenge", nil)
				ad := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
				ad = append(append(binary.BigEndian.AppendUint16(append(ad, make([]byte, 16)...), uint16(len(cred))), cred...), 0xa0)
				return map[string]any{"credential_id": b64(cred), "name": name, "public_key": b64(spki),
					"client_data_json": b64(clientData("webauthn.create", c["challenge"].(string))), "authenticator_data": b64(ad)}, c["challenge"].(string)
			},
			sign: func(challenge string) *passkey.Assertion {
				counter++
				cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
				h := sha256.Sum256(cdj)
				digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
				sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
				return &passkey.Assertion{CredentialID: b64(cred), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
			},
		}
	}

	// The phone's passkey is the first key: free, and its answer carries the recovery code. The laptop's is signed in by it.
	phone, laptop := newAuthenticator("Ada's phone"), newAuthenticator("Ada's old laptop")
	reg, _ := phone.register()
	code, first := call(http.MethodPost, agents+"/passkeys", reg)
	recoveryCode, _ := first["recovery_code"].(string)
	if code != http.StatusCreated || recoveryCode == "" {
		t.Fatalf("the phone's passkey = %d %v; want 201 with a recovery_code", code, first)
	}
	reg, over := laptop.register()
	reg["assertion"] = phone.sign(over)
	if code, out := call(http.MethodPost, agents+"/passkeys", reg); code != http.StatusCreated {
		t.Fatalf("the laptop's passkey, signed by the phone's = %d %v; want 201", code, out)
	}

	// Ada's identity was checked before she lost anything; a lockdown lifts only after the same person is checked again.
	verification := "/v1/workspaces/" + ws + "/verification"
	ada := map[string]string{"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"}
	if code, out := call(http.MethodPost, verification+"/contact", map[string]string{"email": "ada@example.com", "phone": "+447700900123"}); code != http.StatusCreated {
		t.Fatalf("contact check = %d %v", code, out)
	}
	if code, out := call(http.MethodPost, verification+"/identity", ada); code != http.StatusCreated {
		t.Fatalf("identity check = %d %v", code, out)
	}

	// A funded agent's 2 LXC payment to another agent waits for Ada's approval.
	payer, err := store.CreateAgent(ctx, ws, "payer", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "payee", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, payer.ID, 5_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, payer.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	payerAccount := "agent:" + payer.ID // the payer's account on agent_postings
	pay := map[string]any{"to_agent_id": payee.ID, "amount_ulxc": 2_000_000}
	code, held := call(http.MethodPost, agents+"/"+payer.ID+"/pay", pay)
	approvalID, _ := held["approval_id"].(string)
	if code != http.StatusAccepted || approvalID == "" {
		t.Fatalf("pay above the approval amount = %d %v; want 202 naming its approval", code, held)
	}
	approve := func(signer authenticator) (int, map[string]any, string) {
		t.Helper()
		_, c := call(http.MethodPost, agents+"/approvals/"+approvalID+"/challenge", nil)
		code, out := call(http.MethodPost, agents+"/approvals/"+approvalID+"/approve", map[string]any{"assertion": signer.sign(c["challenge"].(string))})
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, approvalID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return code, out, status
	}
	payPostings := func() (entries int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(DISTINCT entry_id) FROM agent_postings WHERE account = $1 AND kind = 'pay'`, payerAccount).Scan(&entries); err != nil {
			t.Fatal(err)
		}
		return entries
	}

	// The removed laptop passkey signs no approval.
	if code, out := call(http.MethodDelete, agents+"/passkeys/"+laptop.credID, nil); code != http.StatusOK {
		t.Fatalf("remove the laptop's passkey = %d %v; want 200", code, out)
	}
	if code, out, status := approve(laptop); code == http.StatusOK || status != "pending" {
		t.Fatalf("approve signed by the removed passkey = %d %v, %s; want refused and pending", code, out, status)
	}

	// The phone is lost: the lockdown names no key, so it revokes the last one, and the phone signs no approval either.
	if code, out := call(http.MethodPost, "/v1/money/lockdown", map[string]string{"reason": "lost my phone"}); code != http.StatusOK {
		t.Fatalf("lockdown = %d %v", code, out)
	}
	if code, out, status := approve(phone); code == http.StatusOK || status != "pending" {
		t.Fatalf("approve signed by the lost phone's passkey = %d %v, %s; want refused and pending", code, out, status)
	}
	if code, out := call(http.MethodPost, verification+"/identity", ada); code != http.StatusCreated {
		t.Fatalf("Ada's new identity check = %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/lockdown/lift", nil); code != http.StatusOK {
		t.Fatalf("lift = %d %v; want 200", code, out)
	}

	// The recovery code adds a new passkey at once; it resumes the agents and approves the held payment.
	recovered := newAuthenticator("Ada's new phone")
	reg, _ = recovered.register()
	if code, out := call(http.MethodPost, "/v1/workspaces/"+ws+"/keys/recover", map[string]any{"code": recoveryCode, "passkey": reg}); code != http.StatusCreated {
		t.Fatalf("recover with the recovery code = %d %v; want 201", code, out)
	}
	_, c := call(http.MethodPost, agents+"/resume-all/challenge", nil)
	if code, out := call(http.MethodPost, agents+"/resume-all", map[string]any{"assertion": recovered.sign(c["challenge"].(string))}); code != http.StatusOK {
		t.Fatalf("resume-all signed by the recovery passkey = %d %v; want 200", code, out)
	}
	if n := payPostings(); n != 0 {
		t.Fatalf("pay entries before the approval = %d; want none", n)
	}
	if code, out, status := approve(recovered); code != http.StatusOK || status != "approved" {
		t.Fatalf("approve signed by the recovery passkey = %d %v, %s; want 200 approved", code, out, status)
	}
	if code, out := call(http.MethodPost, agents+"/"+payer.ID+"/pay", pay); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("pay after the recovery passkey approved = %d %v; want paid", code, out)
	}

	// The ledger: one pay entry, balanced, moving exactly 2 LXC from the payer; the approval used and signed by the new key.
	var entries, unbalanced int
	var fromPayer int64
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT entry_id),
			COALESCE(sum(amount_ulxc) FILTER (WHERE account = $1), 0),
			(SELECT count(*) FROM (SELECT 1 FROM agent_postings WHERE entry_id IN
				(SELECT entry_id FROM agent_postings WHERE account = $1 AND kind = 'pay') GROUP BY entry_id HAVING sum(amount_ulxc) <> 0) u)
		FROM agent_postings WHERE entry_id IN (SELECT entry_id FROM agent_postings WHERE account = $1 AND kind = 'pay')`,
		payerAccount).Scan(&entries, &fromPayer, &unbalanced); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || fromPayer != -2_000_000 || unbalanced != 0 {
		t.Errorf("pay entries = %d moving %d µLXC on the payer, %d unbalanced; want one balanced entry of -2000000", entries, fromPayer, unbalanced)
	}
	var status, keyKind, keyID string
	if err := pool.QueryRow(ctx, `SELECT status, owner_key_kind, owner_key_id FROM agent_approvals WHERE id = $1`, approvalID).Scan(&status, &keyKind, &keyID); err != nil {
		t.Fatal(err)
	}
	if status != "used" || keyKind != "passkey" || keyID != recovered.credID {
		t.Errorf("the approval after the payment: %s, signed by %s %s; want used, signed by the recovery passkey %s", status, keyKind, keyID, recovered.credID)
	}
}
