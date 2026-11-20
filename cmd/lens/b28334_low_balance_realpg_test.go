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
)

// B28.334 — an owner sets a £20.00 threshold on an agent's GBP balance. Spending it down to £15.00 raises one alert
// row for the hour however often it is checked — by the sweep or by every read of the alerts — another the next hour
// while it is still below, and none once it is topped back up. The alert is queued once to the owner's push channel.
// Another workspace can neither set a threshold on the agent nor see its alerts.
func TestB28334_LowBalanceAlertsOncePerHour(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other = "ws-b28334", "ws-b28334-other"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	agent, err := store.CreateAgent(ctx, ws, "Researcher", "owner")
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
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).
		Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	move := func(key string, minor int64) { // minor into the agent's account, or out of it when below zero
		t.Helper()
		c := economy.CapabilityPaymentsIn
		if minor < 0 {
			c = economy.CapabilityPaymentsOut
		}
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: c, Kind: "test_" + key, IdempotencyKey: key,
			Funding: economy.FundingTest, Counterparty: "Test", Postings: []economy.MoneyPosting{
				{AccountID: partnerAccount, AmountMinor: -minor}, {AccountID: acct.ID, AmountMinor: minor}}}); err != nil {
			t.Fatal(err)
		}
	}
	move("top_up", 5000)
	if _, err := store.AddAlertChannel(ctx, ws, economy.AlertChannel{Kind: economy.AlertChannelPush}, "owner"); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyLowBalanceRoutes(r, store)
	call := func(asWS, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: asWS, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM low_balance_alerts WHERE workspace_id = $1 AND agent_id = $2`, ws, agent.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	body := `{"agent_id": "` + agent.ID + `", "currency": "GBP", "threshold_minor": 2000}`
	if w := call(other, http.MethodPut, "/v1/money/low-balance", body); w.Code != http.StatusBadRequest {
		t.Fatalf("another workspace setting a threshold on the agent = %d %s, want 400", w.Code, w.Body.String())
	}
	w := call(ws, http.MethodPut, "/v1/money/low-balance", body)
	var set economy.BalanceThreshold
	if err := json.Unmarshal(w.Body.Bytes(), &set); w.Code != http.StatusOK || err != nil {
		t.Fatalf("set threshold = %d %s", w.Code, w.Body.String())
	}
	if set.BalanceMinor != 5000 || set.ThresholdMinor != 2000 || set.Below || !set.HasAccount {
		t.Fatalf("threshold = %+v, want £50.00 held, £20.00 threshold, not below", set)
	}

	now := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	if got, err := store.CheckLowBalanceAlerts(ctx, ws, now); err != nil || len(got) != 0 {
		t.Fatalf("above the threshold: alerts = %+v %v, want none", got, err)
	}

	move("spend", -3500) // £15.00 left
	for i := 0; i < 5; i++ {
		if _, err := store.CheckLowBalanceAlerts(ctx, ws, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ { // every read of the alerts checks first; the GET's own clock is today, so pin the count below
		if w := call(ws, http.MethodGet, "/v1/money/low-balance/alerts", ""); w.Code != http.StatusOK {
			t.Fatalf("alerts = %d %s", w.Code, w.Body.String())
		}
	}
	var hourRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM low_balance_alerts WHERE workspace_id = $1 AND hour_start = $2`, ws, now.Truncate(time.Hour)).
		Scan(&hourRows); err != nil {
		t.Fatal(err)
	}
	if hourRows != 1 {
		t.Fatalf("five checks in 09:00's hour wrote %d alert rows for it, want 1", hourRows)
	}
	var a economy.LowBalanceAlert
	if err := pool.QueryRow(ctx, `SELECT balance_minor, threshold_minor, currency FROM low_balance_alerts WHERE workspace_id = $1 AND hour_start = $2`,
		ws, now.Truncate(time.Hour)).Scan(&a.BalanceMinor, &a.ThresholdMinor, &a.Currency); err != nil {
		t.Fatal(err)
	}
	if a.BalanceMinor != 1500 || a.ThresholdMinor != 2000 || a.Currency != "GBP" {
		t.Fatalf("alert = %+v, want £15.00 held below £20.00", a)
	}
	before := rows()

	got, err := store.CheckLowBalanceAlerts(ctx, ws, now.Add(time.Hour)) // still below at 10:30
	if err != nil || len(got) != 1 || rows() != before+1 {
		t.Fatalf("the next hour: alerts = %+v %v, rows %d, want one more than %d", got, err, rows(), before)
	}

	move("top_up_again", 1000) // £25.00
	if got, err := store.CheckLowBalanceAlerts(ctx, ws, now.Add(2*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("topped back up: alerts = %+v %v, want none", got, err)
	}

	w = call(ws, http.MethodGet, "/v1/money/low-balance/alerts", "")
	var listed struct{ Alerts []economy.LowBalanceAlert }
	if err := json.Unmarshal(w.Body.Bytes(), &listed); w.Code != http.StatusOK || err != nil || len(listed.Alerts) != rows() {
		t.Fatalf("listed alerts = %d %s, want the %d rows", w.Code, w.Body.String(), rows())
	}
	w = call(other, http.MethodGet, "/v1/money/low-balance/alerts", "")
	if err := json.Unmarshal(w.Body.Bytes(), &listed); w.Code != http.StatusOK || err != nil || len(listed.Alerts) != 0 {
		t.Fatalf("another workspace's alerts = %d %s, want none", w.Code, w.Body.String())
	}

	// Each alert is queued once to the owner's push channel, in words.
	if _, err := store.QueueAlertDeliveries(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueAlertDeliveries(ctx, ws); err != nil {
		t.Fatal(err)
	}
	var queued int
	var title, words string
	if err := pool.QueryRow(ctx, `SELECT count(*), min(title), min(body) FROM alert_deliveries WHERE workspace_id = $1 AND alert_kind = 'low_balance'`, ws).
		Scan(&queued, &title, &words); err != nil {
		t.Fatal(err)
	}
	if queued != rows() || title != "Low balance" || words != "Researcher has £15.00, below the £20.00 you set." {
		t.Fatalf("deliveries = %d %q %q, want %d of Researcher at £15.00 below £20.00", queued, title, words, rows())
	}

	// The threshold goes; its alerts stay.
	if w := call(other, http.MethodDelete, "/v1/money/low-balance/"+set.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("another workspace deleting the threshold = %d, want 404", w.Code)
	}
	if w := call(ws, http.MethodDelete, "/v1/money/low-balance/"+set.ID, ""); w.Code != http.StatusOK || rows() != before+1 {
		t.Fatalf("delete = %d, rows %d", w.Code, rows())
	}
}
