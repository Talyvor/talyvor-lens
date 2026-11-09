package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/passkey"
)

// B54.16 — a negotiation domain is proved only by the domain's own agent: one signing as the registrable domain
// itself, or a subdomain's whose key the registrable domain's own directory publishes. A user-content or dangling
// subdomain proves nothing and confirms no deal for the business. And the agents that proved a workspace's domain are
// its own: a confirmation signed with one of their keys, whatever domain publishes it, holds nothing.
func TestB5416_ANegotiationDomainIsProvedByTheDomainsOwnAgent(t *testing.T) {
	pool := agentRoutesDB(t)
	const wsS, wsB, rpID, origin = "ws-b5416-seller", "ws-b5416-buyer", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	sigs := agentsig.Store{Pool: pool}

	// listed is a subdomain whose key seller.example publishes; stray is a subdomain seller.example never listed;
	// vendor is a vendor's own agent; buyer signs as the registrable domain buyer.example itself.
	const listed, stray, vendor, buyer = 0, 1, 2, 3
	var dir b547Keys
	var privs []ed25519.PrivateKey
	for _, o := range []string{"https://agent.seller.example", "https://pages.seller.example", "https://vendor.example", "https://buyer.example"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		dir, privs = append(dir, struct {
			origin, kid string
			pub         ed25519.PublicKey
		}{o, kya.JWKOf(pub).Kid, pub}), append(privs, priv)
	}
	publish := func(origin string, agent int) {
		dir = append(dir, struct {
			origin, kid string
			pub         ed25519.PublicKey
		}{origin, dir[agent].kid, dir[agent].pub})
	}
	publish("https://seller.example", listed) // the owner of seller.example lists agent.seller.example
	lensURL, _ := url.Parse("https://lens.example")
	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	mountNegotiationAgentRoutes(r, &agentsig.Verifier{PublicURL: lensURL, Keys: &dir, Nonces: sigs, Businesses: sigs, Now: time.Now}, store)

	as := func(ws, method, path, body string) (int, string) {
		who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), who)))
		return w.Code, w.Body.String()
	}
	nonce := 0
	// signed posts body to path as agent's signed request, signing as origin with agent's key.
	signed := func(agent int, origin, path, body string) int {
		t.Helper()
		nonce++
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		sum := sha256.Sum256([]byte(body))
		req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
		req.Header.Set("Signature-Agent", `"`+origin+`"`)
		now := time.Now()
		params := fmt.Sprintf(`("@method" "@target-uri" "content-digest" "signature-agent");created=%d;expires=%d;keyid=%q;nonce="b5416-%d";tag="web-bot-auth"`,
			now.Unix(), now.Add(2*time.Minute).Unix(), dir[agent].kid, nonce)
		base := "\"@method\": POST\n\"@target-uri\": https://lens.example" + path + "\n" +
			"\"content-digest\": " + req.Header.Get("Content-Digest") + "\n\"signature-agent\": \"" + origin + "\"\n" +
			"\"@signature-params\": " + params
		req.Header.Set("Signature-Input", "sig1="+params)
		req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(privs[agent], []byte(base)))+":")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	prove := func(agent int, ws string) int {
		t.Helper()
		return signed(agent, dir[agent].origin, "/v1/a2a/negotiations/domains", `{"workspace_id":"`+ws+`"}`)
	}
	verified := func(ws string) bool {
		t.Helper()
		_, msg := as(ws, http.MethodPost, "/v1/negotiations/domains", `{"domain":"seller.example"}`)
		return strings.Contains(msg, `"verified":true`)
	}

	// A subdomain seller.example never listed does not prove seller.example; a listed one does.
	if code, msg := as(wsS, http.MethodPost, "/v1/negotiations/domains", `{"domain":"seller.example"}`); code != http.StatusOK {
		t.Fatalf("the seller claiming seller.example = %d %s", code, msg)
	}
	if code := prove(stray, wsS); code != http.StatusForbidden || verified(wsS) {
		t.Errorf("an unlisted subdomain proving seller.example = %d (verified %v), want 403 and unverified", code, verified(wsS))
	}
	if code := prove(listed, wsS); code != http.StatusOK || !verified(wsS) {
		t.Fatalf("a subdomain seller.example lists proving it = %d, want 200 and verified", code)
	}
	if code, _ := as(wsB, http.MethodPost, "/v1/negotiations/domains", `{"domain":"buyer.example"}`); code != http.StatusOK || prove(buyer, wsB) != http.StatusOK {
		t.Fatalf("the buyer's own agent proving buyer.example = %d", code)
	}

	sign := map[string]func(string) *passkey.Assertion{wsS: b5018Passkey(t, store, wsS, rpID, origin), wsB: b5018Passkey(t, store, wsB, rpID, origin)}
	mandate := func(ws, counterparty string) string {
		t.Helper()
		body := `{"task":{"kind":"purchase","what":"GPU hours"},"side":"sell","deal":"RFQ-B5416","currency":"GBP",
			"target_minor":900000,"walk_away_minor":600000,"deadline":"` + time.Now().Add(48*time.Hour).Format(time.RFC3339) + `",
			"counterparties":["` + counterparty + `"],"channels":["agent"],"authority":"negotiate"}`
		code, msg := as(ws, http.MethodPost, "/v1/negotiations/prepare", body)
		var p struct{ Canonical, Challenge string }
		if code != http.StatusOK || json.Unmarshal([]byte(msg), &p) != nil {
			t.Fatalf("%s prepare = %d %s", ws, code, msg)
		}
		b, _ := json.Marshal(map[string]any{"canonical": p.Canonical, "assertion": sign[ws](p.Challenge)})
		code, msg = as(ws, http.MethodPost, "/v1/negotiations", string(b))
		var m economy.NegotiationMandate
		if code != http.StatusCreated || json.Unmarshal([]byte(msg), &m) != nil {
			t.Fatalf("%s mandate = %d %s", ws, code, msg)
		}
		return m.ID
	}
	confirmed := func(ws, id string) string {
		t.Helper()
		var m economy.NegotiationMandate
		_, msg := as(ws, http.MethodGet, "/v1/negotiations/"+id, "")
		_ = json.Unmarshal([]byte(msg), &m)
		return m.DealConfirmedBy
	}

	// The buyer sells to seller.example: an unlisted subdomain cannot confirm the deal for seller.example.
	toSeller := mandate(wsB, "seller.example")
	if code := signed(stray, dir[stray].origin, "/v1/a2a/negotiations/"+toSeller+"/deal", `{"deal":"rfq-b5416"}`); code != http.StatusForbidden || confirmed(wsB, toSeller) != "" {
		t.Errorf("an unlisted subdomain confirming seller.example's deal = %d (confirmed by %q), want 403 and unconfirmed", code, confirmed(wsB, toSeller))
	}

	// The seller's agent that proved its domain is its own: signing as vendor.example with that same key, published
	// there too, it does not confirm the seller's own mandate naming vendor.example. The vendor's own agent does.
	publish("https://vendor.example", listed)
	toVendor := mandate(wsS, "vendor.example")
	if code := signed(listed, "https://vendor.example", "/v1/a2a/negotiations/"+toVendor+"/deal", `{"deal":"rfq-b5416"}`); code != http.StatusBadRequest || confirmed(wsS, toVendor) != "" {
		t.Errorf("the seller's own agent confirming its own mandate as vendor.example = %d (confirmed by %q), want 400 and unconfirmed", code, confirmed(wsS, toVendor))
	}
	if code := signed(vendor, dir[vendor].origin, "/v1/a2a/negotiations/"+toVendor+"/deal", `{"deal":"rfq-b5416"}`); code != http.StatusOK || confirmed(wsS, toVendor) != "vendor.example" {
		t.Errorf("vendor.example's own agent confirming the seller = %d (confirmed by %q), want 200", code, confirmed(wsS, toVendor))
	}
}
