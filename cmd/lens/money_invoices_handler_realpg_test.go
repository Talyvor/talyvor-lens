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
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

type fakeInvoiceCards struct{ asked billing.InvoiceCheckout }

func (f *fakeInvoiceCards) CreateInvoiceCheckoutSession(_ context.Context, p billing.InvoiceCheckout) (string, error) {
	f.asked = p
	return "https://checkout.stripe.test/c/pay/cs_test_b3020", nil
}

// B30.20 — an agent issues a £120.00 invoice (£100.00 and 20% VAT). Its pay link's card button opens a Stripe
// Checkout Session for £120.00, and the signed checkout.session.completed posts £120.00 into the agent's account once,
// however often Stripe sends it, and the invoice reads paid. Another company's agent pays a £30.00 invoice from its own
// account: £30.00 moves from its account to the issuer's, once, and the invoice reads paid. A transfer quoting the
// invoice's reference marks a third one paid; a fourth, past its due date, reads overdue and is sent one reminder.
func TestInvoiceRoutes_AnInvoicePaidByCardOrByAnAgentPostsToTheIssuer(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, payerWS, secret = "ws-b3020-issuer", "ws-b3020-payer", "whsec_b3020"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	store.SetInvoicePayBase("https://app.talyvor.test")
	seller, err := store.CreateAgent(ctx, ws, "Seller", "owner")
	if err != nil {
		t.Fatal(err)
	}
	buyer, err := store.CreateAgent(ctx, payerWS, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	sellerGBP, err := store.OpenCurrencyAccount(ctx, ws, seller.ID, "GBP", "test")
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
	buyerDetails, err := store.CurrencyAccountDetails(ctx, payerWS, "", buyerGBP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: payerCompany.PartnerAccountRef, PartnerRef: "test_in_b3020_fund",
		Payer: "Buyer Ltd", Reference: buyerDetails.PaymentReference, AmountMinor: 10000, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}

	cards := &fakeInvoiceCards{}
	r := chi.NewRouter()
	mountInvoiceRoutes(r, store)
	mountInvoicePayRoutes(r, store, cards)
	r.Post("/v1/billing/webhook", billing.New(pool, store, nil, secret).WithInvoices(store).HandleWebhook)
	call := func(asWS, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if asWS != "" {
			req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: asWS, AuthMethod: auth.MethodJWT,
				UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		}
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
		return b.TestMinor
	}
	due := time.Now().UTC().AddDate(0, 0, 14).Format(time.DateOnly)
	issue := func(lines string) map[string]any {
		t.Helper()
		code, inv := call(ws, http.MethodPost, "/v1/money/invoices", `{"account_id":"`+sellerGBP.ID+`","customer_name":"Buyer Ltd",
			"customer_email":"ap@buyer.test","lines":`+lines+`,"due_date":"`+due+`","send":true}`)
		if code != http.StatusCreated || inv["status"] != economy.InvoiceSent {
			t.Fatalf("issue an invoice = %d %v", code, inv)
		}
		return inv
	}
	token := func(inv map[string]any) string { return inv["pay_token"].(string) }

	// By card.
	byCard := issue(`[{"description":"Consulting","quantity":2,"unit_amount_minor":5000,"vat_rate_bps":2000}]`)
	if byCard["total_minor"] != float64(12000) || byCard["vat_minor"] != float64(2000) || byCard["number"] != "INV-000001" {
		t.Fatalf("the invoice reads %v; want INV-000001, £100.00 and £20.00 VAT, £120.00 in all", byCard)
	}
	code, page := call("", http.MethodGet, "/v1/pay/"+token(byCard), "")
	transfer, _ := page["transfer"].(map[string]any)
	if code != http.StatusOK || page["card"] != true || page["notice"] == nil || transfer == nil ||
		!strings.Contains(transfer["reference"].(string), byCard["reference"].(string)) || page["invoice"].(map[string]any)["workspace_id"] != nil {
		t.Fatalf("the pay page = %d %v; want a card, a transfer quoting the invoice's reference, the notice, and no issuer ids", code, page)
	}
	if code, out := call("", http.MethodPost, "/v1/pay/"+token(byCard)+"/card", ""); code != http.StatusCreated || out["url"] == "" ||
		cards.asked.AmountMinor != 12000 || cards.asked.Currency != "GBP" {
		t.Fatalf("the card button = %d %v, asking Stripe for %+v; want a Checkout Session for 12000 GBP", code, out, cards.asked)
	}
	session, _ := json.Marshal(map[string]any{"id": "evt_b3020_card", "object": "event", "type": "checkout.session.completed",
		"livemode": false, "created": time.Now().Unix(), "data": map[string]any{"object": map[string]any{"id": "cs_test_b3020", "object": "checkout.session",
			"mode": "payment", "payment_status": "paid", "amount_total": 12000, "currency": "gbp", "livemode": false,
			"customer_details": map[string]any{"name": "Buyer Ltd", "email": "ap@buyer.test"},
			"metadata":         map[string]string{"talyvor_invoice_id": byCard["id"].(string)}}}})
	for range 2 { // Stripe sends it twice: it posts once
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: session, Secret: secret})
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(string(session)))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("checkout.session.completed = %d %s", w.Code, w.Body.String())
		}
	}
	if got := held(ws, sellerGBP.ID); got != 12000 {
		t.Fatalf("the seller's account holds %d after the card payment; want 12000", got)
	}
	var postings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		WHERE e.kind = 'invoice_card' AND p.account_id = $1 AND p.amount_minor = 12000 AND p.currency = 'GBP' AND p.funding = 'test'`,
		sellerGBP.ID).Scan(&postings); err != nil || postings != 1 {
		t.Fatalf("%d card postings of +12000 GBP test to the seller (%v); want 1", postings, err)
	}

	// By another company's agent, whose rules judge it (B30.131): £30.00 is $37.50 at the Test rate, 375 LXC. Capped at
	// 300 LXC a month the buyer is refused and posts nothing; at 700 LXC it pays, and a second £30.00 is refused because
	// the first counts toward the month.
	byAgent := issue(`[{"description":"Report","quantity":1,"unit_amount_minor":3000}]`)
	rules := func(monthlyULXC int64) {
		t.Helper()
		if _, err := store.SetAgentRules(ctx, payerWS, buyer.ID, economy.AgentRules{MonthlyLimitULXC: monthlyULXC}); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(inv map[string]any, wantPostings int) {
		t.Helper()
		code, out := call(payerWS, http.MethodPost, "/v1/pay/"+token(inv)+"/agent", `{"agent_id":"`+buyer.ID+`"}`)
		if msg, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(msg, "monthly limit") {
			t.Fatalf("the buyer pays past its monthly limit = %d %v; want 403 naming the limit", code, out)
		}
		var postings int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
			WHERE e.kind = 'invoice_payment' AND p.account_id = $1`, buyerGBP.ID).Scan(&postings); err != nil || postings != wantPostings {
			t.Fatalf("%d invoice postings on the buyer's account after a refusal (%v); want %d", postings, err, wantPostings)
		}
	}
	rules(300_000_000)
	refused(byAgent, 0)
	rules(700_000_000)
	for range 2 { // a retry pays nothing more
		code, out := call(payerWS, http.MethodPost, "/v1/pay/"+token(byAgent)+"/agent", `{"agent_id":"`+buyer.ID+`"}`)
		if code != http.StatusOK || out["invoice"].(map[string]any)["status"] != economy.InvoicePaid {
			t.Fatalf("the buyer pays = %d %v", code, out)
		}
	}
	if s, b := held(ws, sellerGBP.ID), held(payerWS, buyerGBP.ID); s != 15000 || b != 7000 {
		t.Fatalf("after the agent's payment the seller holds %d and the buyer %d; want 15000 and 7000", s, b)
	}
	if code, _ := call(payerWS, http.MethodPost, "/v1/pay/"+token(byCard)+"/agent", `{"agent_id":"`+buyer.ID+`"}`); code != http.StatusConflict {
		t.Fatalf("paying a paid invoice = %d; want 409", code)
	}
	secondThisMonth := issue(`[{"description":"Report, again","quantity":1,"unit_amount_minor":3000}]`)
	refused(secondThisMonth, 1)
	if b := held(payerWS, buyerGBP.ID); b != 7000 {
		t.Fatalf("the buyer holds %d after the refused second invoice; want 7000", b)
	}

	// By transfer, and overdue.
	byTransfer := issue(`[{"description":"Hosting","quantity":1,"unit_amount_minor":2500}]`)
	overdue := issue(`[{"description":"Support","quantity":1,"unit_amount_minor":1000}]`)
	sellerDetails, err := store.CurrencyAccountDetails(ctx, ws, "", sellerGBP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: company.PartnerAccountRef, PartnerRef: "test_in_b3020_transfer",
		Payer: "Buyer Ltd", Reference: strings.ToLower(sellerDetails.PaymentReference + " " + byTransfer["reference"].(string)),
		AmountMinor: 2500, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_invoices SET due_date = current_date - 1 WHERE id = $1`, overdue["id"]); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // the overdue reminder is sent once
		sent, err := store.SweepInvoices(ctx, time.Now())
		if err != nil || len(sent) != 1-i {
			t.Fatalf("sweep %d sent %v (%v); want the overdue reminder once", i+1, sent, err)
		}
		if i == 0 && (sent[0].InvoiceID != overdue["id"] || sent[0].Kind != "overdue" || sent[0].SentTo != "ap@buyer.test") {
			t.Fatalf("the reminder sent is %+v; want the overdue one's, to ap@buyer.test", sent[0])
		}
	}
	_, list := call(ws, http.MethodGet, "/v1/money/invoices", "")
	want := map[string]string{byCard["id"].(string): economy.InvoicePaid, byAgent["id"].(string): economy.InvoicePaid,
		byTransfer["id"].(string): economy.InvoicePaid, overdue["id"].(string): economy.InvoiceOverdue,
		secondThisMonth["id"].(string): economy.InvoiceSent}
	for _, v := range list["invoices"].([]any) {
		inv := v.(map[string]any)
		if inv["status"] != want[inv["id"].(string)] {
			t.Errorf("%s reads %s; want %s", inv["number"], inv["status"], want[inv["id"].(string)])
		}
		if inv["id"] == overdue["id"] && len(inv["reminders"].([]any)) != 1 {
			t.Errorf("the overdue invoice shows %v; want its one reminder", inv["reminders"])
		}
	}
	if got := held(ws, sellerGBP.ID); got != 17500 {
		t.Fatalf("the seller's account holds %d after the transfer; want 17500", got)
	}
}
