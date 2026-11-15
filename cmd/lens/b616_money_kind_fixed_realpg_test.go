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
)

// B61.6 — an account's kind of money never changes after it is made: the owner's change from each kind to each other
// kind, signed with the owner's phone key over an open challenge for it and on an account holding nothing, is refused
// 409 kind_fixed with its one sentence, and the kind reads back unchanged; so is asking for the challenge, and a change
// with no signature. (The operator's set, and the provision call that sets the kind, are TestB596_*.)
func TestB616_AnAccountsKindOfMoneyIsFixed(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountWorkspaceMoneyKindRoutes(r, store)
	b64 := base64.RawURLEncoding.EncodeToString
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	sign := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return b64(sig)
	}
	kinds := []string{economy.MoneyKindFiat, economy.MoneyKindStablecoin, economy.MoneyKindHybrid}
	for _, from := range kinds {
		for _, to := range kinds {
			if to == from {
				continue
			}
			ws := "ws-b616-" + from + "-" + to
			if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'personal', $2)`,
				ws, from); err != nil {
				t.Fatal(err)
			}
			call := func(method, path, body string) (int, map[string]any) {
				t.Helper()
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
				req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				out := map[string]any{}
				_ = json.Unmarshal(w.Body.Bytes(), &out)
				return w.Code, out
			}
			_, ch := call(http.MethodPost, "/v1/money/devices/challenge", "")
			regChallenge, _ := ch["challenge"].(string)
			code, dev := call(http.MethodPost, "/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Ada's phone",
				"public_key": b64(spki), "challenge": regChallenge, "signature": sign(regChallenge), "attestation": ""}))
			if code != http.StatusCreated {
				t.Fatalf("%s: register device = %d %v", ws, code, dev)
			}
			// An open challenge for the change, as B59.46 issued it, so the signature below would be good.
			challenge := "b616-" + from + "-" + to
			if _, err := pool.Exec(ctx, `INSERT INTO webauthn_challenges (challenge, workspace_id, purpose, expires_at) VALUES ($1, $2, $3, $4)`,
				challenge, ws, "money-kind:"+to, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			signed := jsonOf(map[string]any{"money_kind": to, "assertion": map[string]string{"device_id": dev["id"].(string),
				"challenge": challenge, "signature": sign(challenge)}})
			for _, c := range []struct{ what, method, path, body string }{
				{"signed", http.MethodPut, "/v1/workspaces/" + ws + "/account/money-kind", signed},
				{"unsigned", http.MethodPut, "/v1/workspaces/" + ws + "/account/money-kind", `{"money_kind":"` + to + `"}`},
				{"the challenge for", http.MethodPost, "/v1/workspaces/" + ws + "/account/money-kind/challenge", `{"money_kind":"` + to + `"}`},
			} {
				code, out := call(c.method, c.path, c.body)
				if code != http.StatusConflict || out["code"] != "kind_fixed" || out["field"] != "kind_fixed" || out["money_kind"] != from ||
					out["error"] != "An account's kind of money is fixed. Open another account to use another kind." {
					t.Errorf("%s %s to %s = %d %v, want 409 kind_fixed naming %s and the sentence", c.what, from, to, code, out, from)
				}
				code, acct := call(http.MethodGet, "/v1/workspaces/"+ws+"/account", "")
				if code != http.StatusOK || acct["money_kind"] != from {
					t.Errorf("after %s %s to %s the account reads %d %v, want still %s", c.what, from, to, code, acct, from)
				}
			}
		}
	}
}
