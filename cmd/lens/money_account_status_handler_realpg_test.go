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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B30.24 — the owner freezes an agent's account: a payment from it is refused and posts no hold, while money in still
// lands; a freeze of the company's account holds the agent's sub-account too; an unfreeze goes through at once until
// the workspace has a passkey, and then only with an assertion over the account's challenge. Closing moves the
// balance — an agent's into the company's account, the company's to a saved payee — and each reads closed at zero,
// read off postings.
func TestMoneyAccountRoutes_FreezeRefusesMoneyOutAndCloseMovesTheBalance(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b3024-status", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	agent, err := store.CreateAgent(ctx, ws, "Treasurer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	acct, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	topUp := func(key, to string, minor int64) error {
		_, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
			IdempotencyKey: key, Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
				{AccountID: partnerAccount, AmountMinor: -minor}, {AccountID: to, AmountMinor: minor}}})
		return err
	}
	if err := topUp("top-up-1", acct.ID, 50000); err != nil {
		t.Fatal(err)
	}
	balance := func(id string) int64 {
		t.Helper()
		b, err := store.Balance(ctx, ws, id)
		if err != nil {
			t.Fatal(err)
		}
		return b.AmountMinor
	}
	postings := func(id string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_postings WHERE account_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	pay := func(key string) error {
		_, err := store.PayOutside(ctx, ws, economy.OutsidePayment{AgentID: agent.ID, PayeeID: payee.ID, AmountMinor: 1000, Currency: "GBP", IdempotencyKey: key})
		return err
	}

	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
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
	expect := func(path, body, status string) map[string]any {
		t.Helper()
		code, out := call(path, body)
		if code != http.StatusOK || out["status"] != status {
			t.Fatalf("%s = %d %v, want 200 and the account %s", path, code, out, status)
		}
		return out
	}
	accounts := "/v1/money/accounts/"

	// Frozen: a payment from the agent's account is refused and posts nothing; money in still lands.
	expect(accounts+acct.ID+"/freeze", "", "frozen")
	before := postings(acct.ID)
	if err := pay("frozen-pay"); !errors.Is(err, economy.ErrMoneyAccountNotOpen) {
		t.Fatalf("a payment from the frozen account: %v, want ErrMoneyAccountNotOpen", err)
	}
	if postings(acct.ID) != before || balance(acct.ID) != 50000 {
		t.Fatalf("the refused payment posted: %d postings (was %d), balance %d", postings(acct.ID), before, balance(acct.ID))
	}
	if err := topUp("top-up-2", acct.ID, 2500); err != nil || balance(acct.ID) != 52500 {
		t.Fatalf("money in did not land on the frozen account: %v, balance %d, want 52500", err, balance(acct.ID))
	}
	// Unfrozen at once while the workspace has no passkey; a freeze of the company's account holds the agent's too.
	expect(accounts+acct.ID+"/unfreeze", "{}", "open")
	expect(accounts+company.ID+"/freeze", "", "frozen")
	if err := pay("parent-frozen-pay"); !errors.Is(err, economy.ErrMoneyAccountNotOpen) || !strings.Contains(err.Error(), company.ID) {
		t.Fatalf("a payment under the frozen company account: %v, want ErrMoneyAccountNotOpen naming %s", err, company.ID)
	}
	expect(accounts+company.ID+"/unfreeze", "{}", "open")

	// With a passkey, an unfreeze needs an assertion over this account's challenge. This test is the authenticator.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	authData := func(flags byte, count uint32, attested bool) []byte {
		d := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), flags), count)
		if attested {
			d = binary.BigEndian.AppendUint16(append(d, make([]byte, 16)...), uint16(len(credID)))
			d = append(append(d, credID...), 0xa0)
		}
		return d
	}
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `","crossOrigin":false}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterPasskey(ctx, ws, "Owner's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: authData(0x45, 0, true)}); err != nil {
		t.Fatal(err)
	}
	sign := func(challenge string, count uint32) string {
		cdj, ad := clientData("webauthn.get", challenge), authData(0x05, count, false)
		h := sha256.Sum256(cdj)
		signed := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, signed[:])
		return jsonOf(map[string]any{"assertion": map[string]string{"credential_id": b64(credID), "client_data_json": b64(cdj),
			"authenticator_data": b64(ad), "signature": b64(sig)}})
	}
	expect(accounts+acct.ID+"/freeze", "", "frozen")
	if code, out := call(accounts+acct.ID+"/unfreeze", "{}"); code != http.StatusForbidden {
		t.Fatalf("unfreeze with no assertion = %d %v, want 403", code, out)
	}
	code, out := call(accounts+acct.ID+"/unfreeze/challenge", "")
	if code != http.StatusOK || !strings.Contains(jsonOf(out["allow_credentials"]), b64(credID)) {
		t.Fatalf("unfreeze challenge = %d %v", code, out)
	}
	expect(accounts+acct.ID+"/unfreeze", sign(out["challenge"].(string), 1), "open")
	if err := pay("open-pay"); err != nil {
		t.Fatalf("a payment from the unfrozen account: %v", err)
	}

	// Closing the company's account waits for its agents'; closing the agent's moves its balance into the company's.
	if code, out := call(accounts+company.ID+"/close", "{}"); code != http.StatusConflict {
		t.Fatalf("close the company's account under an open agent account = %d %v, want 409", code, out)
	}
	closed := expect(accounts+acct.ID+"/close", "{}", "closed")
	if closed["balance_minor"] != float64(0) || balance(acct.ID) != 0 || balance(company.ID) != 51500 {
		t.Fatalf("after the close the agent's account reads %v and holds %d; the company's holds %d, want 0 and 51500", closed["balance_minor"], balance(acct.ID), balance(company.ID))
	}
	var moved int64
	if err := pool.QueryRow(ctx, `SELECT p.amount_minor FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		WHERE e.kind = 'account_close' AND p.account_id = $1`, company.ID).Scan(&moved); err != nil || moved != 51500 {
		t.Fatalf("the close's posting to the company's account is %d (%v), want 51500", moved, err)
	}
	if code, out := call(accounts+acct.ID+"/close", "{}"); code != http.StatusConflict {
		t.Errorf("close the closed account again = %d %v, want 409", code, out)
	}
	// The company's holds money: it closes only by paying it to a saved payee, and then reads closed at zero.
	if code, out := call(accounts+company.ID+"/close", "{}"); code != http.StatusConflict {
		t.Fatalf("close the company's account with money and no payee = %d %v, want 409", code, out)
	}
	expect(accounts+company.ID+"/close", `{"payee_id":"`+payee.ID+`"}`, "closed")
	var paid int64
	var status string
	if err := pool.QueryRow(ctx, `SELECT amount_minor, status FROM money_payments WHERE account_id = $1`, company.ID).Scan(&paid, &status); err != nil ||
		paid != 51500 || status != economy.PaymentCompleted || balance(company.ID) != 0 {
		t.Fatalf("the close paid %d (%s, %v) and left %d, want 51500 completed and zero", paid, status, err, balance(company.ID))
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test"); err != nil {
		t.Errorf("a new GBP account after the close: %v", err)
	}
}
