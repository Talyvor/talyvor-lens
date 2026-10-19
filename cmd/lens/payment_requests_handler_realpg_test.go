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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B40.3 — under a signed mandate for the plumber and the grocer up to £100 a payment: a batch of £120 to the grocer,
// £50 to a payee no mandate names and £30 to a payee whose name did not match its account gives two items needing
// approval and one blocked with its reason, a stable hash and both links; the same key again is the same request, the
// same key with a changed amount answers 409; £90 to the plumber is auto from chat and needs approval by voice; bank
// details in the arguments are refused; and no ledger row is written.
func TestPaymentRequests_PreparedNeverPaid(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b403-requests", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, registry))
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	r := chi.NewRouter()
	mountPaymentRequestRoutes(r, store)
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

	for _, p := range [][3]string{{"payee_plumber", "Ada Plumbing", "exact_match"}, {"payee_grocer", "Grace Groceries", "exact_match"},
		{"payee_stranger", "Carol Consulting", "exact_match"}, {"payee_nomatch", "Bob TESTNOMATCH", "no_match"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
			VALUES ($1, $2, $3, 'GB', '20-00-00', '55779911', $4, 'test')`, p[0], ws, p[1], p[2]); err != nil {
			t.Fatal(err)
		}
	}

	// The person's passkey signs the mandate: this test is the authenticator.
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
	canonical, challenge, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{"chat", "voice"},
		Scope:  economy.MandateScope{Payees: []string{"payee_plumber", "payee_grocer"}, Currencies: []string{"GBP"}},
		Limits: economy.MandateLimits{PerPayment: map[string]int64{"GBP": 10000}}, NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee},
		ExpiresAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), 1)
	h := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	mandate, err := store.CreatePermissionMandate(ctx, ws, "ada", canonical, &passkey.Assertion{CredentialID: b64(credID),
		ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)})
	if err != nil {
		t.Fatal(err)
	}

	batch := `{"client":"chat","items":[{"payee_id":"payee_grocer","currency":"GBP","amount_minor":12000,"reference":"INV-1"},
		{"payee_id":"payee_stranger","currency":"gbp","amount_minor":5000,"reference":"INV-2"},
		{"payee_id":"payee_nomatch","currency":"GBP","amount_minor":3000,"reference":"INV-3"}]}`
	code, first := call(http.MethodPost, "/v1/money/payment-requests", "batch-1", batch)
	if code != http.StatusCreated || first["status"] != "pending_approval" || first["kind"] != "batch" {
		t.Fatalf("the batch = %d %v, want 201 pending_approval", code, first)
	}
	id := first["id"].(string)
	if first["approval_url"] != "https://app.talyvor.com/approve/"+id || first["deep_link"] != "talyvor://approve/"+id {
		t.Errorf("the links are %v and %v", first["approval_url"], first["deep_link"])
	}
	if first["payload_hash"] != hex256(first["canonical"].(string)) || !strings.Contains(first["canonical"].(string),
		`{"payee_id":"payee_stranger","listing_id":"","currency":"GBP","amount_minor":5000,"reference":"INV-2"}`) {
		t.Errorf("the frozen payload %v does not hash to %v", first["canonical"], first["payload_hash"])
	}
	items := first["items"].([]any)
	want := [][3]string{{"needs_approval", "limits.per_payment", mandate.ID}, {"needs_approval", "scope", mandate.ID}, {"blocked", "payee", ""}}
	for i, w := range want {
		it := items[i].(map[string]any)
		if it["decision"] != w[0] || it["rule"] != orNil(w[1]) || it["mandate_id"] != orNil(w[2]) || it["reason"] == "" {
			t.Errorf("item %d = %v, want %s by %q", i+1, it, w[0], w[1])
		}
	}
	if reason := items[2].(map[string]any)["reason"].(string); !strings.Contains(reason, "did not match its account") {
		t.Errorf("the blocked payee's reason is %q", reason)
	}

	code, again := call(http.MethodPost, "/v1/money/payment-requests", "batch-1", batch)
	if code != http.StatusOK || again["id"] != id || again["payload_hash"] != first["payload_hash"] {
		t.Errorf("the same key again = %d %v, want 200 and request %s", code, again["id"], id)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests", "batch-1", strings.Replace(batch, "12000", "12001", 1)); code != http.StatusConflict {
		t.Errorf("the same key with a changed amount = %d %v, want 409", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests", "", batch); code != http.StatusBadRequest {
		t.Errorf("no Idempotency-Key = %d %v, want 400", code, out)
	}

	plumber := `{"client":"%s","items":[{"payee_id":"payee_plumber","currency":"GBP","amount_minor":9000}]}`
	code, chat := call(http.MethodPost, "/v1/money/payment-requests", "chat-1", strings.Replace(plumber, "%s", "chat", 1))
	if it := chat["items"].([]any)[0].(map[string]any); code != http.StatusCreated || chat["status"] != "auto" || it["decision"] != "auto" || it["mandate_id"] != mandate.ID {
		t.Errorf("£90 to the plumber from chat = %d %v, want auto under the mandate", code, chat)
	}
	code, voice := call(http.MethodPost, "/v1/money/payment-requests", "voice-1", strings.Replace(plumber, "%s", "voice", 1))
	if it := voice["items"].([]any)[0].(map[string]any); code != http.StatusCreated || voice["status"] != "pending_approval" || it["rule"] != "clients.voice" {
		t.Errorf("£90 to the plumber by voice = %d %v, want needs approval by clients.voice", code, voice)
	}

	code, out := call(http.MethodPost, "/v1/money/payment-requests", "bank-1",
		`{"client":"chat","items":[{"payee_id":"payee_plumber","currency":"GBP","amount_minor":9000,"iban":"GB33BUKB20201555555555"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "payee details supplied by the assistant") {
		t.Errorf("bank details in the arguments = %d %v, want 400", code, out)
	}

	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+chat["id"].(string)+"/cancel", "", ""); code != http.StatusOK || out["status"] != "cancelled" {
		t.Errorf("cancel = %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/v1/money/payment-requests/"+id, "", ""); code != http.StatusOK || out["payload_hash"] != first["payload_hash"] {
		t.Errorf("status = %d %v", code, out)
	}

	for _, table := range []string{"money_entries", "money_postings", "money_payments", "lxc_ledger", "agent_transfers"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s has %d rows (%v): preparing must write no ledger row", table, n, err)
		}
	}
}

func hex256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// orNil is how an omitted JSON field reads back: nil for "".
func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
