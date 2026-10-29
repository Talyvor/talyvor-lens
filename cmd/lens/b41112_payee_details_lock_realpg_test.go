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
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
)

// B41.112 — changing a saved payee's account number is refused without a passkey, and with one signed for other
// details; with the owner's passkey it changes, the name is checked afresh at the new bank, and a payment to the payee
// within 24 hours of the change is held with nothing on the ledger. After 24 hours it is paid.
func TestPayeeDetailsChange_NeedsAPasskeyAndHoldsPaymentsFor24Hours(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b41112-details", "app.talyvor.com", "https://app.talyvor.com"
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
	b.store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	r := chi.NewRouter()
	mountMoneyPayeeRoutes(r, b.store)
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
	account := func() string {
		t.Helper()
		var sort, number string
		if err := pool.QueryRow(ctx, `SELECT sort_code, account_number FROM money_payees WHERE id = $1`, b.payee.ID).Scan(&sort, &number); err != nil {
			t.Fatal(err)
		}
		return sort + " " + number
	}
	paid := func(key string) string { return b468Postings(t, pool, ws, key) }

	b.pay(t, "before", 2000)
	if got, want := paid("before"), "payment_out_hold agent -2000; payment_out_hold hold 2000; payment_out hold -2000; payment_out partner 2000"; got != want {
		t.Fatalf("the payment before any change posted %q, want %q", got, want)
	}

	// A payment made to the old details that the partner never answered: it waits, its money held, to be sent again.
	unsent := b.pay(t, "unsent", 2015)
	if _, err := pool.Exec(ctx, `UPDATE money_payments SET status = 'pending', partner_ref = '' WHERE id = $1`, unsent.ID); err != nil {
		t.Fatal(err)
	}

	// The owner's passkey, the workspace's only key (the fixture's phone, there for pushes, is removed so the first
	// passkey needs no other key's signature, B50.20): this test is the authenticator.
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_device_keys WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := b.store.PasskeyRegistrationChallenge(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	attested := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	attested = append(append(binary.BigEndian.AppendUint16(append(attested, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
	if _, err := b.store.RegisterPasskey(ctx, ws, "Owner's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}, nil); err != nil {
		t.Fatal(err)
	}
	counter := uint32(0)
	signed := func(details, challenge string) string {
		counter++
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		a, _ := json.Marshal(passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)})
		return strings.TrimSuffix(details, "}") + `,"assertion":` + string(a) + `}`
	}
	const newDetails, otherDetails = `{"sort_code":"40 00 00","account_number":"12345678"}`, `{"sort_code":"40 00 00","account_number":"87654321"}`
	path := "/v1/money/payees/" + b.payee.ID + "/details"

	code, ch := call(path+"/challenge", newDetails)
	if code != http.StatusOK || !strings.Contains(jsonOf(ch["allow_credentials"]), b64(credID)) || !strings.Contains(jsonOf(ch["notice"]), "a number you already know") {
		t.Fatalf("the details challenge = %d %v, want 200 with the passkey and the notice", code, ch)
	}
	if code, out := call(path, newDetails); code != http.StatusForbidden {
		t.Errorf("a change with no passkey = %d %v, want 403", code, out)
	}
	if code, out := call(path, signed(otherDetails, ch["challenge"].(string))); code != http.StatusForbidden {
		t.Errorf("a change to other details than the ones signed for = %d %v, want 403", code, out)
	}
	if got := account(); got != "20-00-00 55779911" {
		t.Fatalf("after two refused changes the payee's account is %q", got)
	}

	code, out := call(path, signed(newDetails, ch["challenge"].(string)))
	if code != http.StatusOK || out["sort_code"] != "40-00-00" || out["account_number"] != "12345678" || out["check"] != "exact_match" ||
		out["checked_by"] != "test" || !strings.Contains(jsonOf(out["notice"]), "a number you already know") {
		t.Fatalf("the change with the passkey = %d %v, want 200, the new account checked afresh, and the notice", code, out)
	}
	held, err := time.Parse(time.RFC3339, out["held_until"].(string))
	if err != nil || time.Until(held) < 23*time.Hour || time.Until(held) > 25*time.Hour {
		t.Fatalf("held_until = %v (%v), want 24 hours from now", out["held_until"], err)
	}
	if got := account(); got != "40-00-00 12345678" {
		t.Fatalf("the payee's stored account is %q after the change", got)
	}

	_, err = b.store.PayOutside(ctx, ws, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: 2000,
		Reference: "Invoice during", IdempotencyKey: "during"})
	if !errors.Is(err, economy.ErrPayeeDetailsHeld) || !errors.Is(err, economy.ErrPayeeUnconfirmed) || !strings.Contains(err.Error(), "a number you already know") {
		t.Fatalf("a payment within 24 hours of the change = %v, want it held", err)
	}
	if got := paid("during"); got != "" {
		t.Fatalf("the held payment posted %q, want nothing on the ledger", got)
	}

	// The payment made to the old details is never sent to the new ones: it fails and its money comes back. (Here the
	// sandbox, which saw its key with the old details, would refuse it too; RefreshOutsidePayment fails it before asking.)
	if read, err := b.store.RefreshOutsidePayment(ctx, ws, "", unsent.ID); err != nil || read.Status != economy.PaymentFailed {
		t.Fatalf("sending again a payment made before the change = %+v, %v; want it failed, not sent to the new details", read, err)
	}
	if got, want := paid("unsent"), "payment_out_hold agent -2015; payment_out_hold hold 2015; payment_out_failed hold -2015; payment_out_failed agent 2015"; got != want {
		t.Fatalf("the payment made before the change posted %q, want %q", got, want)
	}

	if _, err := pool.Exec(ctx, `UPDATE money_payees SET details_changed_at = now() - interval '25 hours' WHERE id = $1`, b.payee.ID); err != nil {
		t.Fatal(err)
	}
	b.pay(t, "after", 2000)
	if got, want := paid("after"), "payment_out_hold agent -2000; payment_out_hold hold 2000; payment_out hold -2000; payment_out partner 2000"; got != want {
		t.Fatalf("the payment 25 hours after the change posted %q, want %q", got, want)
	}
}
