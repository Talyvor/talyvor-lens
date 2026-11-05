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
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B53.17 — a charge on an agent's reusable card at a 5411 merchant shows in GET /v1/money/spending under groceries,
// on the agent's wallet, in its total and by category; a correction for that merchant moves it.
func TestB5317_ReusableCardChargesShowInSpending(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5317"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b5317"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			issuer = issuerfake.New(store.AuthorizeCardCharge)
			if err := layer.Register(issuer.Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})

	r := chi.NewRouter()
	mountReusableCardRoutes(r, b.store, true)
	mountMoneySpendingRoutes(r, b.store)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w := call(http.MethodPost, "/v1/money/reusable-cards", `{"agent_id":"`+b.acct.AgentID+`","currency":"GBP","per_payment_limit_minor":5000,
		"daily_limit_minor":8000,"idempotency_key":"agent-card"}`)
	var card economy.ReusableCard
	if err := json.Unmarshal(w.Body.Bytes(), &card); w.Code != http.StatusCreated || err != nil || card.Status != economy.ReusableCardActive {
		t.Fatalf("issue the agent's card = %d %s", w.Code, w.Body.String())
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_reusable_cards WHERE id = $1`, card.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ok, reason, err := issuer.Charge(ctx, ref, "Tesco", money.Amount{Minor: 3120, Asset: "GBP"}); !ok || err != nil {
		t.Fatalf("charge at Tesco = %v %q %v, want approved", ok, reason, err)
	}

	spending := func() (economy.SpendPayment, economy.SpendTotal) {
		t.Helper()
		w := call(http.MethodGet, "/v1/money/spending?from=2000-01-01&to=2100-01-01", "")
		var out economy.Spending
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil || len(out.Payments) != 1 || len(out.Totals) != 1 {
			t.Fatalf("spending = %d %s, want the card's one charge", w.Code, w.Body.String())
		}
		return out.Payments[0], out.Totals[0]
	}
	p, tot := spending()
	if p.Kind != "card_charge" || p.Merchant != "Tesco" || p.MCC != "5411" || p.AgentID != b.acct.AgentID || p.AmountMinor != 3120 ||
		p.Category != economy.SpendGroceries || p.CategorySource != economy.SpendFromCard {
		t.Fatalf("the charge = %+v, want Tesco's 3120 under groceries from its card, on the agent's wallet", p)
	}
	if tot.AgentID != b.acct.AgentID || tot.Currency != "GBP" || tot.TotalMinor != 3120 || tot.ByCategory[economy.SpendGroceries] != 3120 {
		t.Fatalf("the agent's total = %+v, want GBP 31.20 all groceries", tot)
	}

	if w := call(http.MethodPut, "/v1/money/spending/merchants", `{"merchant": "tesco", "category": "eating_out"}`); w.Code != http.StatusOK {
		t.Fatalf("correction = %d %s", w.Code, w.Body.String())
	}
	if p, tot = spending(); p.Category != economy.SpendEatingOut || p.CategorySource != economy.SpendFromCorrection ||
		tot.ByCategory[economy.SpendEatingOut] != 3120 || tot.ByCategory[economy.SpendGroceries] != 0 {
		t.Fatalf("after the correction = %+v %+v, want it moved to eating out", p, tot)
	}
}
