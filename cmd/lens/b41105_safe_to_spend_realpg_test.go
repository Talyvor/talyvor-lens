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
	"github.com/talyvor/lens/internal/tenant"
)

// B41.105 — the route B41.5's Home and MCP tool read: an agent with 100 LXC, a 20 LXC payment scheduled this month and
// a 30 LXC payment waiting for approval has 50 LXC safe to spend; the company's £30 bill due this month comes off the
// company's pounds, and a schedule next month comes off nothing yet.
func TestSafeToSpend_AnAgentsBalanceLessWhatIsPromisedThisMonth(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-safe"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	buyer, err := store.CreateAgent(ctx, ws, "buyer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.CreateAgent(ctx, ws, "writer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, buyer.ID, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer.ID, economy.AgentRules{ApprovalAboveULXC: 10_000_000}); err != nil {
		t.Fatal(err)
	}
	april20 := time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)
	if _, err := store.CreateAgentSchedule(ctx, ws, buyer.ID, writer.ID, 20_000_000, "rent", "month", april20); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAgentSchedule(ctx, ws, buyer.ID, writer.ID, 5_000_000, "", "month", april20.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestPaymentApproval(ctx, ws, buyer.ID, writer.ID, 30_000_000, "", "a large order"); err != nil {
		t.Fatal(err)
	}
	for _, due := range []string{"2026-04-25", "2026-05-02"} { // only April's is due this month
		if _, err := pool.Exec(ctx, `INSERT INTO money_bills (id, workspace_id, source, document_sha256, supplier, amount_minor, currency,
			due_date, payee_match) VALUES ($1, $2, 'upload', $1, 'Acme', 3000, 'GBP', $3, 'new_payee')`, "bill_"+due, ws, due); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/safe-to-spend?at=2026-04-11T00:00:00Z", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner"}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("safe-to-spend = %d %s", w.Code, w.Body.String())
	}
	var got economy.SafeToSpend
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	figure := func(figures []economy.SafeToSpendFigure, currency string) economy.SafeToSpendFigure {
		for _, f := range figures {
			if f.Currency == currency {
				return f
			}
		}
		t.Fatalf("no %s figure in %+v", currency, figures)
		return economy.SafeToSpendFigure{}
	}
	var agent []economy.SafeToSpendFigure
	for _, a := range got.Agents {
		if a.AgentID == buyer.ID {
			agent = a.Figures
		}
	}
	if f := figure(agent, "LXC"); f != (economy.SafeToSpendFigure{Currency: "LXC", Balance: 100_000_000, Scheduled: 20_000_000,
		PendingApprovals: 30_000_000, SafeToSpend: 50_000_000}) {
		t.Errorf("the buyer's LXC = %+v, want 100 − 20 scheduled − 30 waiting = 50 LXC safe to spend", f)
	}
	// The company keeps the 20 LXC its buyer pays its writer; the payment waiting for approval may still leave it.
	if f := figure(got.Company, "LXC"); f.Balance != 200_000_000 || f.Scheduled != 0 || f.SafeToSpend != 170_000_000 {
		t.Errorf("the company's LXC = %+v, want 200 − 30 waiting = 170 LXC", f)
	}
	if f := figure(got.Company, "GBP"); f.BillsDue != 3000 || f.SafeToSpend != -3000 {
		t.Errorf("the company's GBP = %+v, want April's £30 bill due and −£30 safe to spend", f)
	}
}
