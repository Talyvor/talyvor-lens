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

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B45.29 — an outside agent asks a Talyvor agent's owner to pay it: the request lands in the owner's approvals carrying
// who Talyvor takes the asker to be (agent_trust), and is never paid without the owner. Verified, signed but not
// checked, and unsigned each land with their level; a signature that does not hold is refused and stores nothing.
func TestOutsideAgentPaymentRequests_LandInApprovalsWithTrustOnRealPostgres(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4529"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES ('agt_b4529', $1, 'Buyer bot')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_acme', $1, 'Acme Inc.', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
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
	v := &agentsig.Verifier{PublicURL: lensURL, Keys: dir, Nonces: sigs, Businesses: sigs, Now: time.Now}
	r := chi.NewRouter()
	mountOutsideAgentPaymentRoutes(r, v, store)
	mountPaymentRequestRoutes(r, store)

	const path = "/v1/a2a/agents/agt_b4529/payment-requests"
	ask := func(key, reference string, sign func(*http.Request, []byte), wantCode int) map[string]any {
		t.Helper()
		body := []byte(`{"items":[{"payee_id":"payee_acme","currency":"GBP","amount_minor":12500,"reference":"` + reference + `"}]}`)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		req.Header.Set("Idempotency-Key", key)
		if sign != nil {
			sign(req, body)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != wantCode {
			t.Fatalf("POST %s (%s) = %d %s, want %d", path, reference, w.Code, w.Body, wantCode)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	signWith := func(origin, kid string, priv ed25519.PrivateKey, nonce string) func(*http.Request, []byte) {
		return func(req *http.Request, body []byte) {
			sum := sha256.Sum256(body)
			req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
			req.Header.Set("Signature-Agent", `"`+origin+`"`)
			now := time.Now()
			params := fmt.Sprintf(`("@method" "@target-uri" "content-digest" "signature-agent");created=%d;expires=%d;keyid=%q;nonce=%q;tag="web-bot-auth"`,
				now.Unix(), now.Add(2*time.Minute).Unix(), kid, nonce)
			base := "\"@method\": POST\n\"@target-uri\": https://lens.example" + path + "\n" +
				"\"content-digest\": " + req.Header.Get("Content-Digest") + "\n\"signature-agent\": \"" + origin + "\"\n" +
				"\"@signature-params\": " + params
			req.Header.Set("Signature-Input", "sig1="+params)
			req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(base)))+":")
		}
	}

	verified := ask("inv-1", "verified", signWith("https://agent.acme.example", dir[0].kid, acmePriv, "n-1"), http.StatusCreated)
	signed := ask("inv-1", "signed", signWith("https://bot.example", dir[1].kid, botPriv, "n-2"), http.StatusCreated)
	unsigned := ask("inv-1", "unsigned", nil, http.StatusCreated)
	// A signature that does not hold — Acme's key claimed for bot.example — is refused, and nothing is stored.
	ask("inv-2", "forged", signWith("https://bot.example", dir[0].kid, acmePriv, "n-3"), http.StatusUnauthorized)

	want := map[string]struct{ level, label string }{
		verified["id"].(string): {"verified", "Verified agent of Acme Inc."},
		signed["id"].(string):   {"signed", "Signed by bot.example (business not checked)"},
		unsigned["id"].(string): {"unverified", "Unverified agent"},
	}
	if len(want) != 3 {
		t.Fatalf("three outside agents using the same Idempotency-Key got %d requests, want 3 of their own", len(want))
	}

	// The owner's approvals: all three, each with its trust level and nothing paid or allowed automatically.
	req := httptest.NewRequest(http.MethodGet, "/v1/money/payment-requests?status=pending_approval", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
		UserID: "ada", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var list struct {
		Requests []struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			AgentTrust *struct {
				Level, Label, Domain, Business string
			} `json:"agent_trust"`
			Items []struct{ Decision, PayeeName string } `json:"items"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK || len(list.Requests) != 3 {
		t.Fatalf("the owner's approvals = %d %s, want the 3 outside requests", w.Code, w.Body)
	}
	for _, got := range list.Requests {
		exp, ok := want[got.ID]
		switch {
		case !ok:
			t.Errorf("request %s in the approvals is not one the outside agents made", got.ID)
		case got.AgentTrust == nil || got.AgentTrust.Level != exp.level || got.AgentTrust.Label != exp.label:
			t.Errorf("request %s agent_trust = %+v, want %s %q", got.ID, got.AgentTrust, exp.level, exp.label)
		case got.Status != "pending_approval" || len(got.Items) != 1 || got.Items[0].Decision != economy.ItemNeedsApproval:
			t.Errorf("request %s = %s %+v, want pending_approval with its item needing the owner", got.ID, got.Status, got.Items)
		}
	}
	var payments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1`, ws).Scan(&payments); err != nil || payments != 0 {
		t.Fatalf("payments made = %d %v, want 0: an outside agent's request waits for the owner", payments, err)
	}
}

type twoKeyDirectory [2]struct {
	origin, kid string
	pub         ed25519.PublicKey
}

func (d twoKeyDirectory) Key(_ context.Context, origin, kid string) (ed25519.PublicKey, error) {
	for _, k := range d {
		if k.origin == origin && k.kid == kid {
			return k.pub, nil
		}
	}
	return nil, agentsig.ErrUnknownKey
}
