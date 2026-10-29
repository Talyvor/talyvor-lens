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

// B41.111 — a £2,000 first payment to a payee saved an hour ago is held, with nothing on the ledger, until 24 hours
// have passed (the minute's run then pays it) or the owner confirms with a passkey that they called the payee (which
// pays it at once). A payment of £1,000 or less is not held.
func TestNewPayeeCoolingOff_HeldUntil24HoursOrAPasskeyCall(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b41111-cooling-off", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -1000000}, {AccountID: company.ID, AmountMinor: 1000000}}}); err != nil {
		t.Fatal(err)
	}
	payee := map[string]string{}
	for _, name := range []string{"Called Supplier", "Waited Supplier", "Small Supplier"} {
		p, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
		if err != nil {
			t.Fatal(err)
		}
		payee[name] = p.ID
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '1 hour' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountPaymentRequestRoutes(r, store)
	mountMoneyPayeeRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, key, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// The owner's passkey: this test is the authenticator, its counter rising with each signature.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	attested := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	attested = append(append(binary.BigEndian.AppendUint16(append(attested, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
	if _, err := store.RegisterPasskey(ctx, ws, "Ada's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}); err != nil {
		t.Fatal(err)
	}
	counter := uint32(0)
	sign := func(challenge string) *passkey.Assertion {
		counter++
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	}
	signed := func(challenge string) string {
		b, _ := json.Marshal(map[string]any{"assertion": sign(challenge)})
		return string(b)
	}

	// The ledger: each payment out is one entry moving its amount to the partner.
	paymentsOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}
	// payApproved prepares one payment to name and approves it with the passkey, returning its id and its item.
	payApproved := func(name string, minor int64) (string, economy.PaymentRequestItem) {
		t.Helper()
		code, pr := call(http.MethodPost, "/v1/money/payment-requests", "pay-"+name, `{"client":"chat","items":[
			{"payee_id":"`+payee[name]+`","currency":"GBP","amount_minor":`+jsonOf(minor)+`,"reference":"INV-1"}]}`)
		if code != http.StatusCreated || pr["status"] != "pending_approval" {
			t.Fatalf("preparing the payment to %s = %d %v", name, code, pr)
		}
		id := pr["id"].(string)
		code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approval", "", "")
		if code != http.StatusOK {
			t.Fatalf("approval statement = %d %v", code, out)
		}
		body, _ := json.Marshal(map[string]any{"statement": out["statement"], "assertion": sign(out["challenge"].(string))})
		if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approve", "", string(body)); code != http.StatusOK || out["status"] != "approved" {
			t.Fatalf("approving the payment to %s = %d %v", name, code, out)
		}
		return id, coolingOffItem(t, store, ws, id)
	}

	// £1,000 to a payee saved an hour ago is paid at once: only more than £1,000 is held.
	if _, it := payApproved("Small Supplier", 100000); it.PaymentID == "" {
		t.Fatalf("£1,000 to a new payee was not paid: %+v", it)
	}
	if n, minor := paymentsOut(); n != 1 || minor != 100000 {
		t.Fatalf("after the £1,000 payment the ledger has %d payments out of %d, want 1 of 100000", n, minor)
	}

	// £2,000 approved with a passkey is held: nothing on the ledger, and paying again or the minute's run pays nothing.
	calledID, it := payApproved("Called Supplier", 200000)
	if it.PayStatus != economy.PayHeld || it.PaymentID != "" || !strings.Contains(it.PayDetail, "called Called Supplier") {
		t.Fatalf("£2,000 to a payee saved an hour ago = %+v, want held, unpaid, saying how to release it", it)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+calledID+"/pay", "", ""); code != http.StatusOK {
		t.Fatalf("paying the held request again = %d %v", code, out)
	}
	if n, err := store.RunHeldPayments(ctx, ws); err != nil || n != 0 {
		t.Fatalf("the minute's run during the 24 hours paid %d requests (%v), want 0", n, err)
	}
	if n, _ := paymentsOut(); n != 1 {
		t.Fatalf("while held the ledger has %d payments out, want still 1", n)
	}
	if _, err := store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: payee["Called Supplier"], AmountMinor: 200000, Currency: "GBP",
		Funding: economy.FundingTest, Reference: "direct", IdempotencyKey: "direct-1"}); !errors.Is(err, economy.ErrPayeeCoolingOff) {
		t.Fatalf("a direct £2,000 payment to the new payee = %v, want ErrPayeeCoolingOff", err)
	}
	code, out := call(http.MethodGet, "/v1/money/payees", "", "")
	if code != http.StatusOK || !strings.Contains(jsonOf(out), `"cooling_off_until"`) {
		t.Fatalf("the payees list = %d %v, want each new payee's cooling_off_until", code, out)
	}

	// The owner says, with a passkey, that they called the supplier: the held payment is paid at once.
	called := "/v1/money/payees/" + payee["Called Supplier"] + "/called"
	if code, out := call(http.MethodPost, called, "", `{}`); code != http.StatusForbidden {
		t.Fatalf("a call confirmation without a passkey = %d %v, want 403", code, out)
	}
	code, out = call(http.MethodPost, called+"/challenge", "", "")
	if code != http.StatusOK {
		t.Fatalf("the call challenge = %d %v", code, out)
	}
	code, out = call(http.MethodPost, called, "", signed(out["challenge"].(string)))
	if code != http.StatusOK || out["called_at"] == nil || out["called_by"] != b64(credID) || out["cooling_off_until"] != nil {
		t.Fatalf("confirming the call = %d %v, want 200, called by the passkey, no cooling-off left", code, out)
	}
	if it := coolingOffItem(t, store, ws, calledID); it.PaymentID == "" || it.PayStatus == economy.PayHeld {
		t.Fatalf("after the call the held payment = %+v, want paid", it)
	}
	if n, minor := paymentsOut(); n != 2 || minor != 300000 {
		t.Fatalf("after the call the ledger has %d payments out of %d, want 2 of 300000", n, minor)
	}

	// Another £2,000 is held until its payee's 24 hours have passed; the minute's run then pays it, once.
	waitedID, it := payApproved("Waited Supplier", 200000)
	if it.PayStatus != economy.PayHeld {
		t.Fatalf("£2,000 to the second new payee = %+v, want held", it)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '24 hours 1 minute' WHERE id = $1`, payee["Waited Supplier"]); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RunHeldPayments(ctx, ws); err != nil || n != 1 {
		t.Fatalf("the minute's run after 24 hours paid %d requests (%v), want 1", n, err)
	}
	if n, err := store.RunHeldPayments(ctx, ws); err != nil || n != 0 {
		t.Fatalf("the next run paid %d requests (%v), want 0", n, err)
	}
	if it := coolingOffItem(t, store, ws, waitedID); it.PaymentID == "" {
		t.Fatalf("after 24 hours the held payment = %+v, want paid", it)
	}
	if n, minor := paymentsOut(); n != 3 || minor != 500000 {
		t.Fatalf("after 24 hours the ledger has %d payments out of %d, want 3 of 500000", n, minor)
	}
}

func coolingOffItem(t *testing.T, store *economy.DualTokenStore, ws, id string) economy.PaymentRequestItem {
	t.Helper()
	pr, err := store.PaymentRequest(context.Background(), ws, "", id)
	if err != nil || len(pr.Items) != 1 {
		t.Fatalf("request %s = %+v (%v)", id, pr, err)
	}
	return pr.Items[0]
}
