package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/ratelimit"
)

// B54.8 — a counterparty confirms a negotiation's deal by email: the email's DKIM signature must hold for one of the
// mandate's counterparties (or a domain under one), over a subject naming the negotiation and its deal reference. A
// forged From with no signature, or signed by another domain, or altered after signing, confirms nothing; a mandate
// that does not allow email is not found; a DKIM-valid email from the counterparty confirms the deal, recorded as the
// counterparty's registrable domain via email, and it then blocks a second workspace exactly as an agent's
// confirmation does.
func TestB548_ACounterpartyConfirmsADealByDKIMSignedEmail(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const wsA, wsB, rpID, origin = "ws-b548-buyer", "ws-b548-rival", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	for ws, domain := range map[string]string{wsA: "buyer.example", wsB: "rival.example"} {
		if _, err := pool.Exec(ctx, `INSERT INTO negotiation_domains (workspace_id, domain, claimed_by, verified_at) VALUES ($1, $2, 'test', now())`, ws, domain); err != nil {
			t.Fatal(err)
		}
	}

	// The DNS: each signing domain's selector publishes its key.
	keys := map[string]ed25519.PrivateKey{}
	txt := map[string]string{}
	for _, d := range []string{"mail.acme.example", "evil.example"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		keys[d], txt["s1._domainkey."+d] = priv, "v=DKIM1; k=ed25519; p="+base64.StdEncoding.EncodeToString(pub)
	}
	lookup := func(_ context.Context, name string) ([]string, error) {
		if v, ok := txt[name]; ok {
			return []string{v}, nil
		}
		return nil, errors.New("no such record")
	}

	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	mountNegotiationEmailRoutes(r, ratelimit.New(nil, nil), lookup, store)
	as := func(ws, method, path, body string) (int, string) {
		who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), who)))
		return w.Code, w.Body.String()
	}
	sign := map[string]func(string) *passkey.Assertion{wsA: b5018Passkey(t, store, wsA, rpID, origin), wsB: b5018Passkey(t, store, wsB, rpID, origin)}
	mandate := func(ws, channels string) (int, string, economy.NegotiationMandate) {
		t.Helper()
		body := `{"task":{"kind":"purchase","what":"GPU hours"},"side":"buy","deal":"RFQ-ACME-9","currency":"GBP",
			"target_minor":500000,"walk_away_minor":800000,"deadline":"` + time.Now().Add(48*time.Hour).Format(time.RFC3339) + `",
			"counterparties":["acme.example"],"channels":[` + channels + `],"authority":"negotiate"}`
		code, msg := as(ws, http.MethodPost, "/v1/negotiations/prepare", body)
		var p struct{ Canonical, Challenge string }
		if code != http.StatusOK || json.Unmarshal([]byte(msg), &p) != nil {
			t.Fatalf("%s prepare = %d %s", ws, code, msg)
		}
		b, _ := json.Marshal(map[string]any{"canonical": p.Canonical, "assertion": sign[ws](p.Challenge)})
		code, msg = as(ws, http.MethodPost, "/v1/negotiations", string(b))
		var m economy.NegotiationMandate
		_ = json.Unmarshal([]byte(msg), &m)
		return code, msg, m
	}
	// emailFrom is a message from address with subject and the extra headers; signer "" leaves it unsigned, and signed
	// lists the headers it signs.
	emailFrom := func(from, extra, subject, signer string, signed ...string) string {
		t.Helper()
		raw := "From: " + from + "\r\nTo: counter@talyvor.example\r\nSubject: " + subject + "\r\n" + extra +
			"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nYes, that's our RFQ.\r\n"
		if signer == "" {
			return raw
		}
		var out bytes.Buffer
		if err := dkim.Sign(&out, strings.NewReader(raw), &dkim.SignOptions{Domain: signer, Selector: "s1", Signer: keys[signer], HeaderKeys: signed}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	email := func(subject, signer string, signed ...string) string {
		t.Helper()
		return emailFrom("Acme Sales <sales@acme.example>", "", subject, signer, signed...)
	}
	deliver := func(raw string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/inbound/negotiations/deal", strings.NewReader(raw))
		req.Header.Set("Content-Type", "message/rfc822")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	confirmed := func(id string) (by, via string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT deal_confirmed_by, deal_confirmed_via FROM negotiation_mandates WHERE id = $1`, id).Scan(&by, &via); err != nil {
			t.Fatal(err)
		}
		return by, via
	}
	all := []string{"From", "To", "Subject", "Date"}

	codeAgent, msgAgent, agentOnly := mandate(wsA, `"agent"`)
	codeA, msgA, a := mandate(wsA, `"email","agent"`)
	codeB, msgB, b := mandate(wsB, `"email"`)
	if codeAgent != http.StatusCreated || codeA != http.StatusCreated || codeB != http.StatusCreated {
		t.Fatalf("mandates = %d %s / %d %s / %d %s", codeAgent, msgAgent, codeA, msgA, codeB, msgB)
	}
	subject := func(id string) string { return "Confirm RFQ-ACME-9 for negotiation " + id }

	// A forged From confirms nothing: unsigned, signed by a domain that is not the counterparty, altered after Acme
	// signed it, or signed by Acme without the subject that names the negotiation.
	if code := deliver(email(subject(a.ID), "")); code != http.StatusUnauthorized {
		t.Errorf("an unsigned email From Acme = %d, want 401", code)
	}
	if code := deliver(email(subject(a.ID), "evil.example", all...)); code != http.StatusUnauthorized {
		t.Errorf("an email From Acme signed by evil.example = %d, want 401", code)
	}
	if code := deliver(emailFrom("Evil <x@evil.example>", "", subject(a.ID), "evil.example", all...)); code != http.StatusNotFound {
		t.Errorf("evil.example's own signed email to Acme's negotiation = %d, want 404", code)
	}
	tampered := strings.Replace(email(subject(b.ID), "mail.acme.example", all...), b.ID, a.ID, 1)
	if code := deliver(tampered); code != http.StatusUnauthorized {
		t.Errorf("Acme's email with its negotiation changed after signing = %d, want 401", code)
	}
	if code := deliver(email(subject(a.ID), "mail.acme.example", "From", "To", "Date")); code != http.StatusUnauthorized {
		t.Errorf("Acme's email whose subject it did not sign = %d, want 401", code)
	}
	// A subject added above Acme's signed one is not what Acme signed; a reply or an automatic answer that echoes a
	// subject someone else wrote is not Acme confirming; nor is Acme's signature on mail From another domain.
	if code := deliver("Subject: " + subject(a.ID) + "\r\n" + email("Our order", "mail.acme.example", all...)); code != http.StatusUnauthorized {
		t.Errorf("a subject added above Acme's signed one = %d, want 401", code)
	}
	if code := deliver(email("Automatic reply: "+subject(a.ID), "mail.acme.example", all...)); code != http.StatusBadRequest {
		t.Errorf("Acme's automatic reply echoing the subject = %d, want 400", code)
	}
	if code := deliver(emailFrom("Acme Sales <sales@acme.example>", "Auto-Submitted: auto-replied\r\n", subject(a.ID), "mail.acme.example", all...)); code != http.StatusBadRequest {
		t.Errorf("Acme's auto-submitted email = %d, want 400", code)
	}
	if code := deliver(emailFrom("Someone <x@evil.example>", "", subject(a.ID), "mail.acme.example", all...)); code != http.StatusUnauthorized {
		t.Errorf("Acme's signature on mail From evil.example = %d, want 401", code)
	}
	// Acme's signed email to a mandate that does not allow email, or naming another reference, confirms nothing.
	if code := deliver(email(subject(agentOnly.ID), "mail.acme.example", all...)); code != http.StatusNotFound {
		t.Errorf("Acme's email to an agent-only mandate = %d, want 404", code)
	}
	if code := deliver(email("confirm RFQ-ACME-10 for negotiation "+a.ID, "mail.acme.example", all...)); code != http.StatusBadRequest {
		t.Errorf("Acme's email naming another reference = %d, want 400", code)
	}
	for _, id := range []string{a.ID, b.ID, agentOnly.ID} {
		if by, via := confirmed(id); by != "" || via != "" {
			t.Fatalf("a refused email confirmed %s: %q via %q", id, by, via)
		}
	}

	// Acme's DKIM-valid email confirms the buyer: the deal is theirs, from acme.example, by email.
	if code := deliver(email(subject(a.ID), "mail.acme.example", all...)); code != http.StatusOK {
		t.Fatalf("Acme's signed email confirming the buyer = %d, want 200", code)
	}
	if by, via := confirmed(a.ID); by != "acme.example" || via != economy.ChannelEmail {
		t.Fatalf("the buyer's mandate after Acme's email = %q via %q, want acme.example via email", by, via)
	}

	// Then it blocks the rival as an agent's confirmation does: Acme cannot confirm them too, their Counter makes no
	// offer in it, and a new mandate in it is refused naming nobody.
	if code := deliver(email(subject(b.ID), "mail.acme.example", all...)); code != http.StatusConflict {
		t.Errorf("Acme's email confirming a second party in its deal = %d, want 409", code)
	}
	if by, _ := confirmed(b.ID); by != "" {
		t.Errorf("the rival's mandate was confirmed by %q", by)
	}
	offer := `{"counterparty":"acme.example","party":"ours","channel":"email","terms":{"price_minor":600000}}`
	if code, msg := as(wsB, http.MethodPost, "/v1/negotiations/"+b.ID+"/terms", offer); code != http.StatusBadRequest || strings.Contains(msg, wsA) {
		t.Errorf("the rival's offer in a deal confirmed to another = %d %s, want 400 naming nobody", code, msg)
	}
	if code, msg, _ := mandate(wsB, `"email"`); code != http.StatusBadRequest || !strings.Contains(msg, "Counter can't take this negotiation") {
		t.Errorf("a new rival mandate in the held deal = %d %q, want 400 naming nobody", code, msg)
	}
}
