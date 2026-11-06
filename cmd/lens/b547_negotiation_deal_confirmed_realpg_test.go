package main

import (
	"context"
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
	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/passkey"
)

// B54.7 — a deal reference holds a deal only once the counterparty's own signed agent confirms it, to a workspace that
// proved the domain it trades as. An unconfirmed reference blocks no one; a workspace's own agent confirming its own
// mandate holds nothing; a deal is the reference and its two parties, so a squatter's confirmed reference blocks no
// stranger who shares it. A confirmed deal blocks another of the counterparty's bidders, whichever of its subdomains
// signs, and the other party's own mandate, and the refusal is byte for byte an invalid-mandate refusal. A mandate is
// confirmed once, and not once it has ended.
func TestB547_ADealHoldsOnlyOnceTheCounterpartyConfirmsIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const wsA, wsB, wsC, wsD, rpID, origin = "ws-b547-buyer", "ws-b547-rival", "ws-b547-squatter", "ws-b547-acme", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	sigs := agentsig.Store{Pool: pool}
	const acme, bot, proc, buyer, alias = 0, 1, 2, 3, 4
	var dir b547Keys
	var privs []ed25519.PrivateKey
	for _, o := range []string{"https://agent.acme.example", "https://bot.example", "https://procurement.acme.example", "https://agent.buyer.example", "https://alias.example"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		dir, privs = append(dir, struct {
			origin, kid string
			pub         ed25519.PublicKey
		}{o, kya.JWKOf(pub).Kid, pub}), append(privs, priv)
	}
	lensURL, _ := url.Parse("https://lens.example")
	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	mountNegotiationOutreachRoutes(r, store, nil, "")
	mountNegotiationAgentRoutes(r, &agentsig.Verifier{PublicURL: lensURL, Keys: dir, Nonces: sigs, Businesses: sigs, Now: time.Now}, store)

	as := func(ws string) func(method, path, body string) (int, string) {
		who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
		return func(method, path, body string) (int, string) {
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), who)))
			return w.Code, w.Body.String()
		}
	}
	sign := map[string]func(string) *passkey.Assertion{}
	for _, ws := range []string{wsA, wsB, wsC, wsD} {
		sign[ws] = b5018Passkey(t, store, ws, rpID, origin)
	}
	// side prepares, signs and keeps ws's mandate on side of deal RFQ-ACME-7, with counterparties (a JSON list's body).
	side := func(ws, counterparties, side string) (int, string, economy.NegotiationMandate) {
		t.Helper()
		amounts := map[string][2]string{"buy": {"500000", "800000"}, "sell": {"900000", "600000"}}[side]
		target, walkAway := amounts[0], amounts[1]
		body := `{"task":{"kind":"purchase","what":"GPU hours"},"side":"` + side + `","deal":"RFQ-ACME-7","currency":"GBP",
			"target_minor":` + target + `,"walk_away_minor":` + walkAway + `,"deadline":"` + time.Now().Add(48*time.Hour).Format(time.RFC3339) + `",
			"counterparties":[` + counterparties + `],"channels":["agent"],"authority":"negotiate"}`
		code, msg := as(ws)(http.MethodPost, "/v1/negotiations/prepare", body)
		var p struct{ Canonical, Challenge string }
		if code != http.StatusOK || json.Unmarshal([]byte(msg), &p) != nil {
			t.Fatalf("%s prepare = %d %s", ws, code, msg)
		}
		b, _ := json.Marshal(map[string]any{"canonical": p.Canonical, "assertion": sign[ws](p.Challenge)})
		code, msg = as(ws)(http.MethodPost, "/v1/negotiations", string(b))
		var m economy.NegotiationMandate
		_ = json.Unmarshal([]byte(msg), &m)
		return code, msg, m
	}
	nonce := 0
	// signed posts body to path as agent's signed request; agent < 0 sends it unsigned.
	signed := func(agent int, path, body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if agent >= 0 {
			nonce++
			sum := sha256.Sum256([]byte(body))
			req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
			req.Header.Set("Signature-Agent", `"`+dir[agent].origin+`"`)
			now := time.Now()
			params := fmt.Sprintf(`("@method" "@target-uri" "content-digest" "signature-agent");created=%d;expires=%d;keyid=%q;nonce="b547-%d";tag="web-bot-auth"`,
				now.Unix(), now.Add(2*time.Minute).Unix(), dir[agent].kid, nonce)
			base := "\"@method\": POST\n\"@target-uri\": https://lens.example" + path + "\n" +
				"\"content-digest\": " + req.Header.Get("Content-Digest") + "\n\"signature-agent\": \"" + dir[agent].origin + "\"\n" +
				"\"@signature-params\": " + params
			req.Header.Set("Signature-Input", "sig1="+params)
			req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(privs[agent], []byte(base)))+":")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	confirm := func(agent int, id, deal string) int {
		t.Helper()
		return signed(agent, "/v1/a2a/negotiations/"+id+"/deal", `{"deal":"`+deal+`"}`)
	}
	prove := func(agent int, ws string) int {
		t.Helper()
		return signed(agent, "/v1/a2a/negotiations/domains", `{"workspace_id":"`+ws+`"}`)
	}

	// A domain the owner claims is verified only by that domain's own agent.
	if code, msg := as(wsA)(http.MethodPost, "/v1/negotiations/domains", `{"domain":"Buyer.example"}`); code != http.StatusOK || !strings.Contains(msg, `"domain":"buyer.example","verified":false`) {
		t.Fatalf("the buyer claiming buyer.example = %d %s", code, msg)
	}
	if code := prove(bot, wsA); code != http.StatusNotFound {
		t.Errorf("bot.example verifying a domain the buyer never claimed = %d, want 404", code)
	}
	if code := prove(buyer, wsA); code != http.StatusOK {
		t.Fatalf("buyer.example's agent verifying the buyer's claim = %d, want 200", code)
	}
	if code, msg := as(wsA)(http.MethodPost, "/v1/negotiations/domains", `{"domain":"buyer.example"}`); code != http.StatusOK || !strings.Contains(msg, `"verified":true`) {
		t.Errorf("the buyer's claim once verified = %d %s", code, msg)
	}
	if code, _ := as(wsC)(http.MethodPost, "/v1/negotiations/domains", `{"domain":"bot.example"}`); code != http.StatusOK || prove(bot, wsC) != http.StatusOK {
		t.Fatalf("the squatter proving bot.example = %d", code)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO negotiation_domains (workspace_id, domain, claimed_by, verified_at) VALUES ($1, 'acme.example', 'test', now())`, wsD); err != nil {
		t.Fatal(err)
	}

	// The rival's Counter may reach Acme's verified agent: the rival has a verified name, Acme's agent a live credential.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 1, 'contact', 'test', 'completed', 'ref-b547-1', ''), ($1, 2, 'person', 'kyc-partner', 'completed', 'ref-b547-2', 'Rival Ltd')`, wsB); err != nil {
		t.Fatal(err)
	}
	acmeAgent, err := store.CreateAgent(ctx, wsD, "Acme sales", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IssueKYACredential(ctx, wsD, acmeAgent.ID, func(economy.AgentKYAFacts) (economy.KYACredentialRecord, error) {
		return economy.KYACredentialRecord{ID: uuid.NewString(), Kid: "k1", PublicKey: "pk", ClaimsDigest: "d", Token: "t",
			IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	askAcme := `{"counterparty":"acme.example","channel":"agent","to":"` + acmeAgent.ID + `"}`

	// Unconfirmed, the reference blocks no one: the squatter typed it first, and the buyer, a rival and Acme are kept.
	_, _, squat := side(wsC, `"bot.example"`, "buy")
	_, _, squat2 := side(wsC, `"alias.example","buyer.example"`, "sell")
	codeA, msgA, a := side(wsA, `"acme.example","alias.example"`, "buy")
	codeB, msgB, b := side(wsB, `"acme.example"`, "buy")
	codeD, msgD, d := side(wsD, `"buyer.example"`, "sell")
	if squat.ID == "" || squat2.ID == "" || codeA != http.StatusCreated || codeB != http.StatusCreated || codeD != http.StatusCreated {
		t.Fatalf("an unconfirmed deal reference blocked someone: squatter %q %q, buyer %d %s, rival %d %s, Acme %d %s",
			squat.ID, squat2.ID, codeA, msgA, codeB, msgB, codeD, msgD)
	}

	// The squatter's own agent confirming its own mandate holds nothing. Confirmed by another domain it controls, its
	// deal is the reference and its own parties: Acme still makes an offer on that reference, and naming the buyer's
	// domain in its own list does not make the squatter the buyer's party (Acme confirms the buyer below).
	if code := confirm(bot, squat.ID, "rfq-acme-7"); code != http.StatusBadRequest {
		t.Errorf("the squatter's own agent confirming its own mandate = %d, want 400", code)
	}
	if code := confirm(alias, squat2.ID, "rfq-acme-7"); code != http.StatusOK {
		t.Fatalf("alias.example confirming the squatter's other mandate = %d, want 200", code)
	}
	sellOffer := `{"counterparty":"buyer.example","party":"ours","channel":"agent","terms":{"price_minor":700000}}`
	if code, msg := as(wsD)(http.MethodPost, "/v1/negotiations/"+d.ID+"/terms", sellOffer); code != http.StatusCreated {
		t.Errorf("Acme's offer on a reference only a stranger holds = %d %s, want 201", code, msg)
	}

	if code, msg := as(wsB)(http.MethodPost, "/v1/negotiations/"+b.ID+"/outreach", askAcme); code != http.StatusOK || !strings.Contains(msg, `"action":"ask_for_quote"`) {
		t.Fatalf("the rival asking Acme for a quote in an unconfirmed deal = %d %s, want 200", code, msg)
	}

	// A workspace that proved no domain is confirmed to no one; nor does an agent that is not the mandate's
	// counterparty, an unsigned request, or another reference confirm anything.
	if code := confirm(acme, b.ID, "rfq-acme-7"); code != http.StatusBadRequest {
		t.Errorf("Acme confirming a rival who proved no domain = %d, want 400", code)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO negotiation_domains (workspace_id, domain, claimed_by, verified_at) VALUES ($1, 'rival.example', 'test', now())`, wsB); err != nil {
		t.Fatal(err)
	}
	if code := confirm(bot, a.ID, "rfq-acme-7"); code != http.StatusNotFound {
		t.Errorf("bot.example confirming Acme's deal for the buyer = %d, want 404", code)
	}
	if code := confirm(-1, a.ID, "rfq-acme-7"); code != http.StatusUnauthorized {
		t.Errorf("an unsigned confirmation = %d, want 401", code)
	}
	if code := confirm(acme, a.ID, "rfq-acme-8"); code != http.StatusBadRequest {
		t.Errorf("Acme confirming another reference = %d, want 400", code)
	}

	// Acme's agent confirms the buyer: the deal is theirs, with where the confirmation came from. Acme again, from
	// another subdomain, changes nothing; the mandate's other counterparty cannot confirm it over Acme.
	if code := confirm(acme, a.ID, "RFQ-ACME-7"); code != http.StatusOK {
		t.Fatalf("Acme's agent confirming the buyer = %d, want 200", code)
	}
	if code := confirm(proc, a.ID, "rfq-acme-7"); code != http.StatusOK {
		t.Errorf("Acme confirming the buyer again from another subdomain = %d, want 200", code)
	}
	if code := confirm(alias, a.ID, "rfq-acme-7"); code != http.StatusConflict {
		t.Errorf("a second counterparty confirming an already-confirmed mandate = %d, want 409", code)
	}
	var got economy.NegotiationMandate
	if code, msg := as(wsA)(http.MethodGet, "/v1/negotiations/"+a.ID, ""); code != http.StatusOK || json.Unmarshal([]byte(msg), &got) != nil ||
		got.DealConfirmedBy != "acme.example" || got.DealConfirmedVia != economy.ChannelAgent || got.DealConfirmedAt == nil {
		t.Fatalf("the buyer's mandate after Acme confirmed = %d %s", code, msg)
	}

	// Confirmed, it blocks the rival: Acme cannot confirm them as well, their Counter makes no offer in it, and a new
	// mandate in it is refused exactly as a mandate signed over the wrong challenge is.
	if code := confirm(acme, b.ID, "rfq-acme-7"); code != http.StatusConflict {
		t.Errorf("Acme confirming a second party in its deal = %d, want 409", code)
	}
	if code := confirm(proc, b.ID, "rfq-acme-7"); code != http.StatusConflict {
		t.Errorf("another of Acme's subdomains confirming a second party in its deal = %d, want 409", code)
	}
	// Nor can Counter act for the other party: Acme's own mandate selling to the buyer makes no offer, the buyer
	// cannot confirm it, and a new one is refused like any other; nor for the buyer on both sides.
	if code := confirm(buyer, d.ID, "rfq-acme-7"); code != http.StatusConflict {
		t.Errorf("the buyer confirming Acme's side of the buyer's own held deal = %d, want 409", code)
	}
	if code, msg := as(wsD)(http.MethodPost, "/v1/negotiations/"+d.ID+"/terms", sellOffer); code != http.StatusBadRequest || strings.Contains(msg, wsA) {
		t.Errorf("Acme's offer on the other side of its held deal = %d %s, want 400 naming nobody", code, msg)
	}
	if code, msg, _ := side(wsD, `"buyer.example"`, "sell"); code != http.StatusBadRequest || !strings.Contains(msg, "Counter can't take this negotiation") {
		t.Errorf("a new Acme mandate in its held deal = %d %s, want 400 naming nobody", code, msg)
	}
	if code, msg, _ := side(wsA, `"unrelated.example"`, "sell"); code != http.StatusBadRequest {
		t.Errorf("the buyer's own mandate on the other side of its held deal = %d %s, want 400", code, msg)
	}
	offer := `{"counterparty":"acme.example","party":"ours","channel":"agent","terms":{"price_minor":600000}}`
	if code, msg := as(wsB)(http.MethodPost, "/v1/negotiations/"+b.ID+"/terms", offer); code != http.StatusBadRequest || strings.Contains(msg, wsA) {
		t.Errorf("the rival's offer in a deal confirmed to another = %d %s, want 400 naming nobody", code, msg)
	}
	// Asking Acme for a quote is acting in the deal too: refused naming nobody, and nothing is sent.
	var sentBefore, sentAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM negotiation_messages WHERE mandate_id = $1`, b.ID).Scan(&sentBefore); err != nil {
		t.Fatal(err)
	}
	if code, msg := as(wsB)(http.MethodPost, "/v1/negotiations/"+b.ID+"/outreach", askAcme); code != http.StatusBadRequest || strings.Contains(msg, wsA) {
		t.Errorf("the rival asking Acme for a quote in a deal confirmed to another = %d %s, want 400 naming nobody", code, msg)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM negotiation_messages WHERE mandate_id = $1`, b.ID).Scan(&sentAfter); err != nil || sentAfter != sentBefore {
		t.Errorf("the rival's refused outreach left a message: %d before, %d after, %v", sentBefore, sentAfter, err)
	}
	codeHeld, msgHeld, _ := side(wsB, `"acme.example"`, "buy")
	body := `{"task":{"kind":"purchase","what":"GPU hours"},"side":"buy","currency":"GBP","target_minor":500000,"walk_away_minor":800000,
		"deadline":"` + time.Now().Add(48*time.Hour).Format(time.RFC3339) + `","counterparties":["acme.example"],"channels":["agent"],"authority":"negotiate"}`
	_, prep := as(wsB)(http.MethodPost, "/v1/negotiations/prepare", body)
	var p struct{ Canonical, Challenge string }
	_ = json.Unmarshal([]byte(prep), &p)
	wrong, _ := json.Marshal(map[string]any{"canonical": p.Canonical, "assertion": sign[wsB](economy.MandateChallenge("another"))})
	codeBad, msgBad := as(wsB)(http.MethodPost, "/v1/negotiations", string(wrong))
	if codeHeld != http.StatusBadRequest || codeHeld != codeBad || msgHeld != msgBad {
		t.Errorf("refused in a confirmed deal = %d %q; refused for a bad signature = %d %q: want the same answer", codeHeld, msgHeld, codeBad, msgBad)
	}
	// A seller in an unrelated deal that shares the reference is no party to either held deal, and is kept.
	if code, msg, _ := side(wsB, `"unrelated.example"`, "sell"); code != http.StatusCreated {
		t.Errorf("an unrelated seller sharing the reference = %d %s, want 201", code, msg)
	}

	// Revoked, the buyer gives the deal up: its mandate can no longer be confirmed, and Acme may confirm the rival.
	if code, _ := as(wsA)(http.MethodPost, "/v1/negotiations/"+a.ID+"/revoke", ""); code != http.StatusOK {
		t.Fatalf("revoke = %d", code)
	}
	if code := confirm(acme, a.ID, "rfq-acme-7"); code != http.StatusNotFound {
		t.Errorf("Acme confirming a revoked mandate = %d, want 404", code)
	}
	if code := confirm(acme, b.ID, "rfq-acme-7"); code != http.StatusOK {
		t.Errorf("Acme confirming the rival once the buyer revoked = %d, want 200", code)
	}
}

type b547Keys []struct {
	origin, kid string
	pub         ed25519.PublicKey
}

func (d b547Keys) Key(_ context.Context, origin, kid string) (ed25519.PublicKey, error) {
	for _, k := range d {
		if k.origin == origin && k.kid == kid {
			return k.pub, nil
		}
	}
	return nil, agentsig.ErrUnknownKey
}
