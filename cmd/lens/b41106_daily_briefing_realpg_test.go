package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B41.106 — the route B41.6's Home card reads: after a day with two payments, one held approval and one bill due, the
// next morning's briefing names all four with their amounts, the approval first; and a quiet day says so in one line.
func TestDailyBriefing_NamesYesterdaysPaymentsTheApprovalAndTheBillDue(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	brief := func(ws string, at time.Time) economy.Briefing {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/briefing?at="+at.Format(time.RFC3339Nano), nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("briefing = %d %s", w.Code, w.Body.String())
		}
		var b economy.Briefing
		if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		return b
	}

	const ws = "ws-briefing"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, ws); err != nil {
		t.Fatal(err)
	}
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
	if _, err := store.PayAgent(ctx, ws, buyer.ID, writer.ID, 3_000_000, "a logo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, buyer.ID, writer.ID, 4_500_000, "the copy"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestPaymentApproval(ctx, ws, buyer.ID, writer.ID, 30_000_000, "", "a large order"); err != nil {
		t.Fatal(err)
	}
	// The morning after: today's movements are yesterday's. Acme's bill is due this week; Globex's is not.
	morning := time.Now().UTC().Add(24 * time.Hour)
	for supplier, days := range map[string]int{"Acme": 2, "Globex": 10} {
		due := morning.AddDate(0, 0, days).Format("2006-01-02")
		if _, err := pool.Exec(ctx, `INSERT INTO money_bills (id, workspace_id, source, document_sha256, supplier, amount_minor, currency,
			due_date, payee_match) VALUES ($1, $2, 'upload', $1, $3, 3000, 'GBP', $4, 'new_payee')`, "bill_"+supplier, ws, supplier, due); err != nil {
			t.Fatal(err)
		}
	}

	b := brief(ws, morning)
	var got []string
	for _, it := range b.Items {
		got = append(got, fmt.Sprintf("%s %s %d %s", it.Kind, it.Name, it.Amount, it.Currency))
	}
	want := []string{
		"approval writer 30000000 LXC",
		"bill Acme 3000 GBP",
		"payment writer 3000000 LXC",
		"payment writer 4500000 LXC",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the briefing lists\n  %s\nwant the approval first, then the bill due this week, then yesterday's two payments:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i, text := range []string{"30 LXC waits for your approval, to writer", "A bill from Acme for 30.00 GBP is due on",
		"3 LXC was paid to writer for a logo", "4.5 LXC was paid to writer for the copy"} {
		if !strings.HasPrefix(b.Items[i].Text, text) {
			t.Errorf("item %d reads %q, want it to start %q", i, b.Items[i].Text, text)
		}
	}
	if len(b.SpentYesterday) != 1 || b.SpentYesterday[0] != (economy.BriefingTotal{Currency: "LXC", Amount: 7_500_000}) {
		t.Errorf("spent yesterday = %+v, want 7.5 LXC", b.SpentYesterday)
	}
	if b.Quiet || b.Line != "1 approval waiting, 1 bill due this week, 2 payments yesterday, spent yesterday 7.5 LXC." {
		t.Errorf("quiet = %v, line = %q", b.Quiet, b.Line)
	}

	// A workspace whose agents did nothing, with nothing waiting, has a quiet day, in one line.
	const calm = "ws-briefing-calm"
	if _, err := store.CreateAgent(ctx, calm, "idle", "user-owner"); err != nil {
		t.Fatal(err)
	}
	if q := brief(calm, morning); !q.Quiet || len(q.Items) != 0 || q.Line != "A quiet day: nothing was spent yesterday, and nothing is waiting for you." {
		t.Errorf("a quiet day's briefing = %+v", q)
	}
}
