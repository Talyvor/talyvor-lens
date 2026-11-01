package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B53.4 — a weekly eating-out budget crosses 50, 80 and 100% on fake-issuer charges and each alert fires exactly once;
// the week begins again at midnight on Monday in the budget's own time zone, and the next week's spending raises its
// alerts afresh. A card's cap and an agent's daily limit raise theirs at the same points.
func TestB534_BudgetsAlertAt50And80And100OncePerPeriod(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b534"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b534"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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
	agentID := b.acct.AgentID
	charge := func(key string, minor, capMinor int64) string { // one card at Pret, charged minor
		t.Helper()
		batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: agentID, Currency: "GBP", IdempotencyKey: key,
			Cards: []economy.MoneyCard{{Merchant: "Pret A Manger", CapMinor: capMinor}}})
		if err != nil {
			t.Fatal(err)
		}
		if batch, err = b.store.ApproveCardBatch(ctx, ws, batch.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		c := batch.Cards[0]
		if ok, reason, err := issuer.Charge(ctx, issuerRef(t, pool, c.ID), c.Merchant, money.Amount{Minor: minor, Asset: "GBP"}); !ok || err != nil {
			t.Fatalf("charge %s = %v %q %v, want approved", key, ok, reason, err)
		}
		return c.ID
	}
	check := func(now time.Time) []economy.SpendAlert {
		t.Helper()
		got, err := b.store.CheckSpendAlerts(ctx, ws, now)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	wantAlerts := func(got []economy.SpendAlert, want ...string) { // each "kind subject period percent"
		t.Helper()
		var have []string
		for _, a := range got {
			have = append(have, a.Kind+" "+a.Subject+" "+a.Period+" "+strconv.Itoa(a.Percent))
		}
		if strings.Join(have, ", ") != strings.Join(want, ", ") {
			t.Fatalf("alerts raised = %v, want %v", have, want)
		}
	}

	r := chi.NewRouter()
	mountMoneyBudgetRoutes(r, b.store)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	setBudget := func(body string) economy.SpendBudget {
		t.Helper()
		w := call(http.MethodPut, "/v1/money/budgets", body)
		var out economy.SpendBudget
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil {
			t.Fatalf("set budget = %d %s", w.Code, w.Body.String())
		}
		return out
	}
	// £10 a week on eating out for the agent, from Monday in Auckland; and the same for every wallet, from Monday in UTC.
	auckland := setBudget(`{"agent_id": "` + agentID + `", "category": "eating_out", "period": "week", "amount_minor": 1000, "currency": "GBP", "timezone": "Pacific/Auckland"}`)
	utc := setBudget(`{"agent_id": "all", "category": "eating_out", "period": "week", "amount_minor": 1000, "currency": "GBP"}`)
	if w := call(http.MethodPut, "/v1/money/budgets", `{"category": "eating_out", "period": "fortnight", "amount_minor": 1000, "currency": "GBP"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a fortnightly budget = %d %s, want 400", w.Code, w.Body.String())
	}

	now := time.Now()
	charge("w1-a", 550, 5000)
	wantAlerts(check(now), "budget "+auckland.ID+" week 50", "budget "+utc.ID+" week 50")
	wantAlerts(check(now)) // once in the period
	charge("w1-b", 300, 1000)
	wantAlerts(check(now), "budget "+auckland.ID+" week 80", "budget "+utc.ID+" week 80")
	card := charge("w1-c", 200, 250) // 80% of this card's cap
	wantAlerts(check(now), "budget "+auckland.ID+" week 100", "budget "+utc.ID+" week 100", "card "+card+"  50", "card "+card+"  80")
	wantAlerts(check(now))

	// The budget's spending is what the postings sent to the merchant.
	var posted int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		JOIN money_accounts a ON a.id = p.account_id WHERE e.workspace_id = $1 AND e.kind = 'card_charge' AND a.purpose = 'partner'`, ws).Scan(&posted); err != nil {
		t.Fatal(err)
	}
	w := call(http.MethodGet, "/v1/money/budgets", "")
	var list struct{ Budgets []economy.SpendBudget }
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != http.StatusOK || err != nil || len(list.Budgets) != 2 {
		t.Fatalf("budgets = %d %s", w.Code, w.Body.String())
	}
	for _, got := range list.Budgets {
		if posted != 1050 || got.SpentMinor != posted || got.LeftMinor != -50 || got.Percent != 105 {
			t.Errorf("budget %+v, want 1050 spent as the postings sent out (%d), 50 over, 105%%", got, posted)
		}
	}

	// An agent's daily limit: a payment out worth v, under a limit it is 85% of, raises the limit's 50 and 80.
	p := b.pay(t, "inv-1", 400)
	if _, err := b.store.SetAgentRules(ctx, ws, agentID, economy.AgentRules{DailyLimitULXC: p.ValueULXC * 100 / 85}); err != nil {
		t.Fatal(err)
	}
	wantAlerts(check(now), "agent_limit "+agentID+" daily 50", "agent_limit "+agentID+" daily 80")

	// The week begins again at midnight on Monday in Auckland — a Sunday in UTC — with nothing spent in it.
	loc, _ := time.LoadLocation("Pacific/Auckland")
	n := now.In(loc)
	monday := time.Date(n.Year(), n.Month(), n.Day()-(int(n.Weekday())+6)%7+7, 0, 0, 0, 0, loc)
	wantAlerts(check(monday.Add(-time.Minute)))
	before, err := b.store.SpendBudgets(ctx, ws, monday.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	after, err := b.store.SpendBudgets(ctx, ws, monday)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range after {
		switch got.ID {
		case auckland.ID:
			if !got.PeriodStart.Equal(monday) || got.PeriodStart.UTC().Weekday() != time.Sunday || got.SpentMinor != 0 || before[i].SpentMinor != 1050 {
				t.Errorf("Auckland budget a minute before Monday spent %d; at Monday %+v, want 1050 then a week from %v with 0 spent",
					before[i].SpentMinor, got, monday)
			}
		case utc.ID:
			if got.PeriodStart.UTC().Weekday() != time.Monday || got.PeriodStart.UTC().Hour() != 0 || got.PeriodStart.Equal(monday) {
				t.Errorf("UTC budget at Auckland's Monday = %+v, want its own week from Monday 00:00 UTC", got)
			}
		}
	}

	// Next week: last week's charges and alerts are a week old, and this week's first £6 raises the 50% alerts again.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`ALTER TABLE money_entries DISABLE TRIGGER audit_no_mutation`,
		`UPDATE money_entries SET created_at = created_at - interval '7 days' WHERE workspace_id = '` + ws + `'`,
		`ALTER TABLE money_entries ENABLE TRIGGER audit_no_mutation`,
		`UPDATE spend_alerts SET period_start = period_start - interval '7 days', created_at = created_at - interval '7 days' WHERE kind = 'budget'`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantAlerts(check(now))
	charge("w2-a", 600, 5000)
	wantAlerts(check(time.Now()), "budget "+auckland.ID+" week 50", "budget "+utc.ID+" week 50")

	w = call(http.MethodGet, "/v1/money/budgets/alerts", "")
	var alerts struct{ Alerts []economy.SpendAlert }
	if err := json.Unmarshal(w.Body.Bytes(), &alerts); w.Code != http.StatusOK || err != nil {
		t.Fatalf("alerts = %d %s", w.Code, w.Body.String())
	}
	fifty := map[time.Time]bool{}
	for _, a := range alerts.Alerts {
		if a.Subject == auckland.ID && a.Percent == 50 {
			fifty[a.PeriodStart.UTC()] = true
		}
	}
	if len(alerts.Alerts) != 12 || len(fifty) != 2 {
		t.Errorf("alerts = %+v, want 12, the Auckland budget's 50%% once in each of two weeks", alerts.Alerts)
	}
}
