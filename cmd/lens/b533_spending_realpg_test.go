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

// B53.3 — fake-issuer charges with known category codes land in their categories; one correction moves that
// merchant's past payment and its next one; and the totals by category and by tag add up to the postings.
func TestB533_EveryPaymentGetsACategoryAndOneCorrectionTeachesIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b533"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b533"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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
	pay := func(key string, charges map[string]int64) {
		t.Helper()
		var cards []economy.MoneyCard
		for merchant := range charges {
			cards = append(cards, economy.MoneyCard{Merchant: merchant, CapMinor: 5000})
		}
		batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: b.acct.AgentID, Currency: "GBP", IdempotencyKey: key, Cards: cards})
		if err != nil {
			t.Fatal(err)
		}
		if batch, err = b.store.ApproveCardBatch(ctx, ws, batch.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		for _, c := range batch.Cards {
			if ok, reason, err := issuer.Charge(ctx, issuerRef(t, pool, c.ID), c.Merchant, money.Amount{Minor: charges[c.Merchant], Asset: "GBP"}); !ok || err != nil {
				t.Fatalf("charge at %s = %v %q %v, want approved", c.Merchant, ok, reason, err)
			}
		}
	}
	pay("week-1", map[string]int64{"Tesco": 3120, "Pret A Manger": 850, "OpenAI": 2000})

	r := chi.NewRouter()
	mountMoneySpendingRoutes(r, b.store)
	call := func(method, path, body, workspace string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: workspace, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	spending := func() (economy.Spending, map[string]economy.SpendPayment) {
		t.Helper()
		w := call(http.MethodGet, "/v1/money/spending?from=2000-01-01&to=2100-01-01", "", ws)
		var out economy.Spending
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil {
			t.Fatalf("spending = %d %s", w.Code, w.Body.String())
		}
		last := map[string]economy.SpendPayment{} // each merchant's newest payment
		for i := len(out.Payments) - 1; i >= 0; i-- {
			last[out.Payments[i].Merchant] = out.Payments[i]
		}
		return out, last
	}
	_, got := spending()
	for merchant, want := range map[string]string{"Tesco": economy.SpendGroceries, "Pret A Manger": economy.SpendEatingOut, "OpenAI": economy.SpendAISoftware} {
		if p := got[merchant]; p.Category != want || p.CategorySource != economy.SpendFromCard || p.AgentID != b.acct.AgentID {
			t.Errorf("%s landed as %+v, want %s from its card, on the agent's wallet", merchant, p, want)
		}
	}

	// One correction: Pret is groceries from now on — its past payment moves, and so does its next one.
	if w := call(http.MethodPut, "/v1/money/spending/merchants", `{"merchant": " pret a manger ", "category": "groceries"}`, ws); w.Code != http.StatusOK {
		t.Fatalf("correction = %d %s", w.Code, w.Body.String())
	}
	pastPret := got["Pret A Manger"].EntryID
	pay("week-2", map[string]int64{"Pret A Manger": 640})
	out, got := spending()
	if len(out.Payments) != 4 {
		t.Fatalf("payments = %+v, want 4", out.Payments)
	}
	for _, p := range out.Payments {
		if p.Merchant == "Pret A Manger" && (p.Category != economy.SpendGroceries || p.CategorySource != economy.SpendFromCorrection) {
			t.Errorf("Pret payment %s after the correction = %+v, want groceries from the correction", p.EntryID, p)
		}
	}
	if got["Pret A Manger"].EntryID == pastPret {
		t.Fatal("the second Pret payment is missing")
	}

	// Tags: Tesco and OpenAI are "office"; OpenAI is also "research". Another workspace cannot tag this one's money.
	for entry, tags := range map[string]string{got["Tesco"].EntryID: `["Office"]`, got["OpenAI"].EntryID: `["office", "research", "office"]`} {
		if w := call(http.MethodPut, "/v1/money/entries/"+entry+"/tags", `{"tags": `+tags+`}`, ws); w.Code != http.StatusOK {
			t.Fatalf("tag = %d %s", w.Code, w.Body.String())
		}
	}
	if w := call(http.MethodPut, "/v1/money/entries/"+got["Tesco"].EntryID+"/tags", `{"tags": ["mine"]}`, "ws-b533-other"); w.Code != http.StatusNotFound {
		t.Fatalf("another workspace tagging = %d %s, want 404", w.Code, w.Body.String())
	}
	out, _ = spending()
	if len(out.Totals) != 1 {
		t.Fatalf("totals = %+v, want the one agent's GBP", out.Totals)
	}
	tot := out.Totals[0]
	var postedOut, postedOffice, postedResearch int64 // what the ledger says went to the merchants
	if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(p.amount_minor), 0)::bigint,
			COALESCE(SUM(p.amount_minor) FILTER (WHERE EXISTS (SELECT 1 FROM money_entry_tags t WHERE t.entry_id = e.id AND t.tag = 'office')), 0)::bigint,
			COALESCE(SUM(p.amount_minor) FILTER (WHERE EXISTS (SELECT 1 FROM money_entry_tags t WHERE t.entry_id = e.id AND t.tag = 'research')), 0)::bigint
		FROM money_entries e JOIN money_postings p ON p.entry_id = e.id JOIN money_accounts a ON a.id = p.account_id
		WHERE e.workspace_id = $1 AND e.kind = 'card_charge' AND a.purpose = 'partner'`, ws).Scan(&postedOut, &postedOffice, &postedResearch); err != nil {
		t.Fatal(err)
	}
	var byCategory int64
	for _, v := range tot.ByCategory {
		byCategory += v
	}
	if postedOut != 3120+850+2000+640 || tot.TotalMinor != postedOut || byCategory != postedOut {
		t.Errorf("total %d, by category %d, want both the %d the postings sent out", tot.TotalMinor, byCategory, postedOut)
	}
	if want := map[string]int64{economy.SpendGroceries: 3120 + 850 + 640, economy.SpendAISoftware: 2000}; len(tot.ByCategory) != 2 ||
		tot.ByCategory[economy.SpendGroceries] != want[economy.SpendGroceries] || tot.ByCategory[economy.SpendAISoftware] != want[economy.SpendAISoftware] {
		t.Errorf("by category = %v, want %v", tot.ByCategory, want)
	}
	if tot.ByTag["office"] != postedOffice || postedOffice != 3120+2000 || tot.ByTag["research"] != postedResearch || postedResearch != 2000 || len(tot.ByTag) != 2 {
		t.Errorf("by tag = %v, want office %d and research %d as posted", tot.ByTag, postedOffice, postedResearch)
	}
}
