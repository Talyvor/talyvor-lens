package main

import (
	"context"
	"encoding/json"
	"errors"
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
		('agt_b5410_paused', $1, 'Paused bot', NULL), ('agt_b5410_bare', $1, 'Bare bot', NULL), ('agt_b5410_rules', $1, 'Ruled bot', NULL)`, ws); err != nil {
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

	var first string
	t.Run("a mandate scoped to the agent its handle names judges it", func(t *testing.T) {
		w := ask("@seller-b5410", "inv-1")
		var out struct{ ID string }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("POST to @seller-b5410 = %d %s, want 201", w.Code, w.Body)
		}
		first = out.ID
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

	// item is the one item of the request an outside agent asks of address.
	item := func(t *testing.T, address, key string) economy.PaymentRequestItem {
		t.Helper()
		w := ask(address, key)
		var out struct{ ID string }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("POST to %s = %d %s, want 201", address, w.Code, w.Body)
		}
		pr, err := store.PaymentRequest(ctx, ws, "", out.ID)
		if err != nil || len(pr.Items) != 1 || pr.AgentID != address {
			t.Fatalf("request %s = %+v %v, want one item for %s", out.ID, pr, err, address)
		}
		return pr.Items[0]
	}

	t.Run("a mandate scoped to another agent does not judge it", func(t *testing.T) {
		if it := item(t, "agt_b5410_bare", "inv-3"); it.Decision != economy.ItemNeedsApproval || it.MandateID == mandate.ID {
			t.Errorf("item = %s by mandate %q, want needs_approval and not agt_b5410's mandate %s", it.Decision, it.MandateID, mandate.ID)
		}
	})

	t.Run("the agent's own rules block a payee", func(t *testing.T) {
		if _, err := store.SetAgentRules(ctx, ws, "agt_b5410_rules", economy.AgentRules{BlockedPayees: []string{"payee_acme"}}); err != nil {
			t.Fatal(err)
		}
		if it := item(t, "agt_b5410_rules", "inv-4"); it.Decision != economy.ItemBlocked || it.Reason != "the agent's rules block this payee" {
			t.Errorf("item = %s %q, want blocked by the agent's rules", it.Decision, it.Reason)
		}
	})

	t.Run("the agent's own key cannot read, list, cancel or pay it", func(t *testing.T) {
		if _, err := store.PaymentRequest(ctx, ws, "agt_b5410", first); !errors.Is(err, economy.ErrPaymentRequestNotFound) {
			t.Errorf("the agent reads it: %v, want ErrPaymentRequestNotFound", err)
		}
		if prs, err := store.PendingPaymentRequests(ctx, ws, "agt_b5410"); err != nil || len(prs) != 0 {
			t.Errorf("the agent lists %d (%v), want none", len(prs), err)
		}
		if _, err := store.CancelPaymentRequest(ctx, ws, "agt_b5410", first, "key:agt_b5410"); !errors.Is(err, economy.ErrPaymentRequestNotFound) {
			t.Errorf("the agent cancels it: %v, want ErrPaymentRequestNotFound", err)
		}
		if _, err := store.PayPaymentRequest(ctx, ws, "agt_b5410", first); !errors.Is(err, economy.ErrPaymentRequestNotFound) {
			t.Errorf("the agent pays it: %v, want ErrPaymentRequestNotFound", err)
		}
		if pr, err := store.PaymentRequest(ctx, ws, "", first); err != nil || pr.Status != "pending_approval" {
			t.Errorf("the owner's request = %s (%v), want still pending_approval", pr.Status, err)
		}
	})
}

// B54.10 — once the owner approves it, an outside agent's request pays from the addressed agent's own account in its
// currency, and from the company's account when the agent holds none: the ledger says which.
func TestB5410_AnApprovedOutsideRequestPaysFromTheAgentsAccountElseTheCompanys(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	b := b501Payer(t, pool, "ws-b5410-pay")
	ws := b.acct.WorkspaceID
	b.store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	// B50.20: a passkey enrols freely only as the workspace's first key, so the payer's stand-in device key goes first.
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_device_keys WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	sign := b5018Passkey(t, b.store, ws, rpID, origin)
	pennyless, err := b.store.CreateAgent(ctx, ws, "No pounds", "owner")
	if err != nil {
		t.Fatal(err)
	}
	var company, partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT (SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'company' AND currency = 'GBP'),
		(SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP')`, ws).Scan(&company, &partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "company-top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -30000}, {AccountID: company, AmountMinor: 30000}}}); err != nil {
		t.Fatal(err)
	}

	lensURL, _ := url.Parse("https://lens.example")
	sigs := agentsig.Store{Pool: pool}
	r := chi.NewRouter()
	mountOutsideAgentPaymentRoutes(r, &agentsig.Verifier{PublicURL: lensURL, Keys: twoKeyDirectory{}, Nonces: sigs, Businesses: sigs, Now: time.Now}, b.store)
	askApprovePay := func(t *testing.T, agentID, key string, minor int64) economy.PaymentRequestItem {
		t.Helper()
		body := `{"items":[{"payee_id":"` + b.payee.ID + `","currency":"GBP","amount_minor":` + jsonOf(minor) + `,"reference":"INV-` + key + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/a2a/agents/"+agentID+"/payment-requests", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out struct{ ID string }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("POST to %s = %d %s, want 201", agentID, w.Code, w.Body)
		}
		statement, challenge, _, err := b.store.PrepareRequestApproval(ctx, ws, out.ID, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.store.ApprovePaymentRequest(ctx, ws, out.ID, "ada", statement, sign(challenge)); err != nil {
			t.Fatal(err)
		}
		pr, err := b.store.PayPaymentRequest(ctx, ws, "", out.ID)
		if err != nil || len(pr.Items) != 1 {
			t.Fatalf("paying %s = %+v %v", out.ID, pr, err)
		}
		return pr.Items[0]
	}
	paidFrom := func(t *testing.T, paymentID string) string {
		t.Helper()
		var account string
		if err := pool.QueryRow(ctx, `SELECT account_id FROM money_payments WHERE id = $1`, paymentID).Scan(&account); err != nil {
			t.Fatalf("payment %q: %v", paymentID, err)
		}
		return account
	}
	companyBalance := func(t *testing.T, want int64) {
		t.Helper()
		if got, err := b.store.Balance(ctx, ws, company); err != nil || got.TestMinor != want {
			t.Fatalf("the company's account holds %+v (%v), want %d", got, err, want)
		}
	}

	t.Run("the agent holds pounds: its own account pays", func(t *testing.T) {
		it := askApprovePay(t, b.acct.AgentID, "own", 2000)
		if it.PaymentID == "" || paidFrom(t, it.PaymentID) != b.acct.ID {
			t.Fatalf("item = %+v, want paid from the agent's account %s", it, b.acct.ID)
		}
		b.balance(t, 48000)
		companyBalance(t, 30000)
	})

	t.Run("the agent holds no pounds: the company's account pays", func(t *testing.T) {
		it := askApprovePay(t, pennyless.ID, "company", 1500)
		if it.PaymentID == "" || paidFrom(t, it.PaymentID) != company {
			t.Fatalf("item = %+v, want paid from the company's account %s", it, company)
		}
		b.balance(t, 48000)
		companyBalance(t, 28500)
	})

	t.Run("the agent's pounds account was closed: nobody pays", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose, parent_account_id, status)
			VALUES ('macc_b5410_closed', $1, $2, 'GBP', 'agent', $3, 'closed')`, ws, pennyless.ID, company); err != nil {
			t.Fatal(err)
		}
		if it := askApprovePay(t, pennyless.ID, "closed", 1200); it.PaymentID != "" || it.PayStatus != economy.PayFailed {
			t.Fatalf("item = %+v, want failed and unpaid", it)
		}
		b.balance(t, 48000)
		companyBalance(t, 28500)
	})
	b501ZeroSum(t, pool)
}
