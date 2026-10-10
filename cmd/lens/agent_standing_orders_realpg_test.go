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
	"github.com/talyvor/lens/internal/tenant"
)

// B30.18 — a weekly standing order of £20.00 test money to a saved outside payee pays four times over four simulated
// weeks, each a payment out on the ledger (hold, then out through the partner); the fifth is refused once the agent's
// monthly limit of 1,000 LXC is reached, and moves nothing; the owner skips the next tick; the order ends at end_at.
func TestAgentRoutes_AWeeklyStandingOrderPaysUntilTheMonthlyLimitThenSkipsAndEnds(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3018-standing"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
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
	// £20.00 is $25.00 at the Test rate, 250 LXC: four a month fit a monthly limit of 1,000 LXC, a fifth does not.
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{MonthlyLimitULXC: 1_000_000_000}); err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -50000}, {AccountID: acct.ID, AmountMinor: 50000}}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	end := t0.AddDate(0, 0, 7*6+1) // after the seventh tick (12 Nov), before the eighth
	code, body := call(http.MethodPost, base+"/"+agent.ID+"/schedules", `{"to_payee_id":"`+payee.ID+`","amount_minor":2000,"currency":"GBP",`+
		`"memo":"Rent","every":"week","first_run_at":"`+t0.Format(time.RFC3339)+`","end_at":"`+end.Format(time.RFC3339)+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create standing order = %d %s", code, body)
	}
	var sc economy.AgentSchedule
	if err := json.Unmarshal([]byte(body), &sc); err != nil || sc.ToPayeeID != payee.ID || sc.AmountMinor != 2000 || sc.Currency != "GBP" || !sc.Active {
		t.Fatalf("created %s (%v), want an active weekly order of GBP 2000 to the payee", body, err)
	}
	balance := func(want int64) {
		t.Helper()
		if b, err := store.Balance(ctx, ws, acct.ID); err != nil || b.TestMinor != want {
			t.Fatalf("the agent's account holds %+v (%v), want %d test pence", b, err, want)
		}
	}
	run := func(at time.Time, wantPaid, wantRefused int) {
		t.Helper()
		res, err := store.RunAgentSchedules(ctx, at)
		if err != nil || res.Paid != wantPaid || res.Refused != wantRefused {
			t.Fatalf("run at %s = %+v (%v), want %d paid and %d refused", at.Format(time.DateOnly), res, err, wantPaid, wantRefused)
		}
	}
	for week := 0; week < 4; week++ {
		run(t0.AddDate(0, 0, 7*week), 1, 0)
	}
	balance(50000 - 4*2000)
	run(t0.AddDate(0, 0, 28), 0, 1) // 29 Oct: the fifth in October, refused by the monthly limit
	run(t0.AddDate(0, 0, 28), 0, 0) // a refused tick is not retried
	balance(50000 - 4*2000)

	// The owner skips 5 November; 12 November is paid (a new month); then the order has ended.
	code, body = call(http.MethodPost, base+"/schedules/"+sc.ID+"/skip", "")
	if err := json.Unmarshal([]byte(body), &sc); code != http.StatusOK || err != nil || !sc.NextRunAt.Equal(t0.AddDate(0, 0, 42)) {
		t.Fatalf("skip = %d %s, want 200 with the next run on 12 November", code, body)
	}
	run(t0.AddDate(0, 0, 42), 1, 0)
	run(t0.AddDate(0, 0, 49), 0, 0)
	balance(50000 - 5*2000)
	if code, body := call(http.MethodGet, base+"/schedules", ""); code != http.StatusOK || !strings.Contains(body, `"active":false`) {
		t.Errorf("schedules = %d %s, want the order ended after end_at", code, body)
	}

	code, body = call(http.MethodGet, base+"/schedules/"+sc.ID+"/runs", "")
	var runs struct {
		Runs []economy.AgentScheduleRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(body), &runs); code != http.StatusOK || err != nil || len(runs.Runs) != 7 {
		t.Fatalf("runs = %d %s, want seven", code, body)
	}
	wantOutcome := map[int]string{0: "paid", 1: "skipped", 2: "refused", 3: "paid", 4: "paid", 5: "paid", 6: "paid"} // newest first
	for i, run := range runs.Runs {
		wantTick := t0.AddDate(0, 0, 7*(6-i))
		if run.Outcome != wantOutcome[i] || !run.TickAt.Equal(wantTick) {
			t.Errorf("run %d = %+v, want the %s tick %s", i, run, wantTick.Format(time.DateOnly), wantOutcome[i])
		}
		switch run.Outcome {
		case "paid": // each paid tick is a completed payment out on the ledger: the hold, then out through the partner
			var postings string
			if err := pool.QueryRow(ctx, `SELECT string_agg(e.kind || ' ' || a.purpose || ' ' || p.amount_minor, '; ' ORDER BY e.created_at, p.line)
				FROM money_payments m JOIN money_entries e ON e.workspace_id = m.workspace_id AND e.idempotency_key LIKE 'payment_out%:' || m.idempotency_key
				JOIN money_postings p ON p.entry_id = e.id JOIN money_accounts a ON a.id = p.account_id
				WHERE m.id = $1 AND m.agent_id = $2 AND m.status = 'completed' AND m.amount_minor = 2000 AND m.currency = 'GBP' AND m.reference = 'Rent'`,
				run.PaymentID, agent.ID).Scan(&postings); err != nil ||
				postings != "payment_out_hold agent -2000; payment_out_hold hold 2000; payment_out hold -2000; payment_out partner 2000" {
				t.Errorf("run %d's payment %q posted %q (%v), want the hold and the payment out", i, run.PaymentID, postings, err)
			}
		case "refused":
			if !strings.Contains(run.Detail, "monthly limit") || run.PaymentID != "" {
				t.Errorf("run %d = %+v, want refused by the monthly limit with no payment", i, run)
			}
		}
	}
	var payments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1`, ws).Scan(&payments); err != nil || payments != 5 {
		t.Errorf("%d payments out in all (%v), want the five paid ticks and nothing for the refused or skipped ones", payments, err)
	}
}
