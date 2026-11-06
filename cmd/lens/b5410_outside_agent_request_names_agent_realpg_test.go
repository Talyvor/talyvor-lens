package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B54.10 — an outside agent's payment request is for the agent its address names: a mandate scoped to that agent judges
// it, and a paused agent's owner is not asked at all — without the owner's own words for the pause reaching the asker.
func TestB5410_OutsideAgentRequestIsForTheAgentItAddresses(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b5410", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, handle) VALUES ('agt_b5410', $1, 'Seller bot', 'seller-b5410'),
		('agt_b5410_paused', $1, 'Paused bot', NULL)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_accounts SET paused_at = now(), paused_reason = 'Ada is on holiday' WHERE id = 'agt_b5410_paused'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_acme', $1, 'Acme Inc.', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	sign := b5018Passkey(t, store, ws, rpID, origin)
	canonical, challenge, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"},
		AgentID:   "agt_b5410", Clients: []string{"a2a:unverified"},
		Scope: economy.MandateScope{Payees: []string{"payee_acme"}, Currencies: []string{"GBP"}},
		// above £50 the owner approves, so the item names this mandate by its threshold rather than being allowed
		RequiresApprovalAbove: map[string]int64{"GBP": 5000}, NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee},
		ExpiresAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	mandate, err := store.CreatePermissionMandate(ctx, ws, "ada", canonical, sign(challenge))
	if err != nil {
		t.Fatal(err)
	}

	lensURL, _ := url.Parse("https://lens.example")
	sigs := agentsig.Store{Pool: pool}
	r := chi.NewRouter()
	mountOutsideAgentPaymentRoutes(r, &agentsig.Verifier{PublicURL: lensURL, Keys: twoKeyDirectory{}, Nonces: sigs, Businesses: sigs, Now: time.Now}, store)
	ask := func(address, key string) *httptest.ResponseRecorder {
		body := `{"items":[{"payee_id":"payee_acme","currency":"GBP","amount_minor":9000,"reference":"INV-` + key + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/a2a/agents/"+address+"/payment-requests", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("a mandate scoped to the agent its handle names judges it", func(t *testing.T) {
		w := ask("@seller-b5410", "inv-1")
		var out struct{ ID string }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("POST to @seller-b5410 = %d %s, want 201", w.Code, w.Body)
		}
		pr, err := store.PaymentRequest(ctx, ws, "", out.ID)
		if err != nil || len(pr.Items) != 1 {
			t.Fatalf("request %s = %+v %v, want one item", out.ID, pr, err)
		}
		if pr.AgentID != "agt_b5410" {
			t.Errorf("request is for agent %q, want agt_b5410", pr.AgentID)
		}
		if it := pr.Items[0]; it.Decision != economy.ItemNeedsApproval || it.MandateID != mandate.ID || it.Rule != "requires_approval_above" {
			t.Errorf("item = %s %q by %q, want needs_approval naming the agent's mandate %s by requires_approval_above",
				it.Decision, it.MandateID, it.Rule, mandate.ID)
		}
	})

	t.Run("a paused agent's request is refused and recorded nowhere", func(t *testing.T) {
		w := ask("agt_b5410_paused", "inv-2")
		if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "holiday") {
			t.Fatalf("POST to a paused agent = %d %s, want 409 without the owner's pause reason", w.Code, w.Body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_requests WHERE workspace_id = $1 AND idempotency_key LIKE '%inv-2'`, ws).Scan(&n); err != nil || n != 0 {
			t.Fatalf("requests recorded for the paused agent = %d %v, want 0", n, err)
		}
	})
}
