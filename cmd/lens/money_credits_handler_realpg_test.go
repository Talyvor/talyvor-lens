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
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.25 — a £50.00 test purchase from the company's GBP account posts one money entry to Talyvor's revenue account
// and one lxc_ledger purchase row for the credits £50.00 buys at list, and the two reconcile. The same key again buys
// nothing twice; a purchase the account cannot cover, and a live one before currency_accounts is cleared, move nothing.
func TestMoneyCreditRoutes_ATestPurchasePostsOneEntryAndOneMatchingCreditRow(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3025-credits"
	reg := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	gbp, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, gbp.ID, 8000, "fund-b3025"); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	buy := func(body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/money/credits", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	fx, _ := reg.FX(ctx, economy.CapabilityCurrencyAccounts)
	q, err := fx.Quote(ctx, partners.FXQuoteRequest{ID: "expect", Sell: partners.Money{Minor: 5000, Currency: "GBP"}, Buy: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	want := q.Buy.Minor * 100_000 // µLXC: $0.10 a credit

	code, out := buy(`{"amount_minor":5000,"currency":"GBP","idempotency_key":"b3025-1"}`)
	if code != http.StatusCreated || int64(out["credits_ulxc"].(float64)) != want {
		t.Fatalf("buy £50.00 = %d %v, want 201 and %d µLXC", code, out, want)
	}
	entry := out["entry_id"].(string)
	if code, again := buy(`{"amount_minor":5000,"currency":"GBP","idempotency_key":"b3025-1"}`); code != http.StatusCreated || again["entry_id"] != entry {
		t.Fatalf("the same purchase again = %d %v, want 201 answering entry %s", code, again, entry)
	}

	// One money entry: £50.00 from the company's account to Talyvor's revenue account, test money.
	if n := count(`SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'credit_purchase'`, ws); n != 1 {
		t.Fatalf("%d credit_purchase entries, want 1", n)
	}
	if n := count(`SELECT count(*) FROM money_postings p JOIN money_accounts a ON a.id = p.account_id WHERE p.entry_id = $1 AND p.funding = 'test'
		AND ((a.id = $2 AND p.amount_minor = -5000) OR (a.purpose = 'revenue' AND p.amount_minor = 5000))`, entry, gbp.ID); n != 2 {
		t.Fatalf("entry %s has %d of its two postings (company −£50.00, revenue +£50.00), want 2", entry, n)
	}
	// One lxc_ledger purchase row naming that entry, for the matching credits, test-funded.
	var amount int64
	var funding string
	if err := pool.QueryRow(ctx, `SELECT sum(amount)::bigint, min(metadata->>'funding') FROM lxc_ledger WHERE workspace_id = $1 AND type = 'purchase'
		AND metadata->>'money_entry_id' = $2`, ws, entry).Scan(&amount, &funding); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1`, ws); n != 1 || amount != want || funding != "test" {
		t.Fatalf("lxc_ledger: %d rows, %d µLXC naming the entry, funding %q — want 1 row of %d, test", n, amount, funding, want)
	}
	if b, err := store.Balance(ctx, ws, gbp.ID); err != nil || b.AmountMinor != 3000 {
		t.Fatalf("the GBP account holds %+v (%v), want £30.00", b, err)
	}

	// £40.00 is more than the £30.00 left; live money waits for currency_accounts' clearance. Neither moves anything.
	if code, out := buy(`{"amount_minor":4000,"currency":"GBP","idempotency_key":"b3025-2"}`); code != http.StatusConflict {
		t.Errorf("buy £40.00 from £30.00 = %d %v, want 409", code, out)
	}
	code, out = buy(`{"amount_minor":1000,"currency":"GBP","funding":"live","idempotency_key":"b3025-3"}`)
	if msg, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(msg, "class RED") {
		t.Errorf("a live purchase = %d %v, want 403 naming class RED", code, out)
	}
	if n := count(`SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'credit_purchase'`, ws) +
		count(`SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1`, ws); n != 2 {
		t.Errorf("the refused purchases left %d entries and credit rows, want the first purchase's 2", n)
	}
}
