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
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

type fakeCardIssuer struct{ issued int }

func (f *fakeCardIssuer) IssueCard(_ context.Context, _, agentID, _ string, _ agentcard.Cardholder) (economy.AgentCard, error) {
	f.issued++
	return economy.AgentCard{ID: "ic_test_1", AgentID: agentID, StripeCardholderID: "ich_test_1", Last4: "4242",
		ExpMonth: 9, ExpYear: 2029, Currency: "gbp"}, nil
}

// B19.12 — in test mode an agent's card is approved within its rules and declined outside them, and every
// authorisation is a ledger row: through the real routes and Stripe's signed real-time request, on the
// migrated schema, priced at the ECB reference rate of the day.
func TestAgentCards_ApprovedWithinRulesDeclinedOutsideEveryAuthorisationALedgerRow(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-cards"
	for _, q := range []string{
		// Bought in Stripe test mode: test-funded, which a card (class RED, B22.1) may spend without a clearance.
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc, test_funded_ulxc) VALUES ('ws-cards', 1000000000, 1000000000, 1000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-cards', 'ws-cards', 'ws-cards')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	issuer := &fakeCardIssuer{}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCardRoutes(r, store, issuer)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-nicolai", Scopes: []string{auth.ScopeKeys}}
	call := func(router http.Handler, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	code, body := call(r, http.MethodPost, base, `{"name":"buyer"}`)
	if code != http.StatusCreated {
		t.Fatalf("create agent = %d %s", code, body)
	}
	var agent economy.Agent
	_ = json.Unmarshal([]byte(body), &agent)
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/" + agent.ID + "/fund", `{"amount_ulxc":800000000}`},          // 800 LXC = $80
		{http.MethodPut, base + "/" + agent.ID + "/rules", `{"max_per_request_ulxc":300000000}`}, // at most 300 LXC = $30 a purchase
	} {
		if code, body := call(r, step.method, step.path, step.body); code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", step.method, step.path, code, body)
		}
	}

	// A live Stripe key issues nothing: cards are test money only.
	live := chi.NewRouter()
	mountAgentCardRoutes(live, store, agentcard.NewStripe("sk_live_not_a_real_key", "gbp"))
	holder := `{"first_name":"Nicolai","last_name":"Gaborac","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`
	if code, body := call(live, http.MethodPost, base+"/"+agent.ID+"/card", holder); code != http.StatusConflict || !strings.Contains(body, "no real money") {
		t.Fatalf("issuing with a live key = %d %s, want 409", code, body)
	}
	if code, body := call(r, http.MethodPost, base+"/"+agent.ID+"/card", holder); code != http.StatusCreated {
		t.Fatalf("issue card = %d %s", code, body)
	}

	// The ECB's file for the day before (the rate is the latest published on or before the purchase's date).
	ecb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <gesmes:subject>Reference rates</gesmes:subject>
  <Cube><Cube time="2026-09-25"><Cube currency="USD" rate="1.1700"/><Cube currency="JPY" rate="169.50"/><Cube currency="GBP" rate="0.8700"/></Cube></Cube>
</gesmes:Envelope>`)
	}))
	defer ecb.Close()
	const secret = "whsec_test_agent_cards"
	hook := agentcard.NewHandler(secret, store, ecbrate.New(pool, ecb.URL))
	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	purchase := func(eventID, authID string, pence int64, livemode bool) (bool, string) {
		t.Helper()
		payload := fmt.Sprintf(`{"id":%q,"object":"event","type":"issuing_authorization.request","created":%d,"livemode":%t,
			"data":{"object":{"id":%q,"object":"issuing_authorization","livemode":%t,"card":{"id":"ic_test_1","object":"issuing.card"},
			"pending_request":{"amount":%d,"currency":"gbp","merchant_amount":%d,"merchant_currency":"gbp","is_amount_controllable":false},
			"merchant_data":{"name":"Paper Co","category":"stationery_stores","network_id":"m-1"}}}}`,
			eventID, at.Unix(), livemode, authID, livemode, pence, pence)
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(payload), Secret: secret})
		req := httptest.NewRequest(http.MethodPost, "/v1/agent-cards/authorizations", strings.NewReader(payload))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		hook.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Header().Get("Stripe-Version") == "" {
			t.Fatalf("%s answered %d with Stripe-Version %q: %s", eventID, w.Code, w.Header().Get("Stripe-Version"), w.Body.String())
		}
		var out struct {
			Approved bool              `json:"approved"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Approved, out.Metadata["talyvor_reason"]
	}

	// £20.00 at 1.17 USD and 0.87 GBP per EUR is $26.896552 (rounded up) = 268,965,520 µLXC: within the rules.
	if ok, why := purchase("evt_a", "iauth_a", 2000, false); !ok {
		t.Fatalf("£20 within the rules was declined: %s", why)
	}
	// £25.00 is $33.620690 = 336,206,900 µLXC: above the 300 LXC limit per purchase.
	if ok, why := purchase("evt_b", "iauth_b", 2500, false); ok || !strings.Contains(why, "limit per request") {
		t.Fatalf("£25 above the limit per purchase: approved=%t %q, want declined by the limit", ok, why)
	}
	// Live mode is declined whatever the rules say (class RED).
	if ok, why := purchase("evt_c", "iauth_c", 100, true); ok || !strings.Contains(why, "no real money") {
		t.Fatalf("a live-mode purchase: approved=%t %q, want declined", ok, why)
	}
	// Stripe sending the approved request again moves nothing.
	if ok, _ := purchase("evt_a", "iauth_a", 2000, false); !ok {
		t.Fatal("the replayed approval was not answered as approved")
	}

	// Every authorisation is a row; only the approved one debited the agent, at the rate on its row.
	type row struct {
		approved           bool
		rateDate, usd, gbp *string
		usdMicros, ulxc    *int64
	}
	rows := map[string]row{}
	res, err := pool.Query(ctx, `SELECT id, approved, rate_date::text, ecb_usd_per_eur::text, ecb_currency_per_eur::text, amount_usd_micros, amount_ulxc
		FROM agent_card_authorizations WHERE agent_id = $1`, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
		var id string
		var x row
		if err := res.Scan(&id, &x.approved, &x.rateDate, &x.usd, &x.gbp, &x.usdMicros, &x.ulxc); err != nil {
			t.Fatal(err)
		}
		rows[id] = x
	}
	res.Close()
	if len(rows) != 3 || !rows["evt_a"].approved || rows["evt_b"].approved || rows["evt_c"].approved {
		t.Fatalf("authorisation rows = %+v, want evt_a approved and evt_b, evt_c declined", rows)
	}
	a := rows["evt_a"]
	if a.rateDate == nil || *a.rateDate != "2026-09-25" || *a.usd != "1.1700" || *a.gbp != "0.8700" || *a.usdMicros != 26_896_552 || *a.ulxc != 268_965_520 {
		t.Errorf("evt_a priced at %v %v %v → %v µUSD, %v µLXC; want 2026-09-25, 1.1700, 0.8700 → 26,896,552 µUSD, 268,965,520 µLXC",
			*a.rateDate, *a.usd, *a.gbp, *a.usdMicros, *a.ulxc)
	}
	var ledgerRows, ledgerSum, wsBal int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'agent_card'`, ws).
		Scan(&ledgerRows, &ledgerSum); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT balance FROM lxc_balances WHERE workspace_id = $1`, ws).Scan(&wsBal); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 1 || ledgerSum != -268_965_520 || wsBal != 1_000_000_000-268_965_520 {
		t.Errorf("lxc_ledger agent_card rows = %d summing %d, workspace balance %d; want 1 row of −268,965,520 and %d",
			ledgerRows, ledgerSum, wsBal, 1_000_000_000-268_965_520)
	}
	code, body = call(r, http.MethodGet, base+"/"+agent.ID+"/card", "")
	var view struct {
		Card           economy.AgentCard                 `json:"card"`
		Authorizations []economy.CardAuthorizationRecord `json:"authorizations"`
	}
	if err := json.Unmarshal([]byte(body), &view); code != http.StatusOK || err != nil || view.Card.Last4 != "4242" || len(view.Authorizations) != 3 {
		t.Errorf("GET card = %d %s", code, body)
	}
	book, err := store.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if b := book.Agents[0]; b.BalanceULXC != 800_000_000-268_965_520 || b.SpentULXC != 268_965_520 {
		t.Errorf("the agent holds %d and spent %d; want %d and 268,965,520", b.BalanceULXC, b.SpentULXC, 800_000_000-268_965_520)
	}
}

// B17.111 — a frozen card declines a purchase and nothing leaves the agent; unfrozen, the next purchase is approved
// and is one ledger row of what it cost.
func TestAgentCards_FrozenDeclinesUnfrozenApprovesOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-cards-freeze"
	for _, q := range []string{
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc, test_funded_ulxc) VALUES ('ws-cards-freeze', 1000000000, 1000000000, 1000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-cards-freeze', 'ws-cards-freeze', 'ws-cards-freeze')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCardRoutes(r, store, &fakeCardIssuer{})
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-nicolai", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	code, body := call(http.MethodPost, base, `{"name":"buyer"}`)
	if code != http.StatusCreated {
		t.Fatalf("create agent = %d %s", code, body)
	}
	var agent economy.Agent
	_ = json.Unmarshal([]byte(body), &agent)
	card := base + "/" + agent.ID + "/card"
	if code, body := call(http.MethodPost, base+"/"+agent.ID+"/fund", `{"amount_ulxc":800000000}`); code != http.StatusOK {
		t.Fatalf("fund = %d %s", code, body)
	}
	if code, body := call(http.MethodPost, card+"/freeze", ""); code != http.StatusNotFound {
		t.Fatalf("freezing a card the agent does not hold = %d %s, want 404", code, body)
	}
	if code, body := call(http.MethodPost, card, `{"first_name":"Test","last_name":"Shopper","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`); code != http.StatusCreated {
		t.Fatalf("issue card = %d %s", code, body)
	}
	frozen := func(want bool) {
		t.Helper()
		var view struct {
			Card economy.AgentCard `json:"card"`
		}
		code, body := call(http.MethodGet, card, "")
		if err := json.Unmarshal([]byte(body), &view); code != http.StatusOK || err != nil || view.Card.Frozen != want {
			t.Fatalf("GET card = %d %s, want frozen %t", code, body, want)
		}
	}
	purchase := func(id string) economy.CardDecision {
		t.Helper()
		d, err := store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: "evt_" + id, AuthorizationID: "iauth_" + id,
			CardID: "ic_test_1", AmountMinor: 40, Currency: "gbp", MerchantName: "Shop " + id, At: time.Now(), USDMicros: 500_000})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	ledger := func() (rows, sum, agentBal int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'agent_card'`, ws).
			Scan(&rows, &sum); err != nil {
			t.Fatal(err)
		}
		book, err := store.AgentBook(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return rows, sum, book.Agents[0].BalanceULXC
	}

	if code, body := call(http.MethodPost, card+"/freeze", ""); code != http.StatusOK || !strings.Contains(body, `"frozen":true`) {
		t.Fatalf("freeze = %d %s", code, body)
	}
	frozen(true)
	if d := purchase("frozen"); d.Approved || d.Reason != economy.ErrAgentCardFrozen.Error() {
		t.Fatalf("a purchase on the frozen card = %+v, want declined as frozen", d)
	}
	if rows, sum, bal := ledger(); rows != 0 || sum != 0 || bal != 800_000_000 {
		t.Fatalf("frozen: %d agent_card rows summing %d, the agent holds %d; want none and 800,000,000", rows, sum, bal)
	}
	if code, body := call(http.MethodPost, card+"/unfreeze", ""); code != http.StatusOK || !strings.Contains(body, `"frozen":false`) {
		t.Fatalf("unfreeze = %d %s", code, body)
	}
	frozen(false)
	// $0.50 is 5,000,000 µLXC at the peg.
	if d := purchase("open"); !d.Approved || d.AmountULXC != 5_000_000 {
		t.Fatalf("the next purchase = %+v, want approved at 5,000,000 µLXC", d)
	}
	if rows, sum, bal := ledger(); rows != 1 || sum != -5_000_000 || bal != 795_000_000 {
		t.Fatalf("unfrozen: %d agent_card rows summing %d, the agent holds %d; want one of −5,000,000 and 795,000,000", rows, sum, bal)
	}
}
