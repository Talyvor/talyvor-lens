package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.28 — the overview's totals equal the ledger balances for a seeded workspace: for the company in each currency
// and for each agent. Money moved from the company to its agent is in for the agent and nothing for the company; the
// runway is the balance over the last 30 days' spend; a weekly standing order and a mandate are what is due.
func TestMoneyOverview_TotalsEqualTheLedgerBalances(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3028-overview"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	agent, err := store.CreateAgent(ctx, ws, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	open := func(agentID, cur string) economy.CurrencyAccount {
		a, err := store.OpenCurrencyAccount(ctx, ws, agentID, cur, "test")
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	gbp, eur := open("", "GBP"), open("", "EUR")
	agentGBP := open(agent.ID, "GBP")
	fees, err := store.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: ws, Currency: "GBP", Purpose: economy.MoneyRevenue})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, gbp.ID, 100000, "in-gbp"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, eur.ID, 5000, "in-eur"); err != nil {
		t.Fatal(err)
	}
	move := func(key, from, to string, amount int64) {
		t.Helper()
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityCurrencyAccounts, Kind: "transfer",
			IdempotencyKey: key, Funding: "test", Postings: []economy.MoneyPosting{{AccountID: from, AmountMinor: -amount}, {AccountID: to, AmountMinor: amount}}}); err != nil {
			t.Fatal(err)
		}
	}
	move("to-agent", gbp.ID, agentGBP.ID, 30000)
	move("agent-spends", agentGBP.ID, fees.ID, 6000)

	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_payment_schedules (id, workspace_id, from_agent_id, to_payee_id, amount_ulxc, amount_minor, currency, every, next_run_at)
		VALUES ('sch_b3028', $1, $2, 'payee_b3028', 0, 2000, 'GBP', 'week', $3)`, ws, agent.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, "ws-b3028-payee", "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	mandate, err := store.GrantMandate(ctx, ws, "", economy.MandateGrant{AccountID: agentGBP.ID, PayeeWorkspaceID: "ws-b3028-payee", PayeeName: "Hosting Co",
		MaxPerPullMinor: 1000, MaxPerMonthMinor: 3000, ExpiresAt: now.AddDate(0, 3, 0)})
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	req := httptest.NewRequest(http.MethodGet, "/v1/money/overview", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var o economy.MoneyOverview
	if err := json.Unmarshal(w.Body.Bytes(), &o); w.Code != http.StatusOK || err != nil {
		t.Fatalf("overview = %d %s", w.Code, w.Body.String())
	}

	// The ledger's own sums: every posting to the company's and its agents' accounts.
	ledger := map[string]int64{}
	rows, err := pool.Query(ctx, `SELECT COALESCE(a.agent_id, '') || '/' || a.currency, sum(p.amount_minor)::bigint FROM money_postings p
		JOIN money_accounts a ON a.id = p.account_id WHERE a.workspace_id = $1 AND a.purpose IN ('company', 'agent') GROUP BY a.agent_id, a.currency`, ws)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		ledger[k] += v
		if k[0] != '/' {
			ledger["/"+k[len(k)-3:]] += v
		}
	}
	rows.Close()
	if ledger["/GBP"] != 94000 || ledger["/EUR"] != 5000 || ledger[agent.ID+"/GBP"] != 24000 {
		t.Fatalf("the seeded ledger reads %v; want GBP 94000, EUR 5000, the agent's GBP 24000", ledger)
	}
	days := func(n int64) *int64 { return &n }
	want := map[string]economy.CashTotal{
		"/GBP":            {Currency: "GBP", BalanceMinor: 94000, TestMinor: 94000, InThisMonthMinor: 100000, OutThisMonthMinor: 6000, Out30DaysMinor: 6000, RunwayDays: days(470)},
		"/EUR":            {Currency: "EUR", BalanceMinor: 5000, TestMinor: 5000, InThisMonthMinor: 5000},
		agent.ID + "/GBP": {Currency: "GBP", BalanceMinor: 24000, TestMinor: 24000, InThisMonthMinor: 30000, OutThisMonthMinor: 6000, Out30DaysMinor: 6000, RunwayDays: days(120)},
	}
	got := map[string]economy.CashTotal{}
	for _, c := range o.Totals {
		got["/"+c.Currency] = c
	}
	if len(o.Agents) != 1 || o.Agents[0].AgentID != agent.ID || o.Agents[0].Name != "Buyer" {
		t.Fatalf("agents = %+v; want the one agent, Buyer", o.Agents)
	}
	for _, c := range o.Agents[0].Totals {
		got[agent.ID+"/"+c.Currency] = c
	}
	if len(got) != len(want) {
		t.Fatalf("overview totals = %+v; want %d of them", got, len(want))
	}
	for k, wt := range want {
		g := got[k]
		if g.BalanceMinor != ledger[k] {
			t.Errorf("%s: overview balance %d, ledger %d", k, g.BalanceMinor, ledger[k])
		}
		if (g.RunwayDays == nil) != (wt.RunwayDays == nil) || (g.RunwayDays != nil && *g.RunwayDays != *wt.RunwayDays) || (g.RunwayDays != nil && g.RunsOutOn == nil) {
			t.Errorf("%s: runway %v (%v); want %v", k, g.RunwayDays, g.RunsOutOn, wt.RunwayDays)
		}
		g.RunwayDays, g.RunsOutOn, wt.RunwayDays = nil, nil, nil
		if g != wt {
			t.Errorf("%s = %+v; want %+v", k, g, wt)
		}
	}

	// Due in the next 30 days: five weekly £20 payments, and all the mandate may still pull — £30 a calendar month.
	monthStarts := int64(0)
	for m := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC); m.Before(now.AddDate(0, 0, 30)); m = m.AddDate(0, 1, 0) {
		monthStarts++
	}
	due := map[string]economy.UpcomingPayment{}
	for _, u := range o.Upcoming {
		due[u.ID] = u
	}
	if so := due["sch_b3028"]; len(o.Upcoming) != 2 || so.Kind != "standing_order" || so.Times != 5 || so.TotalMinor != 10000 || so.NextAt == nil {
		t.Errorf("upcoming = %+v; want the standing order 5 times, 10000", o.Upcoming)
	}
	if m := due[mandate.ID]; m.Kind != "mandate" || m.TotalMinor != 3000*(1+monthStarts) || m.Payee != "Hosting Co" {
		t.Errorf("the mandate due = %+v; want %d", m, 3000*(1+monthStarts))
	}
}
