package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.38 — a test checkout paid by another company's agent posts to the merchant's account less Talyvor's 1% fee, a
// refund reverses it, and both statements show it.
func TestMerchantCheckouts_AnAgentPaysTheMerchantAndARefundReversesIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, payerWS = "ws-b3038-merchant", "ws-b3038-payer"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	buyer, err := store.CreateAgent(ctx, payerWS, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	merchantGBP, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payerCompany, err := store.OpenCurrencyAccount(ctx, payerWS, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	buyerGBP, err := store.OpenCurrencyAccount(ctx, payerWS, buyer.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	details, err := store.CurrencyAccountDetails(ctx, payerWS, "", buyerGBP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: payerCompany.PartnerAccountRef, PartnerRef: "test_in_b3038_fund",
		Payer: "Buyer Ltd", Reference: details.PaymentReference, AmountMinor: 10000, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Minute)

	r := chi.NewRouter()
	mountMerchantCheckoutRoutes(r, store)
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
	held := func(ws, account string) int64 {
		t.Helper()
		b, err := store.Balance(ctx, ws, account)
		if err != nil {
			t.Fatal(err)
		}
		return b.AmountMinor
	}

	code, co := call(ws, http.MethodPost, "/v1/merchant/checkouts",
		`{"amount_minor":5000,"currency":"GBP","order_id":"order-1","return_url":"https://shop.example/done"}`)
	if code != http.StatusCreated || co["status"] != economy.CheckoutOpen {
		t.Fatalf("open a checkout = %d %v", code, co)
	}
	id := co["id"].(string)
	pay := "/v1/merchant/checkouts/" + id + "/pay"

	// The payer's rules judge it: £50.00 is 625 LXC at the Test rate, so a 300 LXC monthly limit refuses it and posts nothing.
	if _, err := store.SetAgentRules(ctx, payerWS, buyer.ID, economy.AgentRules{MonthlyLimitULXC: 300_000_000}); err != nil {
		t.Fatal(err)
	}
	if code, out := call(payerWS, http.MethodPost, pay, `{"agent_id":"`+buyer.ID+`"}`); code != http.StatusForbidden {
		t.Fatalf("paying past the monthly limit = %d %v; want 403", code, out)
	}
	if b := held(payerWS, buyerGBP.ID); b != 10000 {
		t.Fatalf("the buyer holds %d after a refusal; want 10000", b)
	}
	if _, err := store.SetAgentRules(ctx, payerWS, buyer.ID, economy.AgentRules{MonthlyLimitULXC: 1_000_000_000}); err != nil {
		t.Fatal(err)
	}
	var paid map[string]any
	for range 2 { // a retry pays nothing more
		code, paid = call(payerWS, http.MethodPost, pay, `{"agent_id":"`+buyer.ID+`"}`)
		if code != http.StatusOK || paid["checkout"].(map[string]any)["status"] != economy.CheckoutPaid {
			t.Fatalf("the buyer pays = %d %v", code, paid)
		}
	}
	entry := paid["entry"].(map[string]any)["id"].(string)

	// The ledger: the buyer −5000, the merchant +4950 and Talyvor's revenue +50, test money, summing to zero.
	ledger := func(entryID string) map[string]int64 {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT a.purpose, p.amount_minor FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
			WHERE p.entry_id = $1 AND p.currency = 'GBP' AND p.funding = 'test'`, entryID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var purpose string
			var amount int64
			if err := rows.Scan(&purpose, &amount); err != nil {
				t.Fatal(err)
			}
			out[purpose] += amount
		}
		return out
	}
	if got := ledger(entry); len(got) != 3 || got["agent"] != -5000 || got["company"] != 4950 || got["revenue"] != 50 {
		t.Fatalf("the payment's postings are %v; want agent −5000, company +4950, revenue +50", got)
	}
	if m, b := held(ws, merchantGBP.ID), held(payerWS, buyerGBP.ID); m != 4950 || b != 5000 {
		t.Fatalf("after paying the merchant holds %d and the buyer %d; want 4950 and 5000", m, b)
	}
	if code, _ := call(payerWS, http.MethodPost, "/v1/merchant/checkouts/"+id+"/refund", ``); code != http.StatusNotFound {
		t.Fatalf("another company refunds it = %d; want 404", code)
	}
	code, refund := call(ws, http.MethodPost, "/v1/merchant/checkouts/"+id+"/refund", ``)
	if code != http.StatusOK || refund["checkout"].(map[string]any)["status"] != economy.CheckoutRefunded {
		t.Fatalf("refund = %d %v", code, refund)
	}
	if got := ledger(refund["entry"].(map[string]any)["id"].(string)); got["agent"] != 5000 || got["company"] != -4950 || got["revenue"] != -50 {
		t.Fatalf("the refund's postings are %v; want agent +5000, company −4950, revenue −50", got)
	}
	if m, b := held(ws, merchantGBP.ID), held(payerWS, buyerGBP.ID); m != 0 || b != 10000 {
		t.Fatalf("after the refund the merchant holds %d and the buyer %d; want 0 and 10000", m, b)
	}

	// Both statements show the payment and its refund.
	kinds := func(ws, agentID, account string) string {
		t.Helper()
		st, err := store.AccountStatement(ctx, ws, agentID, account, start, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		var k []string
		for _, l := range st.Lines {
			k = append(k, l.Kind)
		}
		return strings.Join(k, " ")
	}
	if got := kinds(ws, "", merchantGBP.ID); got != "merchant_payment merchant_refund" {
		t.Errorf("the merchant's statement reads %q", got)
	}
	if got := kinds(payerWS, buyer.ID, buyerGBP.ID); !strings.HasSuffix(got, "merchant_payment merchant_refund") {
		t.Errorf("the buyer's statement reads %q", got)
	}
	if code, list := call(ws, http.MethodGet, "/v1/merchant/checkouts", ``); code != http.StatusOK || len(list["checkouts"].([]any)) != 1 {
		t.Errorf("the merchant's checkouts = %d %v", code, list)
	}
}
