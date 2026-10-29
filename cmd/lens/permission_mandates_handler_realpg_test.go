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
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B40.2 — a signed mandate allowing two payees up to £100 each and £250 a day: £90 to one of them is allowed, £120
// needs approval and a third payee is blocked; the same £90 asked for by voice needs approval; an unsigned or altered
// mandate is refused, one altered after it was kept is blocked, and once revoked all three are blocked.
func TestPermissionMandates_SignedEvaluatedAndRevoked(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b402-mandates", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	r := chi.NewRouter()
	mountPermissionMandateRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
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

	// The person's passkey: this test is the authenticator.
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
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterPasskey(ctx, ws, "Ada's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: authData(0x45, 0, true)}, nil); err != nil {
		t.Fatal(err)
	}
	signed := func(canonical, challenge string, count uint32) string {
		cdj, ad := clientData("webauthn.get", challenge), authData(0x05, count, false)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return jsonOf(map[string]any{"canonical": canonical, "assertion": map[string]string{"credential_id": b64(credID),
			"client_data_json": b64(cdj), "authenticator_data": b64(ad), "signature": b64(sig)}})
	}

	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	code, prep := call(http.MethodPost, "/v1/money/permission-mandates/prepare", `{"principal":{"type":"person","name":"Ada Lovelace"},
		"clients":["chat","voice"],"scope":{"payees":["payee-plumber","payee-grocer"],"currencies":["gbp"]},
		"limits":{"per_payment":{"GBP":10000},"per_day":{"GBP":25000}},"new_payee":{"policy":"always_ask"},"expires_at":"`+expires+`"}`)
	if code != http.StatusOK || prep["challenge"] != economy.MandateChallenge(prep["canonical"].(string)) {
		t.Fatalf("prepare = %d %v", code, prep)
	}
	canonical := prep["canonical"].(string)
	if !strings.Contains(canonical, `"principal":{"type":"person","id":"ada","name":"Ada Lovelace"}`) || !strings.Contains(canonical, `"currencies":["GBP"]`) {
		t.Errorf("the canonical mandate is %s", canonical)
	}

	if code, out := call(http.MethodPost, "/v1/money/permission-mandates", jsonOf(map[string]any{"canonical": canonical})); code != http.StatusBadRequest {
		t.Errorf("an unsigned mandate = %d %v, want 400", code, out)
	}
	altered := strings.Replace(canonical, `"per_payment":{"GBP":10000}`, `"per_payment":{"GBP":99900}`, 1)
	if code, out := call(http.MethodPost, "/v1/money/permission-mandates", signed(altered, prep["challenge"].(string), 1)); code != http.StatusBadRequest {
		t.Errorf("a mandate altered after its challenge was signed = %d %v, want 400", code, out)
	}
	code, m := call(http.MethodPost, "/v1/money/permission-mandates", signed(canonical, prep["challenge"].(string), 2))
	if code != http.StatusCreated || m["status"] != "active" {
		t.Fatalf("the signed mandate = %d %v, want 201 active", code, m)
	}
	id := m["id"].(string)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM permission_mandates WHERE id = $1 AND workspace_id = $2 AND canonical = $3`, id, ws, canonical).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the kept mandate rows = %d (%v), want 1 with the signed JSON", n, err)
	}

	judge := func(payment, decision, rule string) {
		t.Helper()
		code, d := call(http.MethodPost, "/v1/money/permission-mandates/"+id+"/evaluate", payment)
		if code != http.StatusOK || d["decision"] != decision || d["rule"] != rule {
			t.Errorf("evaluate %s = %d %v, want %s by %q", payment, code, d, decision, rule)
		}
	}
	pay90 := `{"client":"chat","payee_id":"payee-plumber","currency":"GBP","amount_minor":9000}`
	pay120 := `{"client":"chat","payee_id":"payee-plumber","currency":"GBP","amount_minor":12000}`
	payOther := `{"client":"chat","payee_id":"payee-stranger","currency":"GBP","amount_minor":9000}`
	judge(pay90, economy.MandateAllow, "")
	judge(pay120, economy.MandateNeedsApproval, "limits.per_payment")
	judge(payOther, economy.MandateBlock, "scope")
	judge(`{"client":"voice","payee_id":"payee-plumber","currency":"GBP","amount_minor":9000}`, economy.MandateNeedsApproval, "clients.voice")
	judge(`{"client":"chat","payee_id":"payee-grocer","currency":"GBP","amount_minor":9000,"spent_today_minor":17000}`, economy.MandateNeedsApproval, "limits.per_day")

	// Altered where it is kept: the signature no longer covers it, so it pays nothing.
	if _, err := pool.Exec(ctx, `UPDATE permission_mandates SET canonical = replace(canonical, '"GBP":10000', '"GBP":99900') WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	judge(pay90, economy.MandateBlock, "signature")
	if _, err := pool.Exec(ctx, `UPDATE permission_mandates SET canonical = $2 WHERE id = $1`, id, canonical); err != nil {
		t.Fatal(err)
	}
	judge(pay90, economy.MandateAllow, "")

	if code, out := call(http.MethodPost, "/v1/money/permission-mandates/"+id+"/revoke", ""); code != http.StatusOK || out["status"] != "revoked" {
		t.Fatalf("revoke = %d %v", code, out)
	}
	for _, p := range []string{pay90, pay120, payOther} {
		judge(p, economy.MandateBlock, "revoked_at")
	}
	if code, out := call(http.MethodPost, "/v1/money/permission-mandates", signed(canonical, prep["challenge"].(string), 3)); code != http.StatusBadRequest {
		t.Errorf("the revoked mandate signed again = %d %v, want 400", code, out)
	}
	if code, out := call(http.MethodGet, "/v1/money/permission-mandates", ""); code != http.StatusOK || !strings.Contains(jsonOf(out), `"status":"revoked"`) {
		t.Errorf("list = %d %v", code, out)
	}
}
