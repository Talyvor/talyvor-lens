package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
)

// verifyWithPublishedKeys is what another platform does with a Talyvor agent's credential, and all it has: the JWKS
// published at /.well-known/talyvor-kya/jwks.json and golang-jwt. Nothing of Lens's.
func verifyWithPublishedKeys(jwks []byte, token string) (jwt.MapClaims, error) {
	var set struct {
		Keys []struct{ Kty, Crv, X, Kid string } `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil {
		return nil, err
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		for _, k := range set.Keys {
			if k.Kid == t.Header["kid"] && k.Kty == "OKP" && k.Crv == "Ed25519" {
				x, err := base64.RawURLEncoding.DecodeString(k.X)
				return ed25519.PublicKey(x), err
			}
		}
		return nil, errors.New("no published key has this kid")
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("talyvor"), jwt.WithExpirationRequired())
	return claims, err
}

// B30.5 — the DONE line: a platform verifies an agent's credential using only the published JWKS; a frozen agent's
// credential fails; after a rule change the credential shows the new limit summary, and the old one is revoked.
func TestKYA_APlatformVerifiesAnAgentsCredentialWithOnlyThePublishedKeys(t *testing.T) {
	pool := agentRoutesDB(t)
	unconfirmedAccounts(t, pool) // B61.52: this test's own checks are the account's
	ctx := context.Background()
	const ws = "ws-kya"
	store := economy.NewDualTokenStore(nil, pool, nil)
	agent, err := store.CreateAgent(ctx, ws, "Research bot", "owner")
	if err != nil {
		t.Fatal(err)
	}
	// The owner's email and phone were checked by the Test provider, their identity by a real one.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 1, 'contact', 'test', 'completed', 'ref-1', ''), ($1, 2, 'person', 'kyc-partner', 'completed', 'ref-2', 'Ada Lovelace')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{MaxPerRequestULXC: 1_000_000, DailyLimitULXC: 5_000_000}); err != nil {
		t.Fatal(err)
	}
	key, _, err := kya.ParseKey("")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc := kya.New(store, key, time.Hour)
	mountKYAPublicRoutes(r, svc)
	mountKYAAgentRoutes(r, svc, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(who *auth.AuthContext, method, path, body string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	credential := func() kya.Credential {
		t.Helper()
		code, body := call(owner, http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+agent.ID+"/credential", "")
		var c kya.Credential
		if err := json.Unmarshal(body, &c); code != http.StatusOK || err != nil {
			t.Fatalf("the agent's credential = %d %s", code, body)
		}
		return c
	}
	verify := func(token string) kya.Verification {
		t.Helper()
		code, body := call(nil, http.MethodPost, "/v1/kya/verify", `{"credential":"`+token+`"}`)
		var v kya.Verification
		if err := json.Unmarshal(body, &v); code != http.StatusOK || err != nil {
			t.Fatalf("verify = %d %s", code, body)
		}
		return v
	}
	published := func(path string) []byte {
		t.Helper()
		code, body := call(nil, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		return body
	}

	// Whoever holds a credential can show it, so another agent's key is not given this one.
	other, err := store.CreateAgent(ctx, ws, "Other bot", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, other.ID, "key-other"); err != nil {
		t.Fatal(err)
	}
	otherKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-other", Scopes: []string{auth.ScopeProxy}}
	if code, body := call(otherKey, http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+agent.ID+"/credential", ""); code != http.StatusForbidden {
		t.Fatalf("another agent's key read this agent's credential: %d %s; want 403", code, body)
	}

	first := credential()
	claims, err := verifyWithPublishedKeys(published("/.well-known/talyvor-kya/jwks.json"), first.Token)
	if err != nil {
		t.Fatalf("a platform holding only the published JWKS could not verify the credential: %v", err)
	}
	said, _ := json.Marshal(claims)
	for _, want := range []string{`"sub":"` + agent.ID + `"`, `"name":"Research bot"`, `"name":"Ada Lovelace"`, `"level":"L2"`,
		`"live_level":"L0"`, `"daily_limit_ulxc":5000000`, `"max_per_request_ulxc":1000000`, `{"capability":"fx","money":"test"}`} {
		if !strings.Contains(string(said), want) {
			t.Errorf("the credential does not say %s: %s", want, said)
		}
	}
	parts := strings.Split(first.Token, ".")
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(said), "5000000", "500000000", 1))) + "." + parts[2]
	if _, err := verifyWithPublishedKeys(published("/.well-known/talyvor-kya/jwks.json"), forged); err == nil {
		t.Fatal("a credential with its daily limit raised by hand verified against the published keys")
	}
	if v := verify(first.Token); !v.Valid {
		t.Fatalf("Talyvor's verify said the fresh credential is not valid: %s", v.Reason)
	}

	// A rule change revokes it, and the credential the agent is given next states the new limit.
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{MaxPerRequestULXC: 1_000_000, DailyLimitULXC: 9_000_000}); err != nil {
		t.Fatal(err)
	}
	if v := verify(first.Token); v.Valid || v.Reason != "revoked: rules changed" {
		t.Fatalf("the credential from before the rule change verified as %+v; want revoked: rules changed", v)
	}
	if !strings.Contains(string(published("/.well-known/talyvor-kya/revoked.json")), `"jti":"`+first.ID+`","reason":"rules changed"`) {
		t.Fatal("the revocation list does not have the credential the rule change revoked")
	}
	second := credential()
	if second.ID == first.ID || second.Claims.Limits.DailyLimitULXC != 9_000_000 {
		t.Fatalf("after the rule change the credential is %s with a daily limit of %d µLXC; want a new one stating 9000000",
			second.ID, second.Claims.Limits.DailyLimitULXC)
	}
	if _, err := verifyWithPublishedKeys(published("/.well-known/talyvor-kya/jwks.json"), second.Token); err != nil {
		t.Fatalf("the new credential does not verify against the published keys: %v", err)
	}
	if again := credential(); again.ID != second.ID {
		t.Fatalf("asked again with nothing changed, the agent was given %s; want the credential it holds, %s", again.ID, second.ID)
	}

	// Freezing the agent revokes its credential: verify fails, the revocation list has it, and it is given no other.
	if err := store.PauseAgent(ctx, ws, agent.ID, "owner froze it"); err != nil {
		t.Fatal(err)
	}
	if v := verify(second.Token); v.Valid || v.Reason != "revoked: frozen" {
		t.Fatalf("a frozen agent's credential verified as %+v; want revoked: frozen", v)
	}
	if !strings.Contains(string(published("/.well-known/talyvor-kya/revoked.json")), `"jti":"`+second.ID+`","reason":"frozen"`) {
		t.Fatal("the revocation list does not have the frozen agent's credential")
	}
	if code, body := call(owner, http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+agent.ID+"/credential", ""); code != http.StatusConflict {
		t.Fatalf("a frozen agent's credential = %d %s; want 409", code, body)
	}

	// Resumed, it is given a new one; deleted with its workspace, that one stays on the revocation list.
	if err := store.ResumeAgent(ctx, ws, agent.ID); err != nil {
		t.Fatal(err)
	}
	third := credential()
	if _, err := pool.Exec(ctx, `DELETE FROM kya_credentials WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(published("/.well-known/talyvor-kya/revoked.json")), `"jti":"`+third.ID+`","reason":"deleted"`) {
		t.Fatal("a credential deleted with its workspace before it expired is not on the revocation list")
	}
}
