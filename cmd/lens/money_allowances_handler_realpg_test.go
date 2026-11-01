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

// B30.27 — the owner sets an agent a £200.00 monthly allowance with sweep-back. Month one pays £200.00 from the
// company's GBP account into the agent's; the agent spends £50.00; month two sweeps the £150.00 left back and pays
// £200.00 again. Each is read off the postings on both accounts, and a month ticked twice pays once.
func TestMoneyAllowances_TwoMonthsFundAndSweepBackOnBothAccounts(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3027-allowances"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screener)
	agent, err := store.CreateAgent(ctx, ws, "Researcher", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentAcct, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, company.ID, 100000, "seed"); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyAllowanceRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		_ = json.Unmarshal(w.Body.Bytes(), out)
		return w.Code
	}
	var a economy.MoneyAllowance
	if code := call(http.MethodPut, "/v1/money/allowances", `{"agent_id":"`+agent.ID+`","currency":"GBP","amount_minor":20000,"day_of_month":1,"sweep_back":true}`, &a); code != http.StatusOK {
		t.Fatalf("set allowance: %d", code)
	}
	month1, month2 := a.NextRunAt, a.NextRunAt.AddDate(0, 1, 0)

	if funded, _, err := store.RunMoneyAllowances(ctx, month1); err != nil || funded != 1 {
		t.Fatalf("month one: funded %d, %v", funded, err)
	}
	if funded, refused, err := store.RunMoneyAllowances(ctx, month1); err != nil || funded+refused != 0 {
		t.Fatalf("month one again: funded %d refused %d, %v; want nothing", funded, refused, err)
	}
	var partner string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsOut, Kind: "test_spend",
		IdempotencyKey: "spend", Funding: economy.FundingTest, Counterparty: "Test shop", Postings: []economy.MoneyPosting{
			{AccountID: agentAcct.ID, AmountMinor: -5000}, {AccountID: partner, AmountMinor: 5000}}}); err != nil {
		t.Fatal(err)
	}
	if funded, _, err := store.RunMoneyAllowances(ctx, month2); err != nil || funded != 1 {
		t.Fatalf("month two: funded %d, %v", funded, err)
	}

	// lines is account's allowance postings, in order: "kind amount". A month's sweep and payment share one transaction, so one
	// created_at; the sweep (allowance_sweep) goes first.
	lines := func(account string) string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT e.kind, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE p.account_id = $1 AND e.kind LIKE 'allowance%' ORDER BY e.created_at, e.kind DESC`, account)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var kind string
			var amount int64
			_ = rows.Scan(&kind, &amount)
			out = append(out, kind+" "+jsonOf(amount))
		}
		return strings.Join(out, ", ")
	}
	if got, want := lines(company.ID), "allowance -20000, allowance_sweep 15000, allowance -20000"; got != want {
		t.Errorf("company account: %s; want %s", got, want)
	}
	if got, want := lines(agentAcct.ID), "allowance 20000, allowance_sweep -15000, allowance 20000"; got != want {
		t.Errorf("agent account: %s; want %s", got, want)
	}
	for account, want := range map[string]int64{company.ID: 75000, agentAcct.ID: 20000} {
		if b, err := store.Balance(ctx, ws, account); err != nil || b.TestMinor != want {
			t.Errorf("%s holds %d test pence (%v); want %d", account, b.TestMinor, err, want)
		}
	}

	var runs struct{ Runs []economy.MoneyAllowanceRun }
	if code := call(http.MethodGet, "/v1/money/allowances/"+a.ID+"/runs", "", &runs); code != http.StatusOK || len(runs.Runs) != 2 {
		t.Fatalf("runs: %d, %+v", code, runs.Runs)
	}
	if latest := runs.Runs[0]; latest.Status != "funded" || latest.SweptMinor != 15000 || latest.FundedMinor != 20000 || latest.SweepEntryID == "" {
		t.Errorf("month two's run: %+v", latest)
	}
}
