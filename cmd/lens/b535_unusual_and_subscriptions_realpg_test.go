package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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

// B53.5 — on fake-issuer charges dated through August to October, each unusual-spending rule fires on its planted
// October payment and stays quiet on the ordinary ones; and a monthly Netflix charge is a subscription after its third
// charge, not its second, with the right next date, reminder and costs.
func TestB535_UnusualSpendingAndSubscriptions(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b535"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b535"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).
		Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "b535-top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -100000}, {AccountID: b.acct.ID, AmountMinor: 100000}}}); err != nil {
		t.Fatal(err)
	}
	charge := func(at string, merchant string, minor int64) { // a card at merchant charged minor, dated at (UTC)
		t.Helper()
		when, err := time.Parse("2006-01-02 15:04", at)
		if err != nil {
			t.Fatal(err)
		}
		batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: b.acct.AgentID, Currency: "GBP", IdempotencyKey: "b535-" + at + merchant,
			Cards: []economy.MoneyCard{{Merchant: merchant, CapMinor: minor}}})
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
		tx, err := pool.Begin(ctx) // the audit trail is append-only: date the charge the way B53.4's test does
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `ALTER TABLE money_entries DISABLE TRIGGER audit_no_mutation`); err != nil {
			t.Fatal(err)
		}
		if tag, err := tx.Exec(ctx, `UPDATE money_entries SET created_at = $1 WHERE workspace_id = $2 AND idempotency_key = 'card_charge:' || $3`,
			when, ws, c.ID); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("dating the charge = %v %v", tag, err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE money_entries ENABLE TRIGGER audit_no_mutation`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// The ordinary pattern: twenty grocery shops of £20.00 to £29.50, a Pret lunch, and Netflix.
	for i := range 20 {
		charge(time.Date(2026, 8, 1+i*2, 10, 0, 0, 0, time.UTC).Format("2006-01-02 15:04"), "Tesco", 2000+int64(i)*50)
	}
	charge("2026-08-03 12:00", "Pret A Manger", 850)
	charge("2026-08-05 07:00", "Netflix", 1099)
	charge("2026-09-05 07:00", "Netflix", 1099)

	subs := func(now string) []economy.SpendSubscription {
		t.Helper()
		at, _ := time.Parse(time.DateOnly, now)
		got, err := b.store.SpendSubscriptions(ctx, ws, at)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := subs("2026-09-20"); len(got) != 0 {
		t.Fatalf("subscriptions after two charges = %+v, want none", got)
	}

	// October: ordinary spending, and one planted case for each rule.
	charge("2026-10-02 10:00", "Tesco", 2500)           // ordinary
	charge("2026-10-05 07:00", "Netflix", 1099)         // ordinary, and its third charge
	charge("2026-10-10 10:00", "Tesco", 4000)           // above the usual £29.00
	charge("2026-10-12 19:00", "Marriott Lisbon", 3000) // a new merchant, in a new country
	charge("2026-10-15 12:00", "Pret A Manger", 850)    // ordinary
	charge("2026-10-15 12:03", "Pret A Manger", 850)    // the same again three minutes later
	charge("2026-10-20 21:00", "Bet365", 500)           // gambling, and a new merchant
	if _, err := pool.Exec(ctx, `INSERT INTO agent_spend_alerts (id, workspace_id, agent_id, last_hour_ulxc, usual_per_hour_ulxc, paused, created_at)
		VALUES ('alr_b535', $1, $2, 5000000, 1000000, false, '2026-10-25 09:00Z')`, ws, b.acct.AgentID); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneySpendingRoutes(r, b.store)
	req := httptest.NewRequest(http.MethodGet, "/v1/money/spending/unusual?from=2026-10-01&to=2026-11-01", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
		UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out struct{ Findings []economy.SpendFinding }
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil {
		t.Fatalf("unusual = %d %s", w.Code, w.Body.String())
	}
	var have []string
	by := map[string]economy.SpendFinding{}
	for _, f := range out.Findings {
		have = append(have, f.Rule+" "+f.Merchant)
		by[f.Rule] = f
		if f.AgentID != b.acct.AgentID {
			t.Errorf("finding %+v is not on the agent's wallet", f)
		}
	}
	slices.Sort(have)
	want := []string{"above_usual Tesco", "agent_unusual_spend ", "duplicate Pret A Manger", "gambling Bet365",
		"new_country Marriott Lisbon", "new_merchant Bet365", "new_merchant Marriott Lisbon"}
	if strings.Join(have, ", ") != strings.Join(want, ", ") {
		t.Fatalf("findings = %v, want %v", have, want)
	}
	if f := by[economy.FindingAboveUsual]; f.AmountMinor != 4000 || f.UsualMinor == nil || *f.UsualMinor != 2900 || f.History != 21 || f.Category != economy.SpendGroceries {
		t.Errorf("above usual = %+v, want £40.00 against a 95th percentile of £29.00 over 21 grocery payments", f)
	}
	if f := by[economy.FindingNewCountry]; f.Country != "PT" {
		t.Errorf("new country = %+v, want PT", f)
	}
	if f := by[economy.FindingDuplicate]; f.DuplicateOf == "" || f.DuplicateOf == f.EntryID || f.MinutesApart == nil || *f.MinutesApart != 3 {
		t.Errorf("duplicate = %+v, want the earlier Pret payment, 3 minutes before", f)
	}
	if f := by[economy.FindingAgentUnusualSpend]; f.LastHourULXC != 5000000 || f.UsualPerHourULXC != 1000000 {
		t.Errorf("agent rule = %+v, want its hour and its usual rate", f)
	}
	if out.Findings[0].Rule != economy.FindingAgentUnusualSpend {
		t.Errorf("first finding = %+v, want the newest, the agent rule", out.Findings[0])
	}

	got := subs("2026-10-20")
	if len(got) != 1 {
		t.Fatalf("subscriptions after the third charge = %+v, want Netflix", got)
	}
	s := got[0]
	if s.Merchant != "Netflix" || s.Interval != "monthly" || s.Charges != 3 || s.AmountMinor != 1099 || s.Currency != "GBP" ||
		!s.NextAt.Equal(time.Date(2026, 11, 5, 7, 0, 0, 0, time.UTC)) || !s.RemindOn.Equal(time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)) ||
		s.MonthlyMinor != 1099 || s.YearlyMinor != 12*1099 {
		t.Errorf("subscription = %+v, want Netflix monthly at £10.99, next on 5 November, reminded on 2 November, £131.88 a year", s)
	}
	if got := subs("2026-11-15"); len(got) != 0 {
		t.Errorf("subscriptions ten days after a missed charge = %+v, want none", got)
	}
}
