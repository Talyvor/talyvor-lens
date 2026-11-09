package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// B57.11 — every approval says what pays it, in that one's currency, the approval amount it is above and the budget
// left after it, on GET approvals and on the approval's challenge, on a database migrated from zero: an LXC payment to
// another agent, a listing's use on the marketplace bill, a card purchase, an invoice of $340.00 paid from an agent's
// dollar account (judged as 3,400 LXC), an agent with no monthly limit, and a row filed before any of this was kept.
func TestApprovals_SayWhatPaysThemAndTheBudgetLeftAfter(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, sellerWS = "ws-b5711", "ws-b5711-seller"
	for _, q := range []string{
		// Test-funded, which a card (class RED, B22.1) may spend without a clearance.
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc, test_funded_ulxc) VALUES ('ws-b5711', 100000000000, 100000000000, 100000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-b5711', 'ws-b5711', 'ws-b5711')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	store.SetInvoicePayBase("https://app.talyvor.test")
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountInvoiceRoutes(r, store)
	mountInvoicePayRoutes(r, store, &fakeInvoiceCards{}, func(string) bool { return false })
	call := func(asWS, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: asWS, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	agent := func(name string, rules economy.AgentRules) economy.Agent {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.FundAgent(ctx, ws, a.ID, 10_000_000_000); err != nil { // 10,000 LXC
			t.Fatal(err)
		}
		if _, err := store.SetAgentRules(ctx, ws, a.ID, rules); err != nil {
			t.Fatal(err)
		}
		return a
	}
	const lxc = 1_000_000
	research := agent("Research", economy.AgentRules{ApprovalAboveULXC: 500 * lxc, MonthlyLimitULXC: 5_000 * lxc})
	sales := agent("Sales", economy.AgentRules{ApprovalAboveULXC: 500 * lxc})
	// The approval each case files, by id, as GET approvals answers it.
	approval := func(id string) map[string]any {
		t.Helper()
		code, out := call(ws, http.MethodGet, "/v1/workspaces/"+ws+"/agents/approvals", "")
		if code != http.StatusOK {
			t.Fatalf("GET approvals = %d %v", code, out)
		}
		for _, v := range out["approvals"].([]any) {
			if a := v.(map[string]any); a["id"] == id {
				return a
			}
		}
		t.Fatalf("approval %s is not on GET approvals: %v", id, out)
		return nil
	}
	filed := func(err error) string {
		t.Helper()
		var need *economy.ApprovalNeededError
		if !errors.As(err, &need) {
			t.Fatalf("the payment answered %v; want an approval filed", err)
		}
		return need.ApprovalID
	}
	expect := func(what string, a map[string]any, paidFrom string, asksAbove, left any) {
		t.Helper()
		var want any
		if err := json.Unmarshal([]byte(paidFrom), &want); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(a["paid_from"])
		if w, _ := json.Marshal(want); string(got) != string(w) || a["asks_above_ulxc"] != asksAbove || a["budget_left_after_ulxc"] != left {
			t.Errorf("%s: paid_from %s, asks_above_ulxc %v, budget_left_after_ulxc %v;\n want %s, %v, %v", what, got,
				a["asks_above_ulxc"], a["budget_left_after_ulxc"], paidFrom, asksAbove, left)
		}
	}

	// An LXC payment to another agent: 900 LXC from Research's 10,000, above 500, leaves 4,100 of its 5,000 a month.
	_, err := store.PayAgent(ctx, ws, research.ID, sales.ID, 900*lxc, "data")
	payment := approval(filed(err))
	expect("an LXC payment", payment, `{"kind":"lxc","account_id":"`+research.ID+`","name":"Research’s LXC balance","currency":"LXC",`+
		`"amount_minor":900000000,"balance_minor":10000000000}`, float64(500*lxc), float64(4_100*lxc))

	// The agent asking for the approval itself, before it pays (B19.9).
	asked, err := store.RequestPaymentApproval(ctx, ws, research.ID, sales.ID, 900*lxc, "more data", "a second batch")
	if err != nil {
		t.Fatal(err)
	}
	expect("an approval the agent asked for", approval(asked.ID), `{"kind":"lxc","account_id":"`+research.ID+`","name":"Research’s LXC balance",`+
		`"currency":"LXC","amount_minor":900000000,"balance_minor":10000000000}`, float64(500*lxc), float64(4_100*lxc))

	// Another agent with no monthly limit: no budget to be left.
	_, err = store.PayAgent(ctx, ws, sales.ID, research.ID, 900*lxc, "data")
	expect("no monthly limit", approval(filed(err)), `{"kind":"lxc","account_id":"`+sales.ID+`","name":"Sales’s LXC balance","currency":"LXC",`+
		`"amount_minor":900000000,"balance_minor":10000000000}`, float64(500*lxc), nil)

	// A listing's use goes on the monthly marketplace bill: 900 LXC is $90.00 at the peg.
	err = store.JudgeAgentPurchase(ctx, ws, research.ID, "lst_b5711_leads", 900*lxc, economy.Commitment{}, "Lead finder", "",
		func(pgx.Tx) error { return nil })
	expect("a listing's use", approval(filed(err)), `{"kind":"marketplace_bill","account_id":"`+ws+`","name":"Your monthly marketplace bill",`+
		`"currency":"USD","amount_minor":9000,"balance_minor":null}`, float64(500*lxc), float64(4_100*lxc))

	// A card purchase of $90.00 on Research's card.
	card, err := store.SaveAgentCard(ctx, ws, economy.AgentCard{ID: "ic_test_b5711", AgentID: research.ID, StripeCardholderID: "ich_test_b5711",
		Last4: "4242", ExpMonth: 9, ExpYear: 2029, Currency: "usd"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: "evt_b5711", AuthorizationID: "iauth_b5711", CardID: card.ID,
		AmountMinor: 9000, Currency: "usd", MerchantAmountMinor: 9000, MerchantCurrency: "usd", MerchantName: "Paper Co", MerchantID: "m-b5711",
		At: time.Now(), USDMicros: 90_000_000})
	if err != nil || d.Approved || d.ApprovalID == "" {
		t.Fatalf("the card purchase = %+v, %v; want it declined pending an approval", d, err)
	}
	expect("a card purchase", approval(d.ApprovalID), `{"kind":"card","account_id":"ic_test_b5711","name":"Research’s card",`+
		`"currency":"USD","amount_minor":9000,"balance_minor":null}`, float64(500*lxc), float64(4_100*lxc))

	// An invoice of $340.00 from another company, paid from Research's dollar account holding $1,000.00: the rules judge
	// it as 3,400 LXC, above 2,500, and the sheet shows the $340.00 the account pays.
	if _, err := store.SetAgentRules(ctx, ws, research.ID, economy.AgentRules{ApprovalAboveULXC: 2_500 * lxc, MonthlyLimitULXC: 50_000 * lxc}); err != nil {
		t.Fatal(err)
	}
	seller, err := store.CreateAgent(ctx, sellerWS, "Acme Hosting", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, sellerWS, "", "USD", "test"); err != nil {
		t.Fatal(err)
	}
	sellerUSD, err := store.OpenCurrencyAccount(ctx, sellerWS, seller.ID, "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	researchUSD, err := store.OpenCurrencyAccount(ctx, ws, research.ID, "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	details, err := store.CurrencyAccountDetails(ctx, ws, "", researchUSD.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: company.PartnerAccountRef, PartnerRef: "test_in_b5711_fund",
		Payer: "Research Ltd", Reference: details.PaymentReference, AmountMinor: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().AddDate(0, 0, 14).Format(time.DateOnly)
	code, inv := call(sellerWS, http.MethodPost, "/v1/money/invoices", `{"account_id":"`+sellerUSD.ID+`","customer_name":"Research Ltd",
		"customer_email":"ap@research.test","lines":[{"description":"Hosting","quantity":1,"unit_amount_minor":34000}],"due_date":"`+due+`","send":true}`)
	if code != http.StatusCreated {
		t.Fatalf("issue the invoice = %d %v", code, inv)
	}
	_, err = store.PayInvoiceByAgent(ctx, inv["pay_token"].(string), ws, research.ID, "test")
	id := filed(err)
	invoice := approval(id)
	if invoice["amount_ulxc"] != float64(3_400*lxc) {
		t.Errorf("the invoice's approval is of %v µLXC; want 3,400 LXC", invoice["amount_ulxc"])
	}
	expect("an invoice from a dollar account", invoice, `{"kind":"account","account_id":"`+researchUSD.ID+`","name":"Research’s US dollar account",`+
		`"currency":"USD","amount_minor":34000,"balance_minor":100000}`, float64(2_500*lxc), float64((50_000-3_400)*lxc))

	// The challenge the owner signs carries the approval as the sheet shows it.
	code, out := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/agents/approvals/"+id+"/challenge", "")
	signed, _ := out["approval"].(map[string]any)
	if code != http.StatusOK || signed == nil {
		t.Fatalf("the challenge = %d %v; want the approval with it", code, out)
	}
	expect("the challenge's approval", signed, `{"kind":"account","account_id":"`+researchUSD.ID+`","name":"Research’s US dollar account",`+
		`"currency":"USD","amount_minor":34000,"balance_minor":100000}`, float64(2_500*lxc), float64((50_000-3_400)*lxc))

	// A row filed before B57.11 says nothing of what pays it.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, expires_at)
		VALUES ('apr_b5711_old', $1, $2, 'old', 900000000, now() + interval '1 hour')`, ws, research.ID); err != nil {
		t.Fatal(err)
	}
	old := approval("apr_b5711_old")
	if _, ok := old["paid_from"]; !ok {
		t.Errorf("an old row has no paid_from key; want it null")
	}
	expect("an old row", old, "null", nil, nil)
}
