package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.13 — on test money a company opens GBP and EUR accounts and an agent a GBP account, each at zero on the
// ledger; a live open without a clearance is refused naming the class, and opens nothing.
func TestMoneyAccountRoutes_ACompanyAndItsAgentOpenAccountsAtZero(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3013-accounts"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	agent, err := store.CreateAgent(ctx, ws, "Treasurer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/money/accounts", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	opened := map[string]map[string]any{}
	for name, body := range map[string]string{"company GBP": `{"currency":"GBP"}`, "company EUR": `{"currency":"eur","funding":"test"}`} {
		code, out := call(http.MethodPost, body)
		if code != http.StatusCreated || out["purpose"] != "company" || out["partner_account_ref"] == "" {
			t.Fatalf("open %s = %d %v, want 201, a company account at the partner", name, code, out)
		}
		opened[name] = out
	}
	code, out := call(http.MethodPost, `{"currency":"GBP","agent_id":"`+agent.ID+`"}`)
	if code != http.StatusCreated || out["agent_id"] != agent.ID || out["parent_account_id"] != opened["company GBP"]["id"] {
		t.Fatalf("open the agent's GBP = %d %v, want 201, a sub-account of the company's GBP account %s", code, out, opened["company GBP"]["id"])
	}
	opened["agent GBP"] = out
	if code, out := call(http.MethodPost, `{"currency":"GBP"}`); code != http.StatusConflict {
		t.Errorf("open the company's GBP again = %d %v, want 409", code, out)
	}

	// Each is at zero on the ledger: no posting names it, and its stored balance reads zero.
	for name, a := range opened {
		id := a["id"].(string)
		var postings int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_postings WHERE account_id = $1`, id).Scan(&postings); err != nil {
			t.Fatal(err)
		}
		b, err := store.Balance(ctx, ws, id)
		if err != nil || postings != 0 || b.AmountMinor != 0 {
			t.Errorf("%s %s: %d postings, balance %+v (%v), want none and zero", name, id, postings, b, err)
		}
	}
	// The company's accounts are mirrored by partner accounts under the same reference, for reconciliation (B30.11).
	var mirrored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_accounts p JOIN money_accounts c ON c.partner_account_ref = p.partner_account_ref
		WHERE p.purpose = 'partner' AND c.purpose = 'company' AND c.workspace_id = $1`, ws).Scan(&mirrored); err != nil || mirrored != 2 {
		t.Errorf("%d company accounts have a partner account under their reference (%v), want 2", mirrored, err)
	}
	if code, out := call(http.MethodGet, ""); code != http.StatusOK || len(out["accounts"].([]any)) != 3 {
		t.Fatalf("list = %d %v, want the three accounts", code, out)
	}

	// Live money: currency_accounts is RED and uncleared, so the open is refused naming the class, and nothing opens.
	code, out = call(http.MethodPost, `{"currency":"USD","funding":"live"}`)
	if msg, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(msg, "class RED") {
		t.Fatalf("a live USD open = %d %v, want 403 naming class RED", code, out)
	}
	var usd int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_accounts WHERE workspace_id = $1 AND currency = 'USD'`, ws).Scan(&usd); err != nil || usd != 0 {
		t.Errorf("the refused live open left %d USD accounts (%v), want none", usd, err)
	}
}

// B30.14, B49.1 — each company account's details read TEST and carry none of the Test partner's made-up details, an
// agent's account reads its own payment reference, and a test £120.00 quoting that reference — as a payer types it —
// lands in the agent's account as one balanced entry from the partner account. Details still read after Lens
// restarts, when the Test partner has forgotten the account.
func TestMoneyAccountRoutes_DetailsReadTESTAndAnAgentsReferenceRoutesMoneyToIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3014-details"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	agent, err := store.CreateAgent(ctx, ws, "Collector", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company := map[string]economy.CurrencyAccount{}
	for _, cur := range []string{"GBP", "EUR", "USD"} {
		if company[cur], err = store.OpenCurrencyAccount(ctx, ws, "", cur, "test"); err != nil {
			t.Fatal(err)
		}
	}
	agentGBP, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	// Lens restarts: a new registry's Test partner has never seen these accounts.
	store.SetAccountPartners(partners.NewRegistry(nil))

	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	details := func(id string) economy.CurrencyAccountDetails {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/money/accounts/"+id+"/details", nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var d economy.CurrencyAccountDetails
		if err := json.Unmarshal(w.Body.Bytes(), &d); w.Code != http.StatusOK || err != nil {
			t.Fatalf("details of %s = %d %s", id, w.Code, w.Body.String())
		}
		return d
	}
	// B49.1: the Test partner's details reach no bank, so the answer carries none of them — only TEST, and for an
	// agent's account the payment reference that routes money to it.
	for _, cur := range []string{"GBP", "EUR", "USD"} {
		d := details(company[cur].ID)
		if d.AccountDetails != (partners.AccountDetails{Currency: cur}) || d.Mode != economy.DetailsTest || d.PaymentReference != "" {
			t.Errorf("the company's %s details = %+v; want no made-up details, mode TEST, no payment reference", cur, d)
		}
	}
	ad := details(agentGBP.ID)
	if ad.AccountDetails != (partners.AccountDetails{Currency: "GBP"}) || ad.Mode != economy.DetailsTest || len(ad.PaymentReference) != 15 {
		t.Fatalf("the agent's GBP details = %+v; want no made-up details, TEST, and a TLV payment reference", ad)
	}

	// The payer types the reference in lower case, split up, after an invoice number.
	quoted := "inv 7 " + strings.ToLower(ad.PaymentReference[:7]+" "+ad.PaymentReference[7:])
	e, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: company["GBP"].PartnerAccountRef, PartnerRef: "test_in_b3014",
		Payer: "Acme Supplies Ltd", Reference: quoted, AmountMinor: 12000, Currency: "GBP"})
	if err != nil {
		t.Fatal(err)
	}
	var partnerID string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE purpose = 'partner' AND partner_account_ref = $1`,
		company["GBP"].PartnerAccountRef).Scan(&partnerID); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT account_id, amount_minor, currency, funding FROM money_postings WHERE entry_id = $1 ORDER BY line`, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var acct, cur, funding string
		var amount int64
		if err := rows.Scan(&acct, &amount, &cur, &funding); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %d %s %s", acct, amount, cur, funding))
	}
	rows.Close()
	want := []string{partnerID + " -12000 GBP test", agentGBP.ID + " 12000 GBP test"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("the payment in posted %v; want %v", got, want)
	}
	if b, _ := store.Balance(ctx, ws, agentGBP.ID); b.TestMinor != 12000 {
		t.Errorf("the agent's GBP balance = %+v; want 12000 test pence", b)
	}
	if b, _ := store.Balance(ctx, ws, company["GBP"].ID); b.AmountMinor != 0 {
		t.Errorf("the company's GBP balance = %+v; want 0: the money was the agent's", b)
	}
}

// B17.255 — the owner adds test money to the company's GBP account: one balanced payment in from the partner account,
// funding test; the same key again posts nothing more; a key without the owner's scope is refused and moves nothing.
func TestMoneyAccountRoutes_TheOwnerAddsTestMoneyToTheCompanyAccountOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b17255-test-money"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	add := func(who *auth.AuthContext, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/money/accounts/"+company.ID+"/test-money", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent"}
	if code, body := add(agentKey, `{"amount_minor":5000,"idempotency_key":"k1"}`); code != http.StatusForbidden {
		t.Errorf("a key without the owner's scope = %d %s, want 403", code, body)
	}
	for range 2 {
		if code, body := add(owner, `{"amount_minor":5000,"idempotency_key":"k1"}`); code != http.StatusCreated {
			t.Fatalf("the owner adds £50.00 of test money = %d %s, want 201", code, body)
		}
	}
	if code, body := add(owner, `{"amount_minor":0,"idempotency_key":"k2"}`); code != http.StatusBadRequest {
		t.Errorf("no amount = %d %s, want 400", code, body)
	}
	var got []string
	rows, err := pool.Query(ctx, `SELECT a.purpose, p.amount_minor, p.funding FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		WHERE a.workspace_id = $1 ORDER BY p.amount_minor`, ws)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var purpose, funding string
		var amount int64
		if err := rows.Scan(&purpose, &amount, &funding); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %d %s", purpose, amount, funding))
	}
	rows.Close()
	if want := "partner -5000 test; company 5000 test"; strings.Join(got, "; ") != want {
		t.Fatalf("the ledger holds %v; want %s: one payment in, posted once", got, want)
	}
	if b, _ := store.Balance(ctx, ws, company.ID); b.TestMinor != 5000 || b.LiveMinor != 0 {
		t.Errorf("the company's GBP balance = %+v; want 5000 test pence and no live money", b)
	}
}
