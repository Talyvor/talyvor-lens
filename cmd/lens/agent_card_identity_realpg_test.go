package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/a2acard"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/jcs"
	"github.com/talyvor/lens/internal/kya"
)

// verifyCardWithPublishedKey is what anyone does with a Talyvor Agent Card, and all they have: the card, the JWKS its
// signature's jku names, and RFC 8785. Nothing of Lens's but the canonicaliser, which is checked against the RFC.
func verifyCardWithPublishedKey(jwks, card []byte) error {
	var c map[string]any
	d := json.NewDecoder(bytes.NewReader(card))
	d.UseNumber()
	if err := d.Decode(&c); err != nil {
		return err
	}
	sigs, _ := c["signatures"].([]any)
	if len(sigs) != 1 {
		return errors.New("the card does not carry exactly one signature")
	}
	sig := sigs[0].(map[string]any)
	delete(c, "signatures")
	payload, err := jcs.Value(c)
	if err != nil {
		return err
	}
	h, _ := base64.RawURLEncoding.DecodeString(sig["protected"].(string))
	var header struct{ Alg, Typ, Kid, Jku string }
	if err := json.Unmarshal(h, &header); err != nil || header.Alg != "EdDSA" || header.Typ != "JOSE" || header.Jku == "" {
		return errors.New("the protected header is not {alg EdDSA, typ JOSE, kid, jku}: " + string(h))
	}
	var set kya.JWKS
	if err := json.Unmarshal(jwks, &set); err != nil {
		return err
	}
	for _, k := range set.Keys {
		if k.Kid == header.Kid && k.Kty == "OKP" && k.Crv == "Ed25519" {
			x, _ := base64.RawURLEncoding.DecodeString(k.X)
			s, _ := base64.RawURLEncoding.DecodeString(sig["signature"].(string))
			if !ed25519.Verify(x, []byte(sig["protected"].(string)+"."+base64.RawURLEncoding.EncodeToString(payload)), s) {
				return errors.New("the signature does not verify")
			}
			return nil
		}
	}
	return errors.New("no published key has this kid")
}

// B45.23 — the DONE line: a card verifies with the published key, and fails after one changed byte or after its key
// is revoked; Talyvor's own card verifies with its own.
func TestAgentCard_VerifiesWithThePublishedKeyAndFailsAfterAChangedByteOrRevocation(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agent-card"
	store := economy.NewDualTokenStore(nil, pool, nil)
	agent, err := store.CreateAgent(ctx, ws, "Research bot", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 1, 'contact', 'test', 'completed', 'ref-1', ''), ($1, 2, 'person', 'kyc-partner', 'completed', 'ref-2', 'Ada Lovelace')`, ws); err != nil {
		t.Fatal(err)
	}
	key, _, err := kya.ParseKey("")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc := a2acard.New(store, key.Derive, "https://lens.example")
	mountAgentCardPublicRoutes(r, svc)
	mountAgentCardOwnerRoutes(r, svc)
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
	get := func(path string) []byte {
		t.Helper()
		code, body := call(nil, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		return body
	}
	verify := func(card []byte) a2acard.Verification {
		t.Helper()
		code, body := call(nil, http.MethodPost, "/v1/a2a/verify", string(card))
		var v a2acard.Verification
		if err := json.Unmarshal(body, &v); code != http.StatusOK || err != nil {
			t.Fatalf("verify = %d %s", code, body)
		}
		return v
	}
	cardPath, jwksPath := "/v1/a2a/agents/"+agent.ID+"/agent-card.json", "/v1/a2a/agents/"+agent.ID+"/jwks.json"

	card := get(cardPath)
	if err := verifyCardWithPublishedKey(get(jwksPath), card); err != nil {
		t.Fatalf("anyone holding only the published key could not verify the card: %v\n%s", err, card)
	}
	for _, want := range []string{`"name":"Research bot"`, `"organization":"Ada Lovelace"`, `"level":"L2"`, `"liveLevel":"L0"`, `to L0 with live ones`,
		`"agentId":"` + agent.ID + `"`, `"jku":"https://lens.example/v1/a2a/agents/` + agent.ID + `/jwks.json"`, `"supportedInterfaces":[]`} {
		header := ""
		var c a2acard.Card
		if json.Unmarshal(card, &c) == nil && len(c.Signatures) == 1 {
			h, _ := base64.RawURLEncoding.DecodeString(c.Signatures[0].Protected)
			header = string(h)
		}
		if !strings.Contains(string(card), want) && !strings.Contains(header, want) {
			t.Errorf("the card does not say %s: %s %s", want, card, header)
		}
	}
	if v := verify(card); !v.Valid || v.SignedBy != agent.ID || v.Principal == nil || v.Principal.Name != "Ada Lovelace" {
		t.Fatalf("Talyvor did not vouch for its own card: %+v", v)
	}

	// One changed byte: the name the card gives its owner.
	forged := bytes.Replace(card, []byte(`"organization":"Ada Lovelace"`), []byte(`"organization":"Ada Lovelacf"`), 1)
	if bytes.Equal(forged, card) {
		t.Fatal("the forgery changed nothing")
	}
	if err := verifyCardWithPublishedKey(get(jwksPath), forged); err == nil {
		t.Fatal("a card with one byte changed verified against the published key")
	}
	if v := verify(forged); v.Valid || !strings.Contains(v.Reason, "changed") {
		t.Fatalf("Talyvor vouched for a card with one byte changed: %+v", v)
	}

	// Another workspace's owner cannot rotate this agent's key.
	stranger := &auth.AuthContext{WorkspaceID: "ws-other", AuthMethod: auth.MethodJWT, UserID: "x", Scopes: []string{auth.ScopeKeys}}
	if code, body := call(stranger, http.MethodPost, "/v1/workspaces/ws-other/agents/"+agent.ID+"/agent-card/rotate", ""); code != http.StatusNotFound {
		t.Fatalf("another workspace rotated this agent's key: %d %s; want 404", code, body)
	}

	// Rotating revokes the old key: the old card fails, the new one verifies with the newly published key.
	oldJWKS := get(jwksPath)
	code, rotated := call(owner, http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+agent.ID+"/agent-card/rotate", "")
	if code != http.StatusOK {
		t.Fatalf("rotate = %d %s", code, rotated)
	}
	if v := verify(card); v.Valid || !strings.HasPrefix(v.Reason, "revoked") {
		t.Fatalf("a card signed with a revoked key was vouched for: %+v", v)
	}
	if bytes.Equal(get(jwksPath), oldJWKS) {
		t.Fatal("the revoked key is still the one published")
	}
	if err := verifyCardWithPublishedKey(get(jwksPath), card); err == nil {
		t.Fatal("the card signed with the revoked key verified against the key published now")
	}
	if err := verifyCardWithPublishedKey(get(jwksPath), get(cardPath)); err != nil {
		t.Fatalf("the card signed with the new key does not verify: %v", err)
	}
	if v := verify(get(cardPath)); !v.Valid {
		t.Fatalf("the card signed with the new key was not vouched for: %+v", v)
	}

	// Talyvor's own card, at the A2A well-known path.
	own := get("/.well-known/agent-card.json")
	if err := verifyCardWithPublishedKey(get("/.well-known/talyvor-agent-cards/jwks.json"), own); err != nil {
		t.Fatalf("Talyvor's own card does not verify with its published key: %v\n%s", err, own)
	}
	if v := verify(own); !v.Valid || v.SignedBy != "talyvor" {
		t.Fatalf("Talyvor did not vouch for its own card: %+v", v)
	}

	// An archived agent has no card.
	if _, err := pool.Exec(ctx, `UPDATE agent_accounts SET archived_at = now() WHERE id = $1`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := call(nil, http.MethodGet, cardPath, ""); code != http.StatusNotFound {
		t.Fatalf("an archived agent's card = %d %s; want 404", code, body)
	}
}
