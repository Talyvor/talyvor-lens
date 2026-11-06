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
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B54.33 — in a workspace with the phone's device key, a person's payment is paid only with that key's signature over
// the challenge Lens issued for exactly that payment: unsigned, signed by another key, or signed for another amount it
// pays nothing (nothing on the ledger); signed, it pays once, and the same request again answers that payment.
func TestPhonePayment_PaysOnlyWithTheDeviceSignatureOverThatPayment(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5433-signed-pay"
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry { return sandboxMoneyLayer(t, pool, store, reg) })
	b64 := base64.RawURLEncoding.EncodeToString
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	if _, err := pool.Exec(ctx, `UPDATE workspace_device_keys SET public_key = $2 WHERE id = $1`, "dev_"+ws, spki); err != nil {
		t.Fatal(err)
	}
	sign := func(key *ecdsa.PrivateKey, challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, h[:])
		return b64(sig)
	}

	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, b.store)
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
	payment := func(key string, minor int) string {
		return `{"payee_id":"` + b.payee.ID + `","account_id":"` + b.acct.ID + `","currency":"GBP","amount_minor":` + jsonOf(minor) +
			`,"reference":"Invoice","idempotency_key":"` + key + `"}`
	}
	signed := func(body string, key *ecdsa.PrivateKey, challenge string) string {
		a, _ := json.Marshal(map[string]string{"device_id": "dev_" + ws, "challenge": challenge, "signature": sign(key, challenge)})
		return strings.TrimSuffix(body, "}") + `,"assertion":` + string(a) + `}`
	}
	paid := func(key string) string { return b468Postings(t, pool, ws, key) }

	code, ch := call("/v1/money/payments/challenge", payment("k1", 2000))
	challenge, _ := ch["challenge"].(string)
	if code != http.StatusOK || challenge == "" {
		t.Fatalf("the payment challenge = %d %v, want 200 with a challenge", code, ch)
	}
	for name, body := range map[string]string{
		"unsigned":                   payment("k1", 2000),
		"signed by another key":      signed(payment("k1", 2000), other, challenge),
		"signed for another amount":  signed(payment("k1", 9000), phone, challenge),
		"signed for another payment": signed(payment("k2", 2000), phone, challenge),
	} {
		if code, out := call("/v1/money/payments", body); code != http.StatusForbidden {
			t.Errorf("%s = %d %v, want 403", name, code, out)
		}
	}
	if got := paid("k1") + paid("k2"); got != "" {
		t.Fatalf("the refused payments posted %q, want nothing on the ledger", got)
	}
	b.balance(t, 50000)

	body := signed(payment("k1", 2000), phone, challenge)
	code, out := call("/v1/money/payments", body)
	if code != http.StatusCreated {
		t.Fatalf("the payment signed by the phone = %d %v, want 201", code, out)
	}
	if got, want := paid("k1"), "payment_out_hold agent -2000; payment_out_hold hold 2000; payment_out hold -2000; payment_out partner 2000"; got != want {
		t.Fatalf("the signed payment posted %q, want %q", got, want)
	}
	if code, again := call("/v1/money/payments", body); code != http.StatusCreated || again["id"] != out["id"] {
		t.Errorf("the same signed request again = %d %v, want the same payment", code, again)
	}
	b.balance(t, 48000)
	if code, out := call("/v1/money/payments", signed(payment("k3", 2000), phone, challenge)); code != http.StatusForbidden || paid("k3") != "" {
		t.Errorf("a new payment on the spent challenge = %d %v (posted %q), want 403 and nothing", code, out, paid("k3"))
	}
}
