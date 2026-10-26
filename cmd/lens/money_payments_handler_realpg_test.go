package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.17 — on test money an agent pays a saved payee outside Talyvor: the payment holds the money and then moves it
// out through the partner; one that fails (an amount ending 13) and one returned (ending 14) give it back; one above
// the agent's approval amount files an approval and moves nothing until the owner approves it. All read off postings.
// B46.8: the same again with the money layer set, as main.go sets it, where every payment goes through the sandbox
// adapter's Transfers.
func TestMoneyPaymentRoutes_HoldThenCompleteFailOrReturnAndApproval(t *testing.T) {
	t.Run("through the account partner", func(t *testing.T) { moneyPaymentRoutesHoldThenComplete(t, false) })
	t.Run("through the money layer's sandbox", func(t *testing.T) { moneyPaymentRoutesHoldThenComplete(t, true) })
}

func moneyPaymentRoutesHoldThenComplete(t *testing.T, layered bool) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3017-payments"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	partnerName := "test"
	if layered {
		store.SetMoneyLayer(sandboxMoneyLayer(t, pool, store, registry))
		partnerName = money.SandboxName
	}
	agent, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	acct, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	// Above 1,000 LXC needs approval: £100.00 is $125.00 at the Test rate, 1,250 LXC; £20.00 is 250 LXC.
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000_000}); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{acct.ID: "agent"}
	rows, err := pool.Query(ctx, `SELECT id, purpose FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws)
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	for rows.Next() {
		var id, purpose string
		_ = rows.Scan(&id, &purpose)
		names[id], partnerAccount = purpose, id
	}
	rows.Close()
	// £500.00 of test money in, from outside, to the agent's account.
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -50000}, {AccountID: acct.ID, AmountMinor: 50000}}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	pay := func(key string, minor int) (int, map[string]any) {
		return call(http.MethodPost, "/v1/money/payments", `{"payee_id":"`+payee.ID+`","account_id":"`+acct.ID+`","currency":"GBP","amount_minor":`+
			jsonOf(minor)+`,"reference":"Invoice `+key+`","idempotency_key":"`+key+`"}`)
	}
	// postings is every posting of the payment under key, in order: "kind account amount".
	postings := func(key string) string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT e.kind, p.account_id, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.idempotency_key LIKE 'payment_out%:' || $2 ORDER BY e.created_at, p.line`, ws, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var kind, id string
			var amount int64
			_ = rows.Scan(&kind, &id, &amount)
			if names[id] == "" {
				var purpose string
				_ = pool.QueryRow(ctx, `SELECT purpose FROM money_accounts WHERE id = $1`, id).Scan(&purpose)
				names[id] = purpose
			}
			out = append(out, kind+" "+names[id]+" "+jsonOf(amount))
		}
		return strings.Join(out, "; ")
	}
	balance := func(want int64) {
		t.Helper()
		if b, err := store.Balance(ctx, ws, acct.ID); err != nil || b.TestMinor != want {
			t.Fatalf("the agent's account holds %+v (%v), want %d test pence", b, err, want)
		}
	}

	code, out := pay("ok", 2000)
	if code != http.StatusCreated || out["status"] != "completed" || out["partner_ref"] == "" || out["value_ulxc"] != float64(250_000_000) ||
		out["partner"] != partnerName {
		t.Fatalf("pay £20.00 = %d %v, want 201 completed at the partner %q, worth 250 LXC", code, out, partnerName)
	}
	if got, want := postings("ok"), "payment_out_hold agent -2000; payment_out_hold hold 2000; payment_out hold -2000; payment_out partner 2000"; got != want {
		t.Errorf("£20.00 posted %q, want %q", got, want)
	}
	balance(48000)
	if code, again := pay("ok", 2000); code != http.StatusCreated || again["id"] != out["id"] {
		t.Errorf("the same key again = %d %v, want the same payment", code, again)
	}
	if code, _ := pay("ok", 2100); code != http.StatusConflict {
		t.Errorf("the same key for another amount = %d, want 409", code)
	}
	balance(48000)

	if code, out := pay("fail", 2013); code != http.StatusCreated || out["status"] != "failed" {
		t.Fatalf("pay £20.13 = %d %v, want 201 failed", code, out)
	}
	if got, want := postings("fail"), "payment_out_hold agent -2013; payment_out_hold hold 2013; payment_out_failed hold -2013; payment_out_failed agent 2013"; got != want {
		t.Errorf("£20.13 posted %q, want %q", got, want)
	}
	balance(48000)

	code, out = pay("return", 2014)
	if code != http.StatusCreated || out["status"] != "completed" {
		t.Fatalf("pay £20.14 = %d %v, want 201 completed before it comes back", code, out)
	}
	if code, out := call(http.MethodGet, "/v1/money/payments/"+out["id"].(string), ""); code != http.StatusOK || out["status"] != "returned" {
		t.Fatalf("read £20.14 = %d %v, want returned", code, out)
	}
	if got, want := postings("return"), "payment_out_hold agent -2014; payment_out_hold hold 2014; payment_out hold -2014; payment_out partner 2014; "+
		"payment_out_returned partner -2014; payment_out_returned agent 2014"; got != want {
		t.Errorf("£20.14 posted %q, want %q", got, want)
	}
	balance(48000)

	// £100.00 is above the approval amount: an approval is filed, and nothing moves until the owner approves it.
	code, out = pay("big", 10000)
	if msg, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(msg, "approval apr_") {
		t.Fatalf("pay £100.00 = %d %v, want 403 naming the approval", code, out)
	}
	var approval, payeeKind, memo string
	if err := pool.QueryRow(ctx, `SELECT id, payee_kind, memo FROM agent_approvals WHERE agent_id = $1 AND status = 'pending'`, agent.ID).
		Scan(&approval, &payeeKind, &memo); err != nil || payeeKind != "outside" || !strings.Contains(memo, "GBP 100.00 to Ada Lovelace") {
		t.Fatalf("approval %s for payee %q, memo %q (%v), want one pending for the outside payee", approval, payeeKind, memo, err)
	}
	var payments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1 AND idempotency_key = 'big'`, ws).Scan(&payments); err != nil || payments != 0 {
		t.Errorf("the refused payment left %d payment rows (%v), want none", payments, err)
	}
	if got := postings("big"); got != "" {
		t.Errorf("before approval £100.00 posted %q, want nothing", got)
	}
	balance(48000)
	if _, err := store.DecideAgentApproval(ctx, ws, approval, true); err != nil {
		t.Fatal(err)
	}
	if code, out := pay("big", 10000); code != http.StatusCreated || out["status"] != "completed" {
		t.Fatalf("pay £100.00 once approved = %d %v, want 201 completed", code, out)
	}
	if got, want := postings("big"), "payment_out_hold agent -10000; payment_out_hold hold 10000; payment_out hold -10000; payment_out partner 10000"; got != want {
		t.Errorf("approved £100.00 posted %q, want %q", got, want)
	}
	balance(38000)

	if code, out := call(http.MethodGet, "/v1/money/payments", ""); code != http.StatusOK || len(out["payments"].([]any)) != 4 {
		t.Errorf("list = %d %v, want the four payments made", code, out)
	}
}

// B30.22 — an international payment from a GBP account to an EUR IBAN: the quote shows the partner's cost, Talyvor's
// fee for pounds (£5.00), the total that leaves the account, the rate and what arrives; the payment at the quote holds
// the amount with the fee, sends, and posts one entry balanced in both currencies; a payment that is not the quote's
// is refused, so is one to a payee in another currency without a quote, and so is a quote for a payee in the account's
// own currency.
func TestMoneyPaymentRoutes_InternationalQuoteThenPay(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3022-intl"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	acct, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"})
	if err != nil {
		t.Fatal(err)
	}
	local, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	// £500.00 of test money in, from outside, to the company's account.
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -50000}, {AccountID: acct.ID, AmountMinor: 50000}}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// The quote first: £100.00 to Germany costs the partner's £1.00 and Talyvor's £5.00 on top, and €113.63 arrives.
	code, q := call(http.MethodPost, "/v1/money/payments/quote", `{"payee_id":"`+payee.ID+`","currency":"GBP","amount_minor":10000}`)
	want := map[string]any{"partner_cost_minor": float64(100), "fee_minor": float64(500), "total_minor": float64(10600), "arrives_minor": float64(11363),
		"arrives_currency": "EUR", "rate": "1.13636364", "destination_country": "DE", "currency": "GBP", "funding": "test"}
	for k, v := range want {
		if code != http.StatusCreated || q[k] != v {
			t.Fatalf("quote = %d %v, want 201 with %s = %v", code, q, k, v)
		}
	}
	quoteID, _ := q["id"].(string)
	if code, out := call(http.MethodPost, "/v1/money/payments/quote", `{"payee_id":"`+local.ID+`","currency":"GBP","amount_minor":10000}`); code != http.StatusBadRequest {
		t.Errorf("a quote for a payee in pounds = %d %v, want 400", code, out)
	}

	pay := func(key string, minor int) (int, map[string]any) {
		return call(http.MethodPost, "/v1/money/payments", `{"payee_id":"`+payee.ID+`","currency":"GBP","amount_minor":`+jsonOf(minor)+
			`,"quote_id":"`+quoteID+`","reference":"Rechnung 7","idempotency_key":"`+key+`"}`)
	}
	if code, out := pay("other", 10001); code != http.StatusConflict {
		t.Errorf("a payment of another amount at the quote = %d %v, want 409", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/payments", `{"payee_id":"`+payee.ID+`","currency":"GBP","amount_minor":10000,"idempotency_key":"no-quote"}`); code != http.StatusBadRequest {
		t.Errorf("paying a payee in euros without a quote = %d %v, want 400", code, out)
	}
	code, out := pay("intl", 10000)
	if code != http.StatusCreated || out["status"] != "completed" || out["fee_minor"] != float64(500) || out["arrives_minor"] != float64(11363) ||
		out["arrives_currency"] != "EUR" || out["quote_id"] != quoteID {
		t.Fatalf("pay at the quote = %d %v, want 201 completed with the quote's figures", code, out)
	}
	// Every posting of the payment, in order, as "kind purpose currency amount".
	rows, err := pool.Query(ctx, `SELECT e.kind, a.purpose, p.currency, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		JOIN money_accounts a ON a.id = p.account_id WHERE e.workspace_id = $1 AND e.idempotency_key LIKE 'payment_out%:intl' ORDER BY e.created_at, p.line`, ws)
	if err != nil {
		t.Fatal(err)
	}
	var posted []string
	sums := map[string]int64{}
	for rows.Next() {
		var kind, purpose, currency string
		var amount int64
		_ = rows.Scan(&kind, &purpose, &currency, &amount)
		posted = append(posted, kind+" "+purpose+" "+currency+" "+jsonOf(amount))
		if kind == "payment_out" {
			sums[currency] += amount
		}
	}
	rows.Close()
	if got, want := strings.Join(posted, "; "), "payment_out_hold company GBP -10600; payment_out_hold hold GBP 10600; "+
		"payment_out hold GBP -10600; payment_out partner GBP 10100; payment_out revenue GBP 500; payment_out partner EUR -11363; payment_out partner EUR 11363"; got != want {
		t.Errorf("posted %q,\n want %q", got, want)
	}
	if sums["GBP"] != 0 || sums["EUR"] != 0 || len(sums) != 2 {
		t.Errorf("the payment's entry sums to %v per currency, want 0 in GBP and in EUR", sums)
	}
	if b, err := store.Balance(ctx, ws, acct.ID); err != nil || b.TestMinor != 39400 {
		t.Fatalf("the company's account holds %+v (%v), want £394.00 of test money", b, err)
	}
}
