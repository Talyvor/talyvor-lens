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
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B56.23 — an agent converts between its currency accounts at the Test partner's reference rate (£1 = $1.25) plus its
// plan's FX margin: free's 0, team's 50 basis points, an Enterprise contract's own. The conversion is one money entry
// under its key, so a retry converts once; a quote converts once and never after it expires; both legs are lines on
// the agent's account statements. All read off the ledger.
func TestB5623_ConvertAtThePlansFXMargin(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5623-convert"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screener)
	store.SetFXMargin(func(ctx context.Context, q economy.FeeQuerier, workspaceID string) (int64, error) {
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
	for _, cur := range []string{"GBP", "USD"} {
		if _, err := store.OpenCurrencyAccount(ctx, ws, "", cur, "test"); err != nil {
			t.Fatal(err)
		}
		a, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, cur, "test")
		if err != nil {
			t.Fatal(err)
		}
		accounts[cur] = a.ID
	}
	var gbpPartner string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).Scan(&gbpPartner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: gbpPartner, AmountMinor: -500_00}, {AccountID: accounts["GBP"], AmountMinor: 500_00}}}); err != nil {
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
	quote := func() economy.Conversion {
		t.Helper()
		code, q, body := call(base+"/quote", "", `{"sell_currency":"GBP","buy_currency":"USD","sell_minor":10000}`)
		if code != http.StatusCreated {
			t.Fatalf("quote = %d %s", code, body)
		}
		return q
	}
	entries := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'fx_conversion'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The margin per plan and contract: £100.00 less the margin, at $1.25.
	if q := quote(); q.MarginBPS != 0 || q.MarginMinor != 0 || q.BuyMinor != 125_00 || q.ReferenceRate != "1.25000000" || q.Rate != "1.25000000" {
		t.Fatalf("free quote %+v; want no margin, $125.00 at 1.25", q)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id,
		status, livemode, last_event_at, plan, byok) VALUES ($1, 'sub_b5623', 'cus_b5623', 'price_team', 'active', false, NOW(), 'team', false)`, ws); err != nil {
		t.Fatal(err)
	}
	team := quote()
	if team.MarginBPS != 50 || team.MarginMinor != 50 || team.BuyMinor != 124_37 || team.Rate != "1.24370000" {
		t.Fatalf("team quote %+v; want 50 bps, 50p margin, $124.37 for £100.00", team)
	}
	seven := int64(7)
	if _, err := billing.SetContract(ctx, pool, billing.Contract{WorkspaceID: ws, FXMarginBPS: &seven, Reference: "B56.23"}, "tester"); err != nil {
		t.Fatal(err)
	}
	if q := quote(); q.MarginBPS != 7 || q.MarginMinor != 7 || q.BuyMinor != 124_91 {
		t.Fatalf("contract quote %+v; want 7 bps, 7p margin, $124.91 for £99.93", q)
	}

	// Converting at the team quote moves both legs in one entry; the same key again converts nothing more.
	code, c, body := call(base, "k1", `{"quote_id":"`+team.ID+`"}`)
	if code != http.StatusOK || c.Status != economy.ConversionConverted || c.EntryID == "" || c.ValueULXC != 1_250_000_000 {
		t.Fatalf("convert = %d %s; want converted, valued at 1,250 LXC", code, body)
	}
	if code, again, body := call(base, "k1", `{"quote_id":"`+team.ID+`"}`); code != http.StatusOK || again.ID != c.ID || again.EntryID != c.EntryID {
		t.Fatalf("retry = %d %s; want the same conversion", code, body)
	}
	if n := entries(); n != 1 {
		t.Fatalf("%d conversion entries after a retry; want 1", n)
	}
	rows, err := pool.Query(ctx, `SELECT a.purpose, p.currency, p.amount_minor FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		WHERE p.entry_id = $1 ORDER BY p.line`, c.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	var legs []string
	for rows.Next() {
		var purpose, cur string
		var amount int64
		_ = rows.Scan(&purpose, &cur, &amount)
		legs = append(legs, purpose+" "+cur+" "+jsonOf(amount))
	}
	rows.Close()
	if want := "agent GBP -10000|revenue GBP 50|partner GBP 9950|partner USD -12437|agent USD 12437"; strings.Join(legs, "|") != want {
		t.Fatalf("postings %v; want %s", legs, want)
	}

	// A quote converts once: another key at it is refused. A stale quote is refused and moves nothing.
	if code, _, body := call(base, "k2", `{"quote_id":"`+team.ID+`"}`); code != http.StatusConflict {
		t.Fatalf("a converted quote under another key = %d %s; want 409", code, body)
	}
	stale := quote()
	if _, err := pool.Exec(ctx, `UPDATE money_conversions SET expires_at = now() - interval '1 second' WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, body := call(base, "k3", `{"quote_id":"`+stale.ID+`"}`); code != http.StatusConflict || !strings.Contains(body, "expired") {
		t.Fatalf("a stale quote = %d %s; want 409, expired", code, body)
	}
	if n := entries(); n != 1 {
		t.Fatalf("%d conversion entries after the refusals; want 1", n)
	}

	// Both legs are lines on the agent's account statements.
	for cur, want := range map[string]int64{"GBP": -100_00, "USD": 124_37} {
		st, err := store.AccountStatement(ctx, ws, agent.ID, accounts[cur], time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		l := st.Lines[len(st.Lines)-1]
		if l.Kind != "fx_conversion" || l.AmountMinor != want || l.EntryID != c.EntryID {
			t.Errorf("%s statement ends %+v; want the fx_conversion line of %d", cur, l, want)
		}
	}
}
