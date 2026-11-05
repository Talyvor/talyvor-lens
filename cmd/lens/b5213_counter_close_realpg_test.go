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

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B52.13 — through the routes: inside the mandate's authority, terms that meet every must-have, sit inside the
// walk-away and are paid inside the agent's limits are accepted at once and paid through the payment request path;
// a deal beyond the mandate — an authority short of accepting, or a payment above the agent's limits — waits for the
// owner's signature on a statement of the counterparty's exact terms against the mandate, and pays nothing until
// signed; an altered statement is refused; signing it twice pays once.
func TestB5213_CounterClosesInsideTheMandateAndAsksTheOwnerForTheRest(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b5213", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -5000000}, {AccountID: company.ID, AmountMinor: 5000000}}}); err != nil {
		t.Fatal(err)
	}
	acme, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Acme Ltd", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET called_at = now() WHERE id = $1`, acme.ID); err != nil {
		t.Fatal(err)
	}
	sign := b5018Passkey(t, store, ws, rpID, origin)
	// The agent's limits: Counter may pay Acme up to 11,500.00 GBP a payment on its own.
	pc, pch, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{economy.ClientCounter},
		Scope:  economy.MandateScope{Payees: []string{acme.ID}, Currencies: []string{"GBP"}},
		Limits: economy.MandateLimits{PerPayment: map[string]int64{"GBP": 1150000}}, NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee},
		ExpiresAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePermissionMandate(ctx, ws, "ada", pc, sign(pch)); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	mountNegotiationDealRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s: %v in %s", path, err, w.Body.String())
			}
		}
		return w.Code, w.Body.String()
	}
	mandate := func(authority string) string {
		t.Helper()
		var prep struct{ Canonical, Challenge string }
		if code, msg := call(http.MethodPost, "/v1/negotiations/prepare", `{"task":{"kind":"renewal","what":"Acme CRM, 40 seats"},
			"side":"buy","currency":"GBP","target_minor":1000000,"walk_away_minor":1200000,"must_haves":["99.9% uptime"],"deadline":"`+
			time.Now().Add(48*time.Hour).Format(time.RFC3339)+`","counterparties":["acme.example"],"channels":["agent"],"authority":"`+authority+`"}`, &prep); code != http.StatusOK {
			t.Fatalf("prepare = %d %s", code, msg)
		}
		b, _ := json.Marshal(map[string]any{"canonical": prep.Canonical, "assertion": sign(prep.Challenge)})
		var m economy.NegotiationMandate
		if code, msg := call(http.MethodPost, "/v1/negotiations", string(b), &m); code != http.StatusCreated {
			t.Fatalf("keep = %d %s", code, msg)
		}
		return m.ID
	}
	theirs := func(id string, price int64) {
		t.Helper()
		if code, msg := call(http.MethodPost, "/v1/negotiations/"+id+"/terms", fmt.Sprintf(`{"counterparty":"acme.example","party":"theirs",
			"channel":"agent","terms":{"price_minor":%d,"term_months":12,"sla":"99.9%% uptime","must_haves_met":["99.9%% uptime"]}}`, price), nil); code != http.StatusCreated {
			t.Fatalf("their offer = %d %s", code, msg)
		}
	}
	closeDeal := func(id string) (int, string, economy.NegotiationDeal) {
		t.Helper()
		var d economy.NegotiationDeal
		code, msg := call(http.MethodPost, "/v1/negotiations/"+id+"/close", `{"counterparty":"acme.example","payee_id":"`+acme.ID+`"}`, &d)
		return code, msg, d
	}
	// The ledger: each payment out is one entry moving its amount to the partner.
	paymentsOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}

	// Inside the mandate: accepted at once, recorded, paid once under the agent's limits, and the negotiation is closed.
	a := mandate(economy.AuthorityAccept)
	theirs(a, 1100000)
	code, msg, deal := closeDeal(a)
	if code != http.StatusOK || deal.Status != "accepted" || deal.Terms.PriceMinor != 1100000 || len(deal.Outside) != 0 || deal.RequestID == "" {
		t.Fatalf("an in-mandate close = %d %s", code, msg)
	}
	if n, minor := paymentsOut(); n != 1 || minor != 1100000 {
		t.Fatalf("after the in-mandate close the ledger has %d payments out of %d, want 1 of 1100000", n, minor)
	}
	var closed economy.NegotiationMandate
	if code, msg := call(http.MethodGet, "/v1/negotiations/"+a, "", &closed); code != http.StatusOK || closed.Status != "closed" ||
		closed.Closing == nil || closed.Closing.ID != deal.ID {
		t.Fatalf("the closed negotiation reads %d %s", code, msg)
	}

	// Beyond the mandate's authority: it waits for the owner and pays nothing.
	b := mandate(economy.AuthorityNegotiate)
	theirs(b, 1050000) // another amount: the same payee paid the same amount twice is flagged a duplicate (B41.103)
	code, msg, waiting := closeDeal(b)
	if code != http.StatusAccepted || waiting.Status != "awaiting_approval" || len(waiting.Outside) == 0 || waiting.RequestID != "" {
		t.Fatalf("an out-of-mandate close = %d %s", code, msg)
	}
	// A payment above the agent's limits waits for the owner too, though the mandate lets Counter accept.
	c := mandate(economy.AuthorityAccept)
	theirs(c, 1180000)
	if code, msg, over := closeDeal(c); code != http.StatusAccepted || over.Status != "awaiting_approval" ||
		!strings.Contains(strings.Join(over.Outside, " "), "limits.per_payment") {
		t.Fatalf("a close over the agent's limits = %d %s", code, msg)
	}
	if n, _ := paymentsOut(); n != 1 {
		t.Fatalf("a deal waiting for the owner paid: %d payments out", n)
	}

	// The owner reads the counterparty's exact terms against the mandate, and signs.
	var prep struct {
		Statement string `json:"statement"`
		Challenge string `json:"challenge"`
	}
	if code, msg := call(http.MethodPost, "/v1/negotiations/"+b+"/deal/approval", "", &prep); code != http.StatusOK ||
		!strings.Contains(prep.Statement, `"price_minor":1050000`) || !strings.Contains(prep.Statement, `"walk_away_minor":1200000`) ||
		!strings.Contains(prep.Statement, `"authority":"negotiate"`) {
		t.Fatalf("the approval statement = %d %s", code, msg)
	}
	approve := func(statement string, a *passkey.Assertion) (int, string) {
		bb, _ := json.Marshal(map[string]any{"statement": statement, "assertion": a})
		return call(http.MethodPost, "/v1/negotiations/"+b+"/deal/approve", string(bb), nil)
	}
	altered := strings.Replace(prep.Statement, `"price_minor":1050000`, `"price_minor":900000`, 1)
	if code, msg := approve(altered, sign(economy.MandateChallenge(altered))); code != http.StatusBadRequest {
		t.Fatalf("an altered statement = %d %s", code, msg)
	}
	if code, msg := approve(prep.Statement, sign(economy.MandateChallenge("another statement"))); code != http.StatusBadRequest {
		t.Fatalf("a signature over another statement = %d %s", code, msg)
	}
	if n, _ := paymentsOut(); n != 1 {
		t.Fatalf("an altered or unsigned statement paid: %d payments out", n)
	}
	if code, msg := approve(prep.Statement, sign(prep.Challenge)); code != http.StatusOK || !strings.Contains(msg, `"status":"accepted"`) {
		t.Fatalf("the owner's signature = %d %s", code, msg)
	}
	if code, msg := approve(prep.Statement, sign(prep.Challenge)); code < 400 {
		t.Fatalf("signing again = %d %s", code, msg)
	}
	if n, minor := paymentsOut(); n != 2 || minor != 2150000 {
		t.Fatalf("after the owner signed the ledger has %d payments out of %d, want 2 of 2150000", n, minor)
	}
	b501ZeroSum(t, pool)
}
