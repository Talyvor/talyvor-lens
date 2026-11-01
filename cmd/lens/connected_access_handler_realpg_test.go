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
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B41.115 — the access page's one call: a mandate signed for an assistant ("mcp:claude") and the phone that signed it
// are listed with what each may do, when each was last used and when the mandate expires; revoking the mandate through
// the route its row names shows it revoked, and the assistant's next payment request is no longer allowed under it.
func TestConnectedAccess_ListsAndRevokesInOneTap(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b41115-access"
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_plumber', $1, 'Ada Plumbing', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountPermissionMandateRoutes(r, store)
	mountPaymentRequestRoutes(r, store)
	mountConnectedAccessRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "k-"+time.Now().Format(time.RFC3339Nano))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	b64 := base64.RawURLEncoding.EncodeToString

	// The phone registers its key and signs a mandate for Claude.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sign := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, h[:])
		return b64(sig)
	}
	_, ch := call(http.MethodPost, "/v1/money/devices/challenge", "")
	code, dev := call(http.MethodPost, "/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Ada's phone",
		"public_key": b64(spki), "challenge": ch["challenge"].(string), "signature": sign(ch["challenge"].(string)), "attestation": ""}))
	if code != http.StatusCreated {
		t.Fatalf("register device = %d %v", code, dev)
	}
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	code, prep := call(http.MethodPost, "/v1/money/permission-mandates/prepare", `{"principal":{"type":"person","name":"Ada Lovelace"},
		"clients":["mcp:claude"],"scope":{"payees":["payee_plumber"],"currencies":["GBP"]},"limits":{"per_payment":{"GBP":10000}},
		"new_payee":{"policy":"confirmation_of_payee"},"expires_at":"`+expires.Format(time.RFC3339)+`"}`)
	if code != http.StatusOK {
		t.Fatalf("prepare mandate = %d %v", code, prep)
	}
	code, m := call(http.MethodPost, "/v1/money/permission-mandates", jsonOf(map[string]any{"canonical": prep["canonical"],
		"assertion": map[string]string{"device_id": dev["id"].(string), "challenge": prep["challenge"].(string), "signature": sign(prep["challenge"].(string))}}))
	if code != http.StatusCreated {
		t.Fatalf("sign mandate = %d %v", code, m)
	}
	mandateID := m["id"].(string)

	// Claude prepares a payment under it.
	pay := `{"client":"mcp:claude","items":[{"payee_id":"payee_plumber","currency":"GBP","amount_minor":9000,"reference":"INV-1"}]}`
	if code, req := call(http.MethodPost, "/v1/money/payment-requests", pay); code != http.StatusCreated || req["status"] != "auto" {
		t.Fatalf("Claude's payment under the mandate = %d %v, want 201 auto", code, req)
	}

	items := func() map[string]map[string]any {
		t.Helper()
		code, out := call(http.MethodGet, "/v1/money/access", "")
		if code != http.StatusOK {
			t.Fatalf("GET /v1/money/access = %d %v", code, out)
		}
		byKind := map[string]map[string]any{}
		for _, it := range out["items"].([]any) {
			row := it.(map[string]any)
			byKind[row["kind"].(string)] = row
		}
		return byKind
	}
	got := items()
	mandate, device := got["mandate"], got["device"]
	if mandate == nil || mandate["id"] != mandateID || mandate["name"] != "mcp:claude" || mandate["status"] != "active" ||
		mandate["may"] != "prepare and pay 1 payee in GBP, up to GBP 100.00 a payment" || mandate["last_used_at"] == nil ||
		mandate["revoke"] != "/v1/money/permission-mandates/"+mandateID+"/revoke" {
		t.Fatalf("mandate row = %v", mandate)
	}
	if exp, _ := time.Parse(time.RFC3339, mandate["expires_at"].(string)); !exp.Equal(expires) {
		t.Fatalf("mandate expires_at = %v, want %v", mandate["expires_at"], expires)
	}
	if device == nil || device["name"] != "Ada's phone" || device["last_used_at"] == nil || device["status"] != "active" ||
		device["revoke"] != "/v1/money/devices/"+dev["id"].(string)+"/revoke" {
		t.Fatalf("device row = %v", device)
	}

	// One tap: the row's own revoke route.
	if code, out := call(http.MethodPost, mandate["revoke"].(string), ""); code != http.StatusOK {
		t.Fatalf("revoke = %d %v", code, out)
	}
	if st := items()["mandate"]["status"]; st != "revoked" {
		t.Fatalf("after revoke the page shows %v, want revoked", st)
	}
	if code, req := call(http.MethodPost, "/v1/money/payment-requests", pay); code == http.StatusCreated && req["status"] == "auto" {
		t.Fatalf("Claude's next payment still auto under a revoked mandate: %v", req)
	}

	// Only the owner or an admin sees the list.
	owner = &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent"}
	if code, _ := call(http.MethodGet, "/v1/money/access", ""); code != http.StatusForbidden {
		t.Fatalf("an agent's key listing access = %d, want 403", code)
	}
}
