package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B45.42 — the synthetic key brings a test plan's signed standing order due now: after its first run was approved, the
// next minute tick runs it once, a second payment request paid on its own inside the mandate, one more payment out and
// the account down by its amount; a tick right after runs nothing. A workspace that is not synthetic is refused 403 and
// the order's next tick does not move; an order the workspace does not have is 404.
func TestB4542_ASyntheticWorkspacesStandingOrderBroughtDueRunsOnceOnTheNextTick(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const rpID, origin, syntheticKey = "app.talyvor.com", "https://app.talyvor.com", "b4542-synthetic-key"
	b := b501Payer(t, pool, "ws-b4542")
	ws := b.acct.WorkspaceID
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, false)`, ws); err != nil {
		t.Fatal(err)
	}
	b.store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	// B50.20: a passkey enrols freely only as the workspace's first key, so the payer's stand-in device key goes first.
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_device_keys WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	sign := b5018Passkey(t, b.store, ws, rpID, origin)
	// Saved last week, so the new-payee cooling-off (B41.111) does not hold it.
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '7 days' WHERE id = $1`, b.payee.ID); err != nil {
		t.Fatal(err)
	}

	plan, _, err := b.store.PrepareRecurringPayments(ctx, ws, "ada", "b4542-list", economy.RecurringPlanIn{AgentID: b.acct.AgentID,
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, AskAbove: map[string]int64{"GBP": 50000},
		Until: time.Now().AddDate(1, 0, 0), Payments: []economy.RecurringPaymentIn{
			{PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: 4000, Every: "week", Reference: "Cleaning"}}})
	if err != nil {
		t.Fatal(err)
	}
	order := plan.Schedules[0].ID
	if _, err := b.store.CreatePermissionMandate(ctx, ws, "ada", plan.Mandate.Canonical, sign(plan.Mandate.Challenge)); err != nil {
		t.Fatal(err)
	}
	tick := func() {
		t.Helper()
		if _, err := b.store.RunAgentSchedules(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	runs := func() []economy.AgentScheduleRun {
		t.Helper()
		rs, err := b.store.ListAgentScheduleRuns(ctx, ws, order)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	paymentsOut := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'payment_out'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	nextRun := func() (at time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT next_run_at FROM agent_payment_schedules WHERE id = $1`, order).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	// The first run waits for the owner, who approves it.
	tick()
	first := runs()
	if len(first) != 1 || first[0].Outcome != "awaiting_approval" {
		t.Fatalf("the first run is %+v, want one waiting for approval", first)
	}
	statement, challenge, _, err := b.store.PrepareRequestApproval(ctx, ws, first[0].RequestID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.ApprovePaymentRequest(ctx, ws, first[0].RequestID, "jwt:user:ada", statement, sign(challenge)); err != nil {
		t.Fatal(err)
	}
	if n := paymentsOut(); n != 1 {
		t.Fatalf("%d payments out after the first run was approved, want 1", n)
	}
	b.balance(t, 46000)

	r := chi.NewRouter()
	mountSyntheticDueRoutes(r, syntheticKey, syntheticDeps{audit: pool, due: syntheticDueDeps{db: pool, schedules: b.store}})
	due := func(scheduleID string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/workspaces/"+ws+"/money/schedules/"+scheduleID+"/due", nil)
		req.Header.Set(syntheticKeyHeader, syntheticKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	weekOn := nextRun()
	if code := due(order); code != http.StatusForbidden || !nextRun().Equal(weekOn) {
		t.Fatalf("bringing a real workspace's order due = %d, next run %v, want 403 and still %v", code, nextRun(), weekOn)
	}
	tick()
	if n := len(runs()); n != 1 || paymentsOut() != 1 {
		t.Fatalf("a tick after the refusal ran the order %d times with %d payments out, want 1 and 1", n, paymentsOut())
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET synthetic = true WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if code := due("sch_not_this_workspaces"); code != http.StatusNotFound {
		t.Fatalf("bringing no order of the workspace due = %d, want 404", code)
	}

	if code := due(order); code != http.StatusOK || nextRun().After(time.Now()) {
		t.Fatalf("bringing the test order due = %d, next run %v, want 200 and now", code, nextRun())
	}
	tick()
	tick()
	all := runs()
	if len(all) != 2 || all[0].Outcome != "paid" || all[0].PaymentID == "" || all[0].RequestID == first[0].RequestID {
		t.Fatalf("the order's runs after it was brought due are %+v, want a second run, its own request paid on its own", all)
	}
	if n := paymentsOut(); n != 2 {
		t.Fatalf("%d payments out after the order was brought due and ticked twice, want 2", n)
	}
	b.balance(t, 42000)
	if next := nextRun(); next.Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("the order's next run is %v, want a week on", next)
	}
	b501ZeroSum(t, pool)
}
