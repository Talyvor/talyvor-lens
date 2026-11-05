package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
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
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B45.40 — an outside agent's payment request is judged by the mandate's "who": under a mandate "only verified agents",
// a verified agent's item names the mandate with no counterparty rule, and a signed-but-unchecked one's carries
// counterparty.min_trust. Neither is paid: an outside agent's request always waits for the owner.
func TestB4540_OutsideAgentRequestJudgedByTheMandatesWho(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4540", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES ('agt_b4540', $1, 'Buyer bot')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_acme', $1, 'Acme Inc.', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}

	// The owner signs "only verified agents" with a passkey: this test is the authenticator.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	attested := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	attested = append(append(binary.BigEndian.AppendUint16(append(attested, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
	if _, err := store.RegisterPasskey(ctx, ws, "Ada's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}, nil); err != nil {
		t.Fatal(err)
	}
	canonical, challenge, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"},
		Clients:   []string{"a2a:agent.acme.example", "a2a:bot.example"},
		Scope:     economy.MandateScope{Payees: []string{"payee_acme"}, Currencies: []string{"GBP"}},
		// above £50 the owner approves, so the verified agent's item names this mandate rather than being allowed
		RequiresApprovalAbove: map[string]int64{"GBP": 5000}, NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee},
		ExpiresAt: time.Now().Add(24 * time.Hour), Counterparty: &economy.MandateCounterparty{MinTrust: "verified"}})
	if err != nil {
		t.Fatal(err)
	}
	cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), 1)
	h := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	mandate, err := store.CreatePermissionMandate(ctx, ws, "ada", canonical, &passkey.Assertion{CredentialID: b64(credID),
		ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)})
	if err != nil {
		t.Fatal(err)
	}

	// Two outside agents: Acme's, whose business Talyvor checked, and bot.example's, signed but not checked.
	sigs := agentsig.Store{Pool: pool}
	if _, err := sigs.SetChecked(ctx, "agent.acme.example", "Acme Inc.", "ops@talyvor.com"); err != nil {
		t.Fatal(err)
	}
	acmePub, acmePriv, _ := ed25519.GenerateKey(rand.Reader)
	botPub, botPriv, _ := ed25519.GenerateKey(rand.Reader)
	dir := twoKeyDirectory{
		{"https://agent.acme.example", kya.JWKOf(acmePub).Kid, acmePub},
		{"https://bot.example", kya.JWKOf(botPub).Kid, botPub},
	}
	lensURL, _ := url.Parse("https://lens.example")
	r := chi.NewRouter()
	mountOutsideAgentPaymentRoutes(r, &agentsig.Verifier{PublicURL: lensURL, Keys: dir, Nonces: sigs, Businesses: sigs, Now: time.Now}, store)

	const path = "/v1/a2a/agents/agt_b4540/payment-requests"
	ask := func(name, originURL, kid string, priv ed25519.PrivateKey) economy.PaymentRequestItem {
		t.Helper()
		body := []byte(`{"items":[{"payee_id":"payee_acme","currency":"GBP","amount_minor":9000,"reference":"` + name + `"}]}`)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		req.Header.Set("Idempotency-Key", "inv-"+name)
		sum := sha256.Sum256(body)
		req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
		req.Header.Set("Signature-Agent", `"`+originURL+`"`)
		now := time.Now()
		params := fmt.Sprintf(`("@method" "@target-uri" "content-digest" "signature-agent");created=%d;expires=%d;keyid=%q;nonce=%q;tag="web-bot-auth"`,
			now.Unix(), now.Add(2*time.Minute).Unix(), kid, "n-"+name)
		base := "\"@method\": POST\n\"@target-uri\": https://lens.example" + path + "\n" +
			"\"content-digest\": " + req.Header.Get("Content-Digest") + "\n\"signature-agent\": \"" + originURL + "\"\n" +
			"\"@signature-params\": " + params
		req.Header.Set("Signature-Input", "sig1="+params)
		req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(base)))+":")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out struct{ ID string }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("POST %s (%s) = %d %s, want 201", path, name, w.Code, w.Body)
		}
		pr, err := store.PaymentRequest(ctx, ws, "", out.ID)
		if err != nil || len(pr.Items) != 1 {
			t.Fatalf("request %s = %+v %v, want one item", out.ID, pr, err)
		}
		return pr.Items[0]
	}

	verified := ask("verified", "https://agent.acme.example", dir[0].kid, acmePriv)
	if verified.Decision != economy.ItemNeedsApproval || verified.MandateID != mandate.ID || verified.Rule != "requires_approval_above" {
		t.Errorf("a verified agent's item = %s %q by %q, want needs_approval naming %s by requires_approval_above, not its who",
			verified.Decision, verified.MandateID, verified.Rule, mandate.ID)
	}
	unchecked := ask("signed", "https://bot.example", dir[1].kid, botPriv)
	if unchecked.Decision != economy.ItemNeedsApproval || unchecked.MandateID != mandate.ID || unchecked.Rule != "counterparty.min_trust" {
		t.Errorf("a signed-but-unchecked agent's item = %s %q by %q, want needs_approval naming %s by counterparty.min_trust",
			unchecked.Decision, unchecked.MandateID, unchecked.Rule, mandate.ID)
	}
	var payments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1`, ws).Scan(&payments); err != nil || payments != 0 {
		t.Fatalf("payments made = %d %v, want 0: an outside agent's request waits for the owner", payments, err)
	}
}
