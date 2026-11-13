package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

// B54.45 — a tester confirms a deal by email without real DNS: it publishes the counterparty's DKIM key for a domain
// under .test through the synthetic key, and the inbound email route reads that key. Before it is published the same
// signed email confirms nothing; once it is, the deal is confirmed from the counterparty's domain by email. A domain
// outside .test is refused 400, and without LENS_SYNTHETIC_KEY the route does not exist.
func TestB5445_AnEmailSignedWithAPublishedTestKeyConfirmsTheDeal(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin, key = "ws-b5445-buyer", "app.talyvor.com", "https://app.talyvor.com", "b5445-synthetic-key"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO negotiation_domains (workspace_id, domain, claimed_by, verified_at) VALUES ($1, 'buyer.test', 'test', now())`, ws); err != nil {
		t.Fatal(err)
	}

	keys := newSyntheticDKIMKeys()
	r := chi.NewRouter()
	mountSyntheticRoutes(r, key, syntheticDeps{dkim: keys})
	mountNegotiationRoutes(r, store)
	mountNegotiationEmailRoutes(r, ratelimit.New(nil, nil), keys.resolver(func(_ context.Context, name string) ([]string, error) {
		t.Errorf("asked DNS for %s", name)
		return nil, nil
	}), store)
	call := func(method, path, body string, hdr map[string]string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), who)))
		return w.Code, w.Body.String()
	}
	publish := func(domain string, pub ed25519.PublicKey) (int, string) {
		b, _ := json.Marshal(map[string]string{"domain": domain, "selector": "s1", "key_type": "ed25519", "public_key": base64.StdEncoding.EncodeToString(pub)})
		return call(http.MethodPost, "/v1/synthetic/dkim-keys", string(b), map[string]string{syntheticKeyHeader: key})
	}

	// The buyer's signed mandate, with Acme as its counterparty and email allowed.
	sign := b5018Passkey(t, store, ws, rpID, origin)
	body := `{"task":{"kind":"purchase","what":"GPU hours"},"side":"buy","deal":"RFQ-ACME-45","currency":"GBP",
		"target_minor":500000,"walk_away_minor":800000,"deadline":"` + time.Now().Add(48*time.Hour).Format(time.RFC3339) + `",
		"counterparties":["acme.test"],"channels":["email"],"authority":"negotiate"}`
	code, msg := call(http.MethodPost, "/v1/negotiations/prepare", body, nil)
	var p struct{ Canonical, Challenge string }
	if code != http.StatusOK || json.Unmarshal([]byte(msg), &p) != nil {
		t.Fatalf("prepare = %d %s", code, msg)
	}
	b, _ := json.Marshal(map[string]any{"canonical": p.Canonical, "assertion": sign(p.Challenge)})
	code, msg = call(http.MethodPost, "/v1/negotiations", string(b), nil)
	var m economy.NegotiationMandate
	if code != http.StatusCreated || json.Unmarshal([]byte(msg), &m) != nil {
		t.Fatalf("mandate = %d %s", code, msg)
	}

	// Acme's email, signed by mail.acme.test.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw := "From: Acme Sales <sales@acme.test>\r\nTo: counter@talyvor.example\r\nSubject: Confirm RFQ-ACME-45 for negotiation " + m.ID +
		"\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nYes, that's our RFQ.\r\n"
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, strings.NewReader(raw), &dkim.SignOptions{Domain: "mail.acme.test", Selector: "s1", Signer: priv,
		HeaderKeys: []string{"From", "To", "Subject", "Date"}}); err != nil {
		t.Fatal(err)
	}
	deliver := func() (int, string) {
		return call(http.MethodPost, "/v1/inbound/negotiations/deal", signed.String(), map[string]string{"Content-Type": "message/rfc822"})
	}
	confirmed := func() (by, via string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT deal_confirmed_by, deal_confirmed_via FROM negotiation_mandates WHERE id = $1`, m.ID).Scan(&by, &via); err != nil {
			t.Fatal(err)
		}
		return by, via
	}

	// Unpublished, the key is not found and the email confirms nothing.
	if code, msg := deliver(); code != http.StatusUnauthorized {
		t.Fatalf("Acme's email before its key is published = %d %s, want 401", code, msg)
	}
	// The same key for a domain outside .test is refused, and nothing is published for it.
	for _, d := range []string{"mail.acme.example", "acme.com", "test", "mail.acme.test.example"} {
		if code, msg := publish(d, pub); code != http.StatusBadRequest {
			t.Errorf("the key published for %s = %d %s, want 400", d, code, msg)
		}
	}
	if by, via := confirmed(); by != "" || via != "" {
		t.Fatalf("the mandate was confirmed before the key was published: %q via %q", by, via)
	}

	// Published under .test, the same email confirms the deal: from acme.test, by email.
	if code, msg := publish("mail.acme.test", pub); code != http.StatusCreated {
		t.Fatalf("the key published for mail.acme.test = %d %s, want 201", code, msg)
	}
	if code, msg := deliver(); code != http.StatusOK {
		t.Fatalf("Acme's email after its key is published = %d %s, want 200", code, msg)
	}
	if by, via := confirmed(); by != "acme.test" || via != economy.ChannelEmail {
		t.Fatalf("the mandate after Acme's email = %q via %q, want acme.test via email", by, via)
	}

	// Without the synthetic key the route does not exist.
	bare := chi.NewRouter()
	mountSyntheticRoutes(bare, "", syntheticDeps{dkim: keys})
	req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/dkim-keys", strings.NewReader(`{}`))
	req.Header.Set(syntheticKeyHeader, key)
	w := httptest.NewRecorder()
	bare.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("the route without LENS_SYNTHETIC_KEY = %d, want 404", w.Code)
	}
}

// After an hour a published key is gone, and a name outside .test is still asked of DNS.
func TestB5445_APublishedKeyExpiresInAnHour(t *testing.T) {
	keys := newSyntheticDKIMKeys()
	now := time.Now()
	keys.now = func() time.Time { return now }
	r := chi.NewRouter()
	mountSyntheticRoutes(r, "k", syntheticDeps{dkim: keys})
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	b, _ := json.Marshal(map[string]string{"domain": "acme.test", "selector": "s1", "key_type": "ed25519", "public_key": base64.StdEncoding.EncodeToString(pub)})
	req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/dkim-keys", bytes.NewReader(b))
	req.Header.Set(syntheticKeyHeader, "k")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", w.Code, w.Body.String())
	}
	var asked []string
	lookup := keys.resolver(func(_ context.Context, name string) ([]string, error) { asked = append(asked, name); return nil, nil })
	if txt, err := lookup(context.Background(), "s1._domainkey.acme.test"); err != nil || len(txt) != 1 || !strings.HasPrefix(txt[0], "v=DKIM1; k=ed25519; p=") {
		t.Fatalf("the published key = %q %v", txt, err)
	}
	now = now.Add(syntheticDKIMTTL)
	if txt, err := lookup(context.Background(), "s1._domainkey.acme.test"); err == nil {
		t.Errorf("the key an hour later = %q, want not found", txt)
	}
	_, _ = lookup(context.Background(), "s1._domainkey.acme.example")
	if len(asked) != 1 || asked[0] != "s1._domainkey.acme.example" {
		t.Errorf("asked DNS for %q, want only s1._domainkey.acme.example", asked)
	}
}
