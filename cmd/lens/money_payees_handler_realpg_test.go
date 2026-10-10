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

// B30.16 — saving a payee checks its name against the account and stores the answer on the payee: an exact match may
// be paid at once, a close match carries the holder's name, and a no-match cannot be paid until the owner confirms it
// with a passkey — a confirmation with no assertion, or one signed over another payee's challenge, changes nothing.
// A sanctioned name is refused and nothing is saved.
func TestMoneyPayeeRoutes_ANoMatchIsPaidOnlyOnceConfirmedWithAPasskey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b3016-payees", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screening.NewScreener(pool, registry))
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	r := chi.NewRouter()
	mountMoneyPayeeRoutes(r, store)
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
	save := func(body, check string) map[string]any {
		t.Helper()
		code, p := call(http.MethodPost, "/v1/money/payees", body)
		if code != http.StatusCreated || p["check"] != check || p["checked_by"] != "test" || p["needs_confirmation"] != (check != "exact_match") {
			t.Fatalf("save %s = %d %v, want 201 checked %s by the Test partner", body, code, p, check)
		}
		return p
	}
	payable := func(id string) error {
		_, err := store.PayableOutsidePayee(ctx, ws, id)
		return err
	}

	exact := save(`{"name":"Ada  Lovelace","country":"gb","sort_code":"20 00 00","account_number":"55779911"}`, "exact_match")
	if exact["name"] != "Ada Lovelace" || exact["sort_code"] != "20-00-00" || exact["country"] != "GB" {
		t.Errorf("the exact match was saved as %v", exact)
	}
	if err := payable(exact["id"].(string)); err != nil {
		t.Errorf("an exact match is not payable: %v", err)
	}
	closeMatch := save(`{"name":"Grace TESTCLOSE Hopper","country":"US","routing_number":"021000021","account_number":"123456789"}`, "close_match")
	if closeMatch["suggested_name"] != "Grace Hopper" {
		t.Errorf("the close match suggests %v, want Grace Hopper", closeMatch["suggested_name"])
	}
	noMatch := save(`{"name":"Bob TESTNOMATCH","country":"DE","iban":"de89 3704 0044 0532 0130 00"}`, "no_match")
	id := noMatch["id"].(string)
	var stored string
	if err := pool.QueryRow(ctx, `SELECT check_result FROM money_payees WHERE id = $1`, id).Scan(&stored); err != nil || stored != "no_match" {
		t.Fatalf("the payee row's check is %q (%v), want no_match", stored, err)
	}
	if err := payable(id); !errors.Is(err, economy.ErrPayeeUnconfirmed) {
		t.Fatalf("an unconfirmed no-match is payable (%v), want ErrPayeeUnconfirmed", err)
	}
	if code, out := call(http.MethodPost, "/v1/money/payees/"+id+"/challenge", ""); code != http.StatusConflict {
		t.Errorf("a challenge before any passkey = %d %v, want 409", code, out)
	}

	// The owner registers a passkey: this test is the authenticator.
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
	challengeFor := func(payeeID string) string {
		t.Helper()
		code, out := call(http.MethodPost, "/v1/money/payees/"+payeeID+"/challenge", "")
		if code != http.StatusOK || !strings.Contains(jsonOf(out["allow_credentials"]), b64(credID)) {
			t.Fatalf("challenge for %s = %d %v", payeeID, code, out)
		}
		return out["challenge"].(string)
	}
	sign := func(challenge string, count uint32) string {
		cdj, ad := clientData("webauthn.get", challenge), authData(0x05, count, false)
		h := sha256.Sum256(cdj)
		signed := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, signed[:])
		return jsonOf(map[string]any{"assertion": map[string]string{"credential_id": b64(credID), "client_data_json": b64(cdj),
			"authenticator_data": b64(ad), "signature": b64(sig)}})
	}
	confirm := "/v1/money/payees/" + id + "/confirm"

	if code, out := call(http.MethodPost, confirm, `{}`); code != http.StatusForbidden {
		t.Errorf("confirm with no assertion = %d %v, want 403", code, out)
	}
	if code, out := call(http.MethodPost, confirm, sign(challengeFor(closeMatch["id"].(string)), 1)); code != http.StatusForbidden {
		t.Errorf("confirm with the close match's challenge = %d %v, want 403", code, out)
	}
	if err := payable(id); !errors.Is(err, economy.ErrPayeeUnconfirmed) {
		t.Fatalf("after two refused confirmations the no-match is payable (%v)", err)
	}
	code, out := call(http.MethodPost, confirm, sign(challengeFor(id), 2))
	if code != http.StatusOK || out["needs_confirmation"] != false || out["confirmed_by"] != b64(credID) || out["confirmed_at"] == nil || out["check"] != "no_match" {
		t.Fatalf("confirm with a passkey = %d %v, want 200 confirmed by the passkey, its check still no_match", code, out)
	}
	if err := payable(id); err != nil {
		t.Errorf("the confirmed no-match is not payable: %v", err)
	}

	if code, out := call(http.MethodPost, "/v1/money/payees", `{"name":"TESTSANCTION Trading","country":"GB","sort_code":"200000","account_number":"12345678"}`); code != http.StatusForbidden {
		t.Errorf("a sanctioned payee = %d %v, want 403", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/payees", `{"name":"Eve","country":"FR","iban":"FR00 0000 0000 0000 0000 0000 000"}`); code != http.StatusBadRequest {
		t.Errorf("an IBAN whose check digits are wrong = %d %v, want 400", code, out)
	}
	if code, out := call(http.MethodGet, "/v1/money/payees", ""); code != http.StatusOK || len(out["payees"].([]any)) != 3 {
		t.Errorf("list = %d %v, want the three payees saved, and neither refused one", code, out)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
