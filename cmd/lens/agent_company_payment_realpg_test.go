package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/tenant"
)

// B19.15 — an agent pays another company's agent through the marketplace: the payment lands once on the
// paying company's monthly bill, within the paying agent's rules, and the payee's company earns it —
// payable after the holdback once that bill is paid. A payment between two companies that share a card is
// refused as a wash trade.
func TestAgentPayment_AnotherCompanysAgentIsPaidThroughTheMarketplace(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const payerCo, payeeCo, twinCo, secret = "ws-co-payer", "ws-co-payee", "ws-co-twin", "whsec_company_payments"

	billFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetCompanyPayments(store)
	svc := billing.New(pool, bank, billFake, secret).WithMarketBill(billFake, "price_market", "talyvor_marketplace_use", store)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountMarketUseRoutes(r, store, http.NotFoundHandler(), svc, bank)
	r.Post("/v1/billing/webhook", svc.HandleWebhook)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	agent := func(ws, name string) string {
		t.Helper()
		a, err := bank.CreateAgent(ctx, ws, name, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	procurement, translator, twin := agent(payerCo, "procurement"), agent(payeeCo, "translator"), agent(twinCo, "twin")
	if code, body := call(payerCo, http.MethodPut, "/v1/workspaces/"+payerCo+"/agents/"+procurement+"/rules", `{"daily_limit_ulxc":30000000}`); code != http.StatusOK {
		t.Fatalf("rules = %d %s", code, body)
	}
	pay := func(to string, ulxc int64) (int, string) {
		t.Helper()
		return call(payerCo, http.MethodPost, "/v1/workspaces/"+payerCo+"/agents/"+procurement+"/pay",
			fmt.Sprintf(`{"to_agent_id":%q,"amount_ulxc":%d,"memo":"translation of the Q3 report"}`, to, ulxc))
	}
	payments := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT buyer_workspace_id || ' ' || seller_workspace_id || ' ' || payee_agent_id || ' ' || price_ulxc || ' ' || charge || ' ' || listing_id
			FROM market_uses WHERE agent_id = $1 ORDER BY used_at`, procurement)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}

	// Procurement pays the other company's translator 10 LXC ($1.00): through the marketplace, onto its company's bill.
	code, body := pay(translator, 10_000_000)
	var paid economy.AgentPayment
	if _ = json.Unmarshal([]byte(body), &paid); code != http.StatusOK || paid.Via != "marketplace" || paid.ToWorkspaceID != payeeCo {
		t.Fatalf("the payment = %d %s, want it through the marketplace to %s", code, body, payeeCo)
	}
	want := payerCo + " " + payeeCo + " " + translator + " 10000000 billed "
	if got := payments(); len(got) != 1 || got[0] != want {
		t.Fatalf("market_uses = %q, want one billed payment %q", got, want)
	}
	var postings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE account IN ($1, $2)`, "agent:"+procurement, "agent:"+translator).Scan(&postings); err != nil || postings != 0 {
		t.Fatalf("agent_postings = %d (%v), want none: the company's bill carries it, not the agents' balances", postings, err)
	}

	// Within the paying agent's rules: another 25 LXC today would pass its 30 LXC daily limit.
	if code, body := pay(translator, 25_000_000); code != http.StatusForbidden || !strings.Contains(body, "daily") || len(payments()) != 1 {
		t.Fatalf("a payment past the daily limit = %d %s, %d payments; want 403 and nothing recorded", code, body, len(payments()))
	}
	// A company that shares a card with the payer is the same party: refused, nothing recorded.
	for _, ws := range []string{payerCo, twinCo} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'card-twin')`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := pay(twin, 1_000_000); code != http.StatusForbidden || !strings.Contains(body, "share a card") || len(payments()) != 1 {
		t.Fatalf("a wash trade = %d %s, %d payments; want 403 and nothing recorded", code, body, len(payments()))
	}

	// The payment goes once onto the payer's monthly bill, naming the agent paid.
	for range 2 {
		if _, err := store.MeterPending(ctx, svc, 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(billFake.events) != 1 || billFake.events[0].value != 10_000_000 || billFake.events[0].identifier != paid.EntryID || billFake.events[0].customer != "cus_"+payerCo {
		t.Fatalf("meter events = %+v, want the payment once on %s's bill", billFake.events, payerCo)
	}
	code, body = call(payerCo, http.MethodGet, "/v1/workspaces/"+payerCo+"/marketplace/bill", "")
	var bill market.Bill
	if _ = json.Unmarshal([]byte(body), &bill); code != http.StatusOK || bill.TotalULXC != 10_000_000 || len(bill.Lines) != 1 ||
		bill.Lines[0].Title != "Payment to translator" || bill.Lines[0].PayeeAgentID != translator || bill.Lines[0].Memo != "translation of the Q3 report" {
		t.Fatalf("the payer's bill = %d %s", code, body)
	}

	// The payer's company pays that bill today: the payee's company earns the $1.00 less Talyvor's 5% on a payment
	// to another company's agent (LENS_SERVICES_TAKE_BPS, B32.8), payable after the holdback.
	now := time.Now()
	obj := map[string]any{"id": "in_payer_1", "object": "invoice", "subscription": "sub_market_1",
		"status_transitions": map[string]any{"paid_at": now.Unix()},
		"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_payer_1", "object": "line_item",
			"period": map[string]any{"start": now.Add(-time.Hour).Unix(), "end": now.Add(time.Hour).Unix()},
			"price":  map[string]any{"id": "price_market", "object": "price"}}}}}
	raw, _ := json.Marshal(map[string]any{"id": "evt_payer_1", "object": "event", "type": "invoice.paid", "created": now.Unix(), "livemode": true, "data": map[string]any{"object": obj}})
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(raw))
	req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(webhook.ComputeSignature(now, raw, secret))))
	w := httptest.NewRecorder()
	if r.ServeHTTP(w, req); w.Code != http.StatusOK {
		t.Fatalf("invoice.paid = %d %s", w.Code, w.Body.String())
	}
	var gross, share, fee int64
	var payableAt time.Time
	if err := pool.QueryRow(ctx, `SELECT gross_usd_micros, share_usd_micros, fee_usd_micros, payable_at FROM market_earnings WHERE use_id = $1 AND seller_workspace_id = $2`,
		paid.EntryID, payeeCo).Scan(&gross, &share, &fee, &payableAt); err != nil || gross != 1_000_000 || share != 950_000 || fee != 50_000 ||
		payableAt.Before(now.Add(market.Holdback-time.Minute)) {
		t.Fatalf("the payee's earning = gross %d share %d fee %d payable %v (%v), want 1,000,000 → share 950,000 and fee 50,000 µUSD, payable 14 days after the bill was paid",
			gross, share, fee, payableAt, err)
	}
	if e, err := store.SellerEarnings(ctx, payeeCo, now); err != nil || e.InHoldbackUSDMicros != 950_000 || e.AvailableUSDMicros != 0 ||
		len(e.Earnings) != 1 || e.Earnings[0].PayeeAgentID != translator {
		t.Fatalf("today the payee's earnings = %+v (%v), want $0.95 in the holdback, earned by the translator", e, err)
	}
	if e, err := store.SellerEarnings(ctx, payeeCo, now.Add(market.Holdback+time.Hour)); err != nil || e.AvailableUSDMicros != 950_000 {
		t.Fatalf("after the holdback the payee's earnings = %+v (%v), want $0.95 available", e, err)
	}
}
