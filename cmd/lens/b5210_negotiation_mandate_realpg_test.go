package main

import (
	"context"
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

// B52.10 — Counter's negotiation mandate: kept only with the owner's signature; altered after it was kept, it allows
// nothing; the walk-away never appears in an outbound offer, however it is written; a second Talyvor owner in the
// same RFQ, on either side of a deal the counterparty confirmed, is refused without being told who holds it, and cannot
// read the first's mandate.
func TestB5210_TheNegotiationMandateTheOwnerSigns(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const wsA, wsB, rpID, origin = "ws-b5210-buyer", "ws-b5210-rival", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	as := func(ws string) func(method, path, body string, out any) (int, string) {
		who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
		return func(method, path, body string, out any) (int, string) {
			t.Helper()
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if out != nil && w.Code < 300 {
				if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
					t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
				}
			}
			return w.Code, w.Body.String()
		}
	}
	callA, callB := as(wsA), as(wsB)
	signA, signB := b5018Passkey(t, store, wsA, rpID, origin), b5018Passkey(t, store, wsB, rpID, origin)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	terms := func(side, deal string) string {
		return `{"task":{"kind":"renewal","what":"Acme CRM, 40 seats"},"side":"` + side + `","deal":"` + deal + `","currency":"GBP",
			"target_minor":1000000,"walk_away_minor":1200000,"must_haves":["SSO"],"trade_offs":["24-month term"],
			"deadline":"` + time.Now().Add(48*time.Hour).Format(time.RFC3339) + `","counterparties":["Acme.example"],
			"channels":["email"],"authority":"negotiate"}`
	}
	prepare := func(call func(string, string, string, any) (int, string), body string) (string, string) {
		t.Helper()
		var out struct{ Canonical, Challenge string }
		if code, msg := call(http.MethodPost, "/v1/negotiations/prepare", body, &out); code != http.StatusOK {
			t.Fatalf("prepare = %d %s", code, msg)
		}
		return out.Canonical, out.Challenge
	}
	keep := func(call func(string, string, string, any) (int, string), canonical string, a *passkey.Assertion, out any) (int, string) {
		b, _ := json.Marshal(map[string]any{"canonical": canonical, "assertion": a})
		return call(http.MethodPost, "/v1/negotiations", string(b), out)
	}

	// Kept only with the owner's signature: unsigned, signed by another workspace's passkey, or altered before
	// signing, nothing is kept.
	canonical, challenge := prepare(callA, terms("buy", "RFQ-ACME-2026-114"))
	if strings.Contains(canonical, "RFQ") || !strings.Contains(canonical, `"deal":"rfq-acme-2026-114"`) {
		t.Fatalf("the deal reference is not normalised: %s", canonical)
	}
	altered := strings.Replace(canonical, `"walk_away_minor":1200000`, `"walk_away_minor":1900000`, 1)
	for name, c := range map[string]struct {
		canonical string
		a         *passkey.Assertion
	}{
		"unsigned":                      {canonical, nil},
		"another workspace's passkey":   {canonical, signB(challenge)},
		"altered, signed as it was":     {altered, signA(challenge)},
		"altered, signed as it now is":  {altered, signA(economy.MandateChallenge(canonical))},
		"signed over another challenge": {canonical, signA(economy.MandateChallenge(altered))},
	} {
		if code, msg := keep(callA, c.canonical, c.a, nil); code != http.StatusBadRequest {
			t.Errorf("%s: keep = %d %s, want 400", name, code, msg)
		}
	}
	if n := count(`SELECT count(*) FROM negotiation_mandates`); n != 0 {
		t.Fatalf("%d mandates kept without the owner's signature", n)
	}
	var m economy.NegotiationMandate
	if code, msg := keep(callA, canonical, signA(challenge), &m); code != http.StatusCreated || m.Status != "active" || m.WalkAwayMinor != 1200000 {
		t.Fatalf("keep the signed mandate = %d %s %+v", code, msg, m)
	}
	base := "/v1/negotiations/" + m.ID

	// The walk-away never appears in an outbound offer: not as £12,000, 12k, 12.000,00, 12 000, the minor units or in
	// the terms' text; an offer at it is refused too. An offer short of it, and theirs above it, are kept.
	offer := func(party string, price int64, message, sla string) (int, string) {
		b, _ := json.Marshal(map[string]any{"counterparty": "acme.example", "party": party, "channel": "email",
			"terms": map[string]any{"price_minor": price, "term_months": 12, "sla": sla}, "message": message})
		return callA(http.MethodPost, base+"/terms", string(b), nil)
	}
	if code, msg := offer("ours", 1100000, "We can renew 40 seats at £11,000 for 12 months.", "99.9%"); code != http.StatusCreated {
		t.Fatalf("an offer inside the mandate = %d %s", code, msg)
	}
	for _, leak := range []struct{ message, sla string }{
		{"We can go as far as £12,000.", ""}, {"Our ceiling is 12k.", ""}, {"Höchstens 12.000,00 €.", ""},
		{"Up to 12 000 GBP.", ""}, {"Budget: 1200000 pence.", ""}, {"£12,000.00 is the most.", ""},
		{"About 12 thousand.", ""}, {"Fine.", "credits up to £12,000"},
	} {
		if code, _ := offer("ours", 1100000, leak.message, leak.sla); code != http.StatusBadRequest {
			t.Errorf("outbound %q / sla %q = %d, want 400: it names the walk-away", leak.message, leak.sla, code)
		}
	}
	if code, _ := offer("ours", 1200000, "Our final offer.", ""); code != http.StatusBadRequest {
		t.Errorf("an offer at the walk-away = %d, want 400", code)
	}
	if code, msg := offer("theirs", 1300000, "Our best price is £13,000.", ""); code != http.StatusCreated {
		t.Fatalf("their counter-offer = %d %s", code, msg)
	}
	rows, err := pool.Query(ctx, `SELECT message || ' ' || terms::text FROM negotiation_terms WHERE party = 'ours'`)
	if err != nil {
		t.Fatal(err)
	}
	var sent int
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		sent++
		if economy.MentionsAmount(text, 1200000, 2) {
			t.Errorf("an outbound offer names the walk-away: %s", text)
		}
	}
	rows.Close()
	if sent != 1 {
		t.Fatalf("%d outbound offers kept, want 1", sent)
	}
	var got economy.NegotiationMandate
	if code, _ := callA(http.MethodGet, base, "", &got); code != http.StatusOK || len(got.TermSheets["acme.example"]) != 2 ||
		got.TermSheets["acme.example"][1].Terms.PriceMinor != 1300000 {
		t.Fatalf("term sheet = %d %+v", code, got.TermSheets)
	}

	// A second Talyvor owner in the same RFQ, on either side — another of Acme's bidders, or Acme's own workspace
	// selling to the first — is refused without being told who holds it, once Acme has confirmed the deal (B54.7: its
	// agent's signature and its proved domain; here, their rows), and cannot read the first's mandate.
	if _, err := pool.Exec(ctx, `UPDATE negotiation_mandates SET deal_confirmed_by = 'acme.example', deal_confirmed_via = 'email',
		deal_confirmed_at = now() WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO negotiation_domains (workspace_id, domain, claimed_by, verified_at) VALUES ($1, 'acme.example', 'test', now())`, wsB); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"buy", "sell"} {
		rival := strings.Replace(terms(side, "rfq-acme-2026-114"), `"target_minor":1000000,"walk_away_minor":1200000`,
			map[string]string{"buy": `"target_minor":1000000,"walk_away_minor":1200000`, "sell": `"target_minor":1500000,"walk_away_minor":1100000`}[side], 1)
		c, ch := prepare(callB, rival)
		code, msg := keep(callB, c, signB(ch), nil)
		if code != http.StatusBadRequest || strings.Contains(msg, wsA) || strings.Contains(msg, "1200000") {
			t.Errorf("a second owner on the %s side of the same RFQ = %d %s, want 400 naming nobody", side, code, msg)
		}
	}
	if n := count(`SELECT count(*) FROM negotiation_mandates WHERE workspace_id = $1`, wsB); n != 0 {
		t.Fatalf("the rival owner holds %d mandates in the deal", n)
	}
	if code, _ := callB(http.MethodGet, base, "", nil); code != http.StatusNotFound {
		t.Errorf("the rival reads the first owner's mandate = %d, want 404", code)
	}

	// Altered after it was kept, it allows nothing: a walk-away raised in the database admits no offer.
	if _, err := pool.Exec(ctx, `UPDATE negotiation_mandates SET canonical = replace(canonical, '"walk_away_minor":1200000', '"walk_away_minor":1900000')
		WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if code, msg := offer("ours", 1500000, "We can renew at £15,000.", ""); code != http.StatusBadRequest || !strings.Contains(msg, "allows nothing") {
		t.Errorf("an offer under the altered mandate = %d %s, want 400: it allows nothing", code, msg)
	}
	if n := count(`SELECT count(*) FROM negotiation_terms WHERE party = 'ours'`); n != 1 {
		t.Fatalf("%d outbound offers after the mandate was altered, want still 1", n)
	}

	// Revoked, the deal is free for the next owner.
	if code, _ := callA(http.MethodPost, base+"/revoke", "", nil); code != http.StatusOK {
		t.Fatalf("revoke = %d", code)
	}
	c, ch := prepare(callB, terms("buy", "rfq-acme-2026-114"))
	if code, msg := keep(callB, c, signB(ch), nil); code != http.StatusCreated {
		t.Fatalf("the next owner in the freed deal = %d %s", code, msg)
	}
}
