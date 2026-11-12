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
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.29 — an agent converts €100.00 to pounds at the ECB reference rate (€1 = £0.86) plus its plan's margin (team's
// 50 basis points), good for 30 seconds: one entry posts both currencies, each summing to zero, with the margin to
// Talyvor's revenue. An expired quote, a currency its rules do not allow, and a conversion over its daily conversion
// maximum are refused and post nothing; the free plan converts test money only. All read off the ledger.
func TestB3029_ConvertBetweenCurrencies(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3029-convert"
	for cur, perEUR := range map[string]string{"USD": "1.08", "GBP": "0.86"} {
		if _, err := pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES (current_date - 1, $1, $2::numeric)`,
			cur, perEUR); err != nil {
			t.Fatal(err)
		}
	}
	registry := partners.NewRegistry(nil)
	registry.UseFXRates(ecbrate.New(pool, ""))
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screening.NewScreener(pool, registry))
	store.SetFXMargin(func(ctx context.Context, q economy.FeeQuerier, workspaceID string) (int64, string, error) {
		return billing.FXMarginBPS(ctx, q, workspaceID, fees.Defaults())
	})
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, 'Convert Ltd', $1)`, ws); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "Converter", "owner")
	if err != nil {
		t.Fatal(err)
	}
	accounts := map[string]string{}
	for _, cur := range []string{"EUR", "GBP", "USD"} {
		if _, err := store.OpenCurrencyAccount(ctx, ws, "", cur, "test"); err != nil {
			t.Fatal(err)
		}
		a, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, cur, "test")
		if err != nil {
			t.Fatal(err)
		}
		accounts[cur] = a.ID
	}
	var eurPartner string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'EUR'`, ws).Scan(&eurPartner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: eurPartner, AmountMinor: -1000_00}, {AccountID: accounts["EUR"], AmountMinor: 1000_00}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id,
		status, livemode, last_event_at, plan, byok) VALUES ($1, 'sub_b3029', 'cus_b3029', 'price_team', 'active', false, NOW(), 'team', false)`, ws); err != nil {
		t.Fatal(err)
	}
	// Euros and pounds only, and at most 1,500 LXC ($150) converted a day: €100.00 is worth $108.00 at the ECB rate.
	maxULXC := int64(1_500_000_000)
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{Timezone: "UTC", AllowedCurrencies: []string{"EUR", "GBP"},
		FXDailyLimitULXC: &maxULXC}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyConvertRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	base := "/v1/workspaces/" + ws + "/agents/" + agent.ID + "/money/convert"
	call := func(path, key, body string) (int, economy.Conversion, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var c economy.Conversion
		_ = json.Unmarshal(w.Body.Bytes(), &c)
		return w.Code, c, w.Body.String()
	}
	quote := func(buy, funding string) (int, economy.Conversion, string) {
		t.Helper()
		return call(base+"/quote", "", `{"sell_currency":"EUR","buy_currency":"`+buy+`","sell_minor":10000,"funding":"`+funding+`"}`)
	}
	entries := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'fx_conversion'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// €100.00 less 50p margin at £0.86, good for 30 seconds.
	code, q, body := quote("GBP", "test")
	if code != http.StatusCreated {
		t.Fatalf("quote = %d %s", code, body)
	}
	if q.MarginMinor != 50 || q.BuyMinor != 85_57 || q.ReferenceRate != "0.86000000" {
		t.Fatalf("quote %+v; want a 50 cent margin and £85.57 at the ECB's 0.86", q)
	}
	if life := time.Until(q.ExpiresAt); life > 30*time.Second {
		t.Fatalf("quote good for %s; want 30 seconds at most", life)
	}

	// It converts in one entry that balances in each currency at the quoted rate.
	code, c, body := call(base, "k1", `{"quote_id":"`+q.ID+`"}`)
	if code != http.StatusOK || c.Status != economy.ConversionConverted || c.EntryID == "" {
		t.Fatalf("convert = %d %s; want converted", code, body)
	}
	sums := map[string]int64{}
	var legs []string
	rows, err := pool.Query(ctx, `SELECT a.purpose, p.currency, p.amount_minor FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		WHERE p.entry_id = $1 ORDER BY p.line`, c.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var purpose, cur string
		var amount int64
		if err := rows.Scan(&purpose, &cur, &amount); err != nil {
			t.Fatal(err)
		}
		sums[cur] += amount
		legs = append(legs, purpose+" "+cur+" "+jsonOf(amount))
	}
	rows.Close()
	if want := "agent EUR -10000|revenue EUR 50|partner EUR 9950|partner GBP -8557|agent GBP 8557"; strings.Join(legs, "|") != want {
		t.Fatalf("postings %v; want %s", legs, want)
	}
	if sums["EUR"] != 0 || sums["GBP"] != 0 {
		t.Fatalf("the entry sums to %v; want zero in each currency", sums)
	}

	// An expired quote is refused and posts nothing.
	_, stale, _ := quote("GBP", "test")
	if _, err := pool.Exec(ctx, `UPDATE money_conversions SET expires_at = now() - interval '1 second' WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, body := call(base, "k2", `{"quote_id":"`+stale.ID+`"}`); code == http.StatusOK || !strings.Contains(body, "expired") {
		t.Fatalf("an expired quote = %d %s; want refused, expired", code, body)
	}

	// A currency the agent's rules leave out is refused.
	_, usd, _ := quote("USD", "test")
	if code, _, body := call(base, "k3", `{"quote_id":"`+usd.ID+`"}`); code == http.StatusOK || !strings.Contains(body, "converts only between EUR, GBP") {
		t.Fatalf("euros to dollars = %d %s; want refused by its currencies", code, body)
	}

	// A second €100.00 would take the day to $216.00, over its $150 maximum: refused.
	_, again, _ := quote("GBP", "test")
	if code, _, body := call(base, "k4", `{"quote_id":"`+again.ID+`"}`); code == http.StatusOK || !strings.Contains(body, "daily conversion maximum") {
		t.Fatalf("over the daily maximum = %d %s; want refused", code, body)
	}
	if n := entries(); n != 1 {
		t.Fatalf("%d conversion entries after the refusals; want 1", n)
	}
	var gbp int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1`,
		accounts["GBP"]).Scan(&gbp); err != nil {
		t.Fatal(err)
	}
	if gbp != 85_57 {
		t.Fatalf("the agent holds %d pence; want 8557, the one conversion", gbp)
	}

	// The free plan converts test money only.
	if _, err := pool.Exec(ctx, `DELETE FROM subscriptions WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if code, _, body := quote("GBP", "live"); code != http.StatusBadRequest || !strings.Contains(body, "live conversions") {
		t.Fatalf("a live quote on free = %d %s; want 400", code, body)
	}
}
