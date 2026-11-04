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
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
)

// B53.15 — with a payday in ten days, three bills and a Netflix subscription, the plan shows what is safe to spend
// each day; a spending spree raises "the rent is short at this pace" once, with the right amount, and its notice; a
// bill larger than its supplier's last is a finding; and a payday that passes with no income raises its own.
func TestB5315_MonthPlan(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5315"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b5315"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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
	pusher := &b537Pusher{}
	b.store.SetApprovalAuth(passkey.RelyingParty{}, pusher)
	b.store.SetNativePush(&b537Phones{}) // the fixture's phone channel holds four pushes; this one keeps every push
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_push_subscriptions (endpoint, workspace_id, p256dh, auth) VALUES ($1, $2, 'k', 'a')`,
		"https://fcm.googleapis.com/b5315", ws); err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).
		Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "b5315-top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -50000}, {AccountID: b.acct.ID, AmountMinor: 50000}}}); err != nil {
		t.Fatal(err)
	}
	// £1,000.00 in the agent's wallet now.
	charge := func(merchant string, minor int64, at time.Time) { // a card at merchant charged minor, dated at
		t.Helper()
		batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: b.acct.AgentID, Currency: "GBP",
			IdempotencyKey: fmt.Sprintf("b5315-%s-%d", merchant, at.Unix()), Cards: []economy.MoneyCard{{Merchant: merchant, CapMinor: minor}}})
		if err != nil {
			t.Fatal(err)
		}
		if batch, err = b.store.ApproveCardBatch(ctx, ws, batch.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		c := batch.Cards[0]
		if ok, reason, err := issuer.Charge(ctx, issuerRef(t, pool, c.ID), c.Merchant, money.Amount{Minor: minor, Asset: "GBP"}); !ok || err != nil {
			t.Fatalf("charge at %s = %v %q %v, want approved", merchant, ok, reason, err)
		}
		tx, err := pool.Begin(ctx) // the audit trail is append-only: date the charge the way B53.5's test does
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, q := range []string{`ALTER TABLE money_entries DISABLE TRIGGER audit_no_mutation`,
			`UPDATE money_entries SET created_at = $1 WHERE workspace_id = $2 AND idempotency_key = 'card_charge:' || $3`,
			`ALTER TABLE money_entries ENABLE TRIGGER audit_no_mutation`} {
			args := []any{}
			if strings.HasPrefix(q, "UPDATE") {
				args = []any{at, ws, c.ID}
			}
			if _, err := tx.Exec(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return today.AddDate(0, 0, n) }
	// Netflix, monthly: £10.99 three times, the last 26 days ago, so its next charge falls in the next nine days.
	for _, ago := range []int{86, 56, 26} {
		charge("Netflix", 1099, now.AddDate(0, 0, -ago))
	}
	// Three bills due before payday — energy in 2 days, the phone in 4, the rent in 9 — and last quarter's energy bill.
	bill := func(id, supplier string, minor int64, due time.Time, status string, created time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO money_bills (id, workspace_id, source, document_sha256, supplier, amount_minor, currency, due_date,
			payee_match, status, created_at) VALUES ($1, $2, 'upload', $1, $3, $4, 'GBP', $5, 'new_payee', $6, $7)`,
			id, ws, supplier, minor, due, status, created); err != nil {
			t.Fatal(err)
		}
	}
	bill("bill_b5315_energy_before", "Energy Co", 5000, day(-88), "paid", now.AddDate(0, 0, -95))
	bill("bill_b5315_energy", "Energy Co", 8000, day(2), "due", now)
	bill("bill_b5315_phone", "Phone Co", 3000, day(4), "due", now)
	bill("bill_b5315_rent", "Landlord", 70000, day(9), "due", now)
	// £50.00 a month for eating out, none of it spent yet.
	if _, err := b.store.SetSpendBudget(ctx, ws, economy.SpendBudget{AgentID: economy.BudgetAllWallets, Category: economy.SpendEatingOut,
		Period: economy.BudgetMonth, AmountMinor: 5000, Currency: "GBP"}, "owner", now); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPlanRoutes(r, b.store)
	call := func(method, path, body string) economy.SpendPlan {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var p economy.SpendPlan
		if err := json.Unmarshal(w.Body.Bytes(), &p); w.Code != http.StatusOK || err != nil {
			t.Fatalf("%s %s = %d %s", method, path, w.Code, w.Body.String())
		}
		return p
	}
	find := func(p economy.SpendPlan, kind string) []economy.PlanFinding {
		var out []economy.PlanFinding
		for _, f := range p.Findings {
			if f.Kind == kind {
				out = append(out, f)
			}
		}
		return out
	}

	// Payday in ten days, £2,500.00 from Acme Ltd.
	payday := day(10)
	p := call(http.MethodPut, "/v1/money/plan/income", fmt.Sprintf(`{"payday": %d, "amount_minor": 250000, "currency": "GBP", "payer": "Acme Ltd"}`,
		payday.Day()))
	// £1,000.00 less three Netflix charges is £967.03; the bills and Netflix £820.99; eating out £50.00 left:
	// £96.04 over the ten days to payday, £9.60 a day.
	if p.NextIncome != payday.Format(time.DateOnly) || p.Days != 10 || p.MoneyNowMinor != 96703 || p.UpcomingMinor != 82099 ||
		p.BudgetsLeftMinor != 5000 || p.AvailableMinor != 9604 || p.SafePerDayMinor != 960 || len(p.Payments) != 4 {
		t.Fatalf("the plan = %+v, want £9.60 a day for 10 days from £967.03, £820.99 due and £50.00 of budget", p)
	}
	for _, pay := range p.Payments {
		if !pay.Covered || pay.Due > day(9).Format(time.DateOnly) {
			t.Fatalf("payment %+v, want covered and due by the rent", pay)
		}
	}
	if got := find(p, economy.PlanBillLarger); len(got) != 1 || got[0].Payment.ID != "bill_b5315_energy" || got[0].UsualMinor != 5000 {
		t.Fatalf("larger bills = %+v, want the energy bill against its £50.00 before", got)
	}
	if len(find(p, economy.PlanPaceShort))+len(find(p, economy.PlanUncovered))+len(find(p, economy.PlanIncomeLate)) != 0 {
		t.Fatalf("findings = %+v, want only the larger bill", p.Findings)
	}
	sweep := func(at time.Time) {
		t.Helper()
		if _, err := b.store.NotifySpending(ctx, ws, nil, "", at); err != nil {
			t.Fatal(err)
		}
	}
	sweep(time.Now())

	// A £70.00 spree today: £10.00 a day over the week. The money now still covers every payment, but at that pace
	// £820.99 due by the rent's day and nine days at £10.00 leave the rent £13.96 short.
	charge("Selfridges", 7000, time.Now())
	p = call(http.MethodGet, "/v1/money/plan", "")
	short := find(p, economy.PlanPaceShort)
	if len(short) != 1 || short[0].Payment.ID != "bill_b5315_rent" || short[0].ShortMinor != 1396 || short[0].PacePerDayMinor != 1000 ||
		short[0].KeepToMinor != 844 || !short[0].Urgent || !strings.Contains(short[0].Sentence, "short by 13.96 GBP") {
		t.Fatalf("pace findings = %+v, want the rent £13.96 short at £10.00 a day", short)
	}
	if len(find(p, economy.PlanUncovered)) != 0 || p.SafePerDayMinor != 260 {
		t.Fatalf("the plan after the spree = %+v, want every payment covered and £2.60 a day", p)
	}
	sweep(time.Now())
	sweep(time.Now()) // the finding is still there: no second notice
	rows, err := pool.Query(ctx, `SELECT subject, status, data FROM spend_notices WHERE workspace_id = $1 AND kind = 'plan' ORDER BY subject`, ws)
	if err != nil {
		t.Fatal(err)
	}
	type notice struct {
		Subject, Status string
		Data            struct{ Finding economy.PlanFinding }
	}
	var notices []notice
	for rows.Next() {
		var n notice
		var data []byte
		if err := rows.Scan(&n.Subject, &n.Status, &data); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(data, &n.Data)
		notices = append(notices, n)
	}
	rows.Close()
	if len(notices) != 2 || !strings.HasPrefix(notices[0].Subject, economy.PlanBillLarger) || !strings.HasPrefix(notices[1].Subject, economy.PlanPaceShort) ||
		notices[1].Status != "sent" || notices[1].Data.Finding.ShortMinor != 1396 {
		t.Fatalf("plan notices = %+v, want the larger bill and the rent £13.96 short, each once, sent", notices)
	}
	plans := 0
	for _, k := range pusher.kinds {
		if k == economy.NoticePlan {
			plans++
		}
	}
	if plans != 2 {
		t.Fatalf("plan pushes = %d (%v), want 2", plans, pusher.kinds)
	}

	// Payday passes with nothing from Acme Ltd: the day after, the income is late, and that is a notice too.
	after := payday.AddDate(0, 0, 1).Add(12 * time.Hour)
	late, err := b.store.SpendPlan(ctx, ws, after)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(late, economy.PlanIncomeLate); len(got) != 1 || got[0].ExpectedOn != payday.Format(time.DateOnly) ||
		!strings.Contains(got[0].Sentence, "2500.00 GBP") {
		t.Fatalf("late findings = %+v, want the income expected on %s", got, payday.Format(time.DateOnly))
	}
	sweep(after)
	var lateNotices int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM spend_notices WHERE workspace_id = $1 AND kind = 'plan' AND subject = $2`,
		ws, economy.PlanIncomeLate+"/"+payday.Format(time.DateOnly)).Scan(&lateNotices); err != nil || lateNotices != 1 {
		t.Fatalf("late-income notices = %d %v, want 1", lateNotices, err)
	}
}
