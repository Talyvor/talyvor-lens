package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B52.12 — through the routes: Counter's first message to a counterparty says it is an AI agent acting for the named
// owner and later ones do not repeat it; a site on the refused list is never contacted, even when the mandate names
// it, nor an agent without a live credential, nor anyone by email; a verified agent's reply threads into the deal of
// the message it answers, and no other workspace can answer it.
func TestB5212_CounterReachesTheOtherSideHonestly(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const buyer, seller, other, rpID, origin = "ws-b5212-buyer", "ws-b5212-seller", "ws-b5212-other", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 1, 'contact', 'test', 'completed', 'ref-1', ''), ($1, 2, 'person', 'kyc-partner', 'completed', 'ref-2', 'Ada Lovelace')`, buyer); err != nil {
		t.Fatal(err)
	}
	verified, err := store.CreateAgent(ctx, seller, "Acme sales", "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, _, err := store.IssueKYACredential(ctx, seller, verified.ID, func(economy.AgentKYAFacts) (economy.KYACredentialRecord, error) {
		return economy.KYACredentialRecord{ID: uuid.NewString(), Kid: "k1", PublicKey: "pk", ClaimsDigest: "d", Token: "t",
			IssuedAt: now, ExpiresAt: now.Add(time.Hour)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	unverified, err := store.CreateAgent(ctx, other, "No credential", "owner")
	if err != nil {
		t.Fatal(err)
	}
	// An agent is reached only as a counterparty its workspace proved it trades as (B54.23).
	if _, err := pool.Exec(ctx, `INSERT INTO negotiation_domains (workspace_id, domain, claimed_by, verified_at) VALUES
		($1, 'acme.example', 'test', now()), ($1, 'shop.example', 'test', now()), ($2, 'shop.example', 'test', now())`, seller, other); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	mountNegotiationOutreachRoutes(r, store, nil, "")
	as := func(ws string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
	}
	call := func(who *auth.AuthContext, method, path, body string, out any) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s: %v in %s", path, err, w.Body.String())
			}
		}
		return w.Code, w.Body.String()
	}
	sign := b5018Passkey(t, store, buyer, rpID, origin)
	mandate := func(what string) economy.NegotiationMandate {
		t.Helper()
		var prep struct{ Canonical, Challenge string }
		if code, msg := call(as(buyer), http.MethodPost, "/v1/negotiations/prepare", `{"task":{"kind":"renewal","what":"`+what+`"},
			"side":"buy","currency":"GBP","target_minor":1000000,"walk_away_minor":1200000,"deadline":"`+
			now.Add(48*time.Hour).Format(time.RFC3339)+`","counterparties":["acme.example","shop.example"],
			"channels":["agent","web_form","email"],"web_forms":["shop.example","ebay.co.uk"],"authority":"negotiate"}`, &prep); code != http.StatusOK {
			t.Fatalf("prepare = %d %s", code, msg)
		}
		b, _ := json.Marshal(map[string]any{"canonical": prep.Canonical, "assertion": sign(prep.Challenge)})
		var m economy.NegotiationMandate
		if code, msg := call(as(buyer), http.MethodPost, "/v1/negotiations", string(b), &m); code != http.StatusCreated {
			t.Fatalf("keep = %d %s", code, msg)
		}
		return m
	}
	a, b := mandate("Acme CRM, 40 seats"), mandate("Acme support plan")
	send := func(m economy.NegotiationMandate, cp, channel, to string) (int, string, economy.NegotiationOutreach) {
		t.Helper()
		var o economy.NegotiationOutreach
		code, msg := call(as(buyer), http.MethodPost, "/v1/negotiations/"+m.ID+"/outreach",
			fmt.Sprintf(`{"counterparty":%q,"channel":%q,"to":%q}`, cp, channel, to), &o)
		return code, msg, o
	}
	const disclosure = "This message is from Talyvor Counter, an AI agent negotiating on behalf of Ada Lovelace."

	// The first message to a counterparty carries the disclosure; the second does not repeat it; both say the owner
	// reads every reply.
	code, msg, first := send(a, "acme.example", "agent", verified.ID)
	if code != http.StatusOK || first.Sent == nil || !strings.HasPrefix(first.Sent.Body, disclosure) ||
		!strings.Contains(first.Sent.Body, "Ada Lovelace reads every reply") || first.Delivery != "delivered" {
		t.Fatalf("first message = %d %s", code, msg)
	}
	if code, msg, again := send(a, "acme.example", "agent", verified.ID); code != http.StatusOK || again.Sent == nil ||
		strings.Contains(again.Sent.Body, "AI agent") || !strings.Contains(again.Sent.Body, "Ada Lovelace reads every reply") {
		t.Fatalf("second message = %d %s", code, msg)
	}
	if code, msg, form := send(a, "shop.example", "web_form", "https://shop.example/quote"); code != http.StatusOK ||
		form.Sent == nil || !strings.HasPrefix(form.Sent.Body, disclosure) || form.Delivery != "submit_form" {
		t.Fatalf("web form = %d %s", code, msg)
	}

	// The same counterparty reached at a new address is told again.
	if code, msg, moved := send(a, "shop.example", "agent", verified.ID); code != http.StatusOK || moved.Sent == nil ||
		!strings.HasPrefix(moved.Sent.Body, disclosure) {
		t.Fatalf("a new address = %d %s", code, msg)
	}

	// A refused site, though the mandate names it; an agent without a credential; and email: none is contacted.
	for _, c := range []struct{ channel, to string }{{"web_form", "https://www.ebay.co.uk/contact"},
		{"agent", unverified.ID}, {"email", "sales@shop.example"}} {
		if code, msg, _ := send(a, "shop.example", c.channel, c.to); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s %s = %d %s", c.channel, c.to, code, msg)
		}
	}
	var refused int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM negotiation_messages WHERE address LIKE '%ebay%' OR address = $1
		OR channel = 'email'`, unverified.ID).Scan(&refused); err != nil || refused != 0 {
		t.Fatalf("a refused counterparty was contacted: %d %v", refused, err)
	}

	// The seller's workspace reads what was sent to its agent, and its reply threads into the deal it answers.
	_, _, toB := send(b, "acme.example", "agent", verified.ID)
	var inbox struct {
		Messages []economy.NegotiationInboxMessage
	}
	if code, msg := call(as(seller), http.MethodGet, "/v1/negotiations/inbox", "", &inbox); code != http.StatusOK ||
		len(inbox.Messages) != 4 || inbox.Messages[0].ID != toB.Sent.ID || strings.Contains(msg, a.ID) || strings.Contains(msg, b.ID) {
		t.Fatalf("seller's inbox = %d %s", code, msg)
	}
	replyPath := fmt.Sprintf("/v1/negotiations/inbox/%d/reply", toB.Sent.ID)
	if code, msg := call(as(other), http.MethodPost, replyPath, `{"message":"I am not the seller"}`, nil); code != http.StatusNotFound {
		t.Fatalf("another workspace answered = %d %s", code, msg)
	}
	if code, msg := call(as(seller), http.MethodPost, replyPath, `{"message":"Our support plan renews at 11,500.00 GBP."}`, nil); code != http.StatusCreated {
		t.Fatalf("reply = %d %s", code, msg)
	}
	timeline := func(m economy.NegotiationMandate) []economy.NegotiationMessage {
		t.Helper()
		var got economy.NegotiationMandate
		if code, msg := call(as(buyer), http.MethodGet, "/v1/negotiations/"+m.ID, "", &got); code != http.StatusOK {
			t.Fatalf("read = %d %s", code, msg)
		}
		return got.Messages
	}
	if tb := timeline(b); len(tb) != 2 || tb[1].Direction != "in" || tb[1].Counterparty != "acme.example" ||
		tb[1].Body != "Our support plan renews at 11,500.00 GBP." {
		t.Fatalf("deal B's timeline = %+v", tb)
	}
	for _, msg := range timeline(a) {
		if msg.Direction != "out" {
			t.Fatalf("the reply threaded into the wrong deal: %+v", msg)
		}
	}
}
