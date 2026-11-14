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
)

// B59.46 — changing the kind of money is signed with the owner's passkey or phone key: a change with no signature, or
// one signed over a challenge for another kind, a used or an expired one, is refused and the kind is unchanged; a
// signed fiat to hybrid change lands, and the audit trail names the key that signed it. A workspace with no key at all
// changes nothing either: it adds one first.
func TestB5946_ChangingTheKindOfMoneyIsSigned(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, keyless = "ws-b5946", "ws-b5946-keyless"
	for _, id := range []string{ws, keyless} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'fiat')`, id); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountWorkspaceMoneyKindRoutes(r, store)
	call := func(wsID, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		owner := &auth.AuthContext{WorkspaceID: wsID, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	b64 := base64.RawURLEncoding.EncodeToString
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	sign := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return b64(sig)
	}
	_, ch := call(ws, http.MethodPost, "/v1/money/devices/challenge", "")
	regChallenge, _ := ch["challenge"].(string)
	code, dev := call(ws, http.MethodPost, "/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Ada's phone",
		"public_key": b64(spki), "challenge": regChallenge, "signature": sign(regChallenge), "attestation": ""}))
	if code != http.StatusCreated {
		t.Fatalf("register device = %d %v", code, dev)
	}
	// signedFor is a PUT body for to, signed by the phone over a challenge issued for over.
	signedFor := func(to, over string) string {
		t.Helper()
		code, c := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/account/money-kind/challenge", `{"money_kind":"`+over+`"}`)
		challenge, _ := c["challenge"].(string)
		if code != http.StatusOK || challenge == "" {
			t.Fatalf("money-kind challenge for %s = %d %v, want 200 and a challenge", over, code, c)
		}
		return jsonOf(map[string]any{"money_kind": to, "assertion": map[string]string{"device_id": dev["id"].(string), "challenge": challenge,
			"signature": sign(challenge)}})
	}
	put := func(wsID, body string) (int, map[string]any) {
		t.Helper()
		return call(wsID, http.MethodPut, "/v1/workspaces/"+wsID+"/account/money-kind", body)
	}
	kindOf := func(wsID string) string {
		t.Helper()
		var kind string
		if err := pool.QueryRow(ctx, `SELECT money_kind FROM workspaces WHERE id = $1`, wsID).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		return kind
	}
	refused := func(what string, code int, out map[string]any) {
		t.Helper()
		if code != http.StatusForbidden || kindOf(ws) != economy.MoneyKindFiat {
			t.Fatalf("%s = %d %v, kind %s; want 403 and still fiat", what, code, out, kindOf(ws))
		}
	}

	code, out := put(ws, `{"money_kind":"hybrid"}`)
	refused("fiat to hybrid with no signature", code, out)
	code, out = put(ws, signedFor(economy.MoneyKindHybrid, economy.MoneyKindStablecoin))
	refused("fiat to hybrid signed over a stablecoin challenge", code, out)
	expired := signedFor(economy.MoneyKindHybrid, economy.MoneyKindHybrid)
	if _, err := pool.Exec(ctx, `UPDATE webauthn_challenges SET expires_at = now() - interval '1 second'
		WHERE workspace_id = $1 AND purpose = 'money-kind:hybrid' AND used_at IS NULL`, ws); err != nil {
		t.Fatal(err)
	}
	code, out = put(ws, expired)
	refused("fiat to hybrid signed over an expired challenge", code, out)

	signed := signedFor(economy.MoneyKindHybrid, economy.MoneyKindHybrid)
	if code, out = put(ws, signed); code != http.StatusOK || out["money_kind"] != economy.MoneyKindHybrid || kindOf(ws) != economy.MoneyKindHybrid {
		t.Fatalf("signed fiat to hybrid = %d %v, kind %s; want 200 and hybrid", code, out, kindOf(ws))
	}
	var from, to, signedBy string
	if err := pool.QueryRow(ctx, `SELECT body::jsonb->>'from', body::jsonb->>'to', body::jsonb->>'signed_by' FROM audit_chain
		WHERE workspace_id = $1 AND subject = $2 AND kind = 'workspace.money_kind'`, ws, "workspace:"+ws).Scan(&from, &to, &signedBy); err != nil {
		t.Fatalf("the signed change in the audit trail: %v", err)
	}
	if from != economy.MoneyKindFiat || to != economy.MoneyKindHybrid || signedBy != dev["id"] {
		t.Fatalf("the audit trail says %s to %s signed by %q, want fiat to hybrid signed by %v", from, to, signedBy, dev["id"])
	}
	// The same signature again is spent.
	if code, out = put(ws, signed); code != http.StatusForbidden {
		t.Fatalf("the spent signature again = %d %v, want 403", code, out)
	}
	// A signature for hybrid does not make it fiat.
	if code, out = put(ws, signedFor(economy.MoneyKindFiat, economy.MoneyKindHybrid)); code != http.StatusForbidden || kindOf(ws) != economy.MoneyKindHybrid {
		t.Fatalf("hybrid to fiat signed over a hybrid challenge = %d %v, kind %s; want 403 and still hybrid", code, out, kindOf(ws))
	}

	if code, out = put(keyless, `{"money_kind":"hybrid"}`); code != http.StatusForbidden || kindOf(keyless) != economy.MoneyKindFiat {
		t.Fatalf("a workspace with no key, unsigned = %d %v, kind %s; want 403 and still fiat", code, out, kindOf(keyless))
	}
}
