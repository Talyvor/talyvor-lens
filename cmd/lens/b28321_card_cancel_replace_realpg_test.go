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
	"github.com/talyvor/lens/internal/tenant"
)

// B28.321 — the talyvor-lens side of B28.47: an owner replaces an agent's card, and later cancels it. After the
// replacement a purchase on the old card is declined and writes no posting, and one on the new card is approved and
// writes one; after the cancel a purchase on it writes none, and the agent can be issued a new card. Through the real
// routes on the migrated schema, asserting the ledger, never a status code alone.
func TestB28321_CancelledCardWritesNoPostingReplacementDoes(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28321"
	for _, q := range []string{
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc, test_funded_ulxc) VALUES ('ws-b28321', 1000000000, 1000000000, 1000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-b28321', 'ws-b28321', 'ws-b28321')`,
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
	if code, body := call(http.MethodPost, card+"/replace", ""); code != http.StatusNotFound {
		t.Fatalf("replacing a card the agent does not hold = %d %s, want 404", code, body)
	}
	holder := `{"first_name":"Test","last_name":"Shopper","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`
	if code, body := call(http.MethodPost, card, holder); code != http.StatusCreated {
		t.Fatalf("issue card = %d %s", code, body)
	}
	purchase := func(cardID, id string) economy.CardDecision {
		t.Helper()
		d, err := store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: "evt_" + id, AuthorizationID: "iauth_" + id,
			CardID: cardID, AmountMinor: 40, Currency: "gbp", MerchantName: "Shop " + id, At: time.Now(), USDMicros: 500_000})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// The card postings: one entry of two legs per approved purchase, agent → spend.
	postings := func() (entries, agentSum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(DISTINCT entry_id), COALESCE(sum(amount_ulxc) FILTER (WHERE account = $2), 0)
			FROM agent_postings WHERE workspace_id = $1 AND kind = 'card'`, ws, "agent:"+agent.ID).Scan(&entries, &agentSum); err != nil {
			t.Fatal(err)
		}
		return entries, agentSum
	}
	current := func() (int, economy.AgentCard) {
		t.Helper()
		var view struct {
			Card economy.AgentCard `json:"card"`
		}
		code, body := call(http.MethodGet, card, "")
		_ = json.Unmarshal([]byte(body), &view)
		return code, view.Card
	}

	// $0.50 is 5,000,000 µLXC at the peg.
	if d := purchase("ic_test_1", "before"); !d.Approved {
		t.Fatalf("a purchase on the first card = %+v, want approved", d)
	}
	if n, sum := postings(); n != 1 || sum != -5_000_000 {
		t.Fatalf("before the replacement: %d card entries taking %d from the agent; want 1 of −5,000,000", n, sum)
	}

	// Replace: a new card to the same cardholder, the old one cancelled in Lens and at Stripe.
	code, body = call(http.MethodPost, card+"/replace", "")
	var next economy.AgentCard
	if err := json.Unmarshal([]byte(body), &next); code != http.StatusCreated || err != nil || next.ID != "ic_test_2" || next.CancelledAt != nil {
		t.Fatalf("replace = %d %s, want 201 and the new card ic_test_2", code, body)
	}
	if len(issuer.replaced) != 1 || issuer.replaced[0] != "ich_test_1" || len(issuer.cancelled) != 1 || issuer.cancelled[0] != "ic_test_1" {
		t.Fatalf("at Stripe: replaced for %v and cancelled %v; want one card for ich_test_1 and ic_test_1 cancelled", issuer.replaced, issuer.cancelled)
	}
	if code, c := current(); code != http.StatusOK || c.ID != "ic_test_2" || c.Last4 != next.Last4 {
		t.Fatalf("GET card after the replacement = %d %+v, want ic_test_2", code, c)
	}
	if d := purchase("ic_test_1", "old"); d.Approved || d.Reason != economy.ErrAgentCardCancelled.Error() {
		t.Fatalf("a purchase on the replaced card = %+v, want declined as cancelled", d)
	}
	if n, sum := postings(); n != 1 || sum != -5_000_000 {
		t.Fatalf("the replaced card's purchase wrote postings: %d card entries taking %d; want still 1 of −5,000,000", n, sum)
	}
	if d := purchase("ic_test_2", "new"); !d.Approved || d.AmountULXC != 5_000_000 {
		t.Fatalf("a purchase on the new card = %+v, want approved at 5,000,000 µLXC", d)
	}
	if n, sum := postings(); n != 2 || sum != -10_000_000 {
		t.Fatalf("the new card's purchase: %d card entries taking %d; want 2 of −10,000,000 in all", n, sum)
	}

	// Cancel: the card pays for nothing more, the agent holds no card, and a new one can be issued.
	code, body = call(http.MethodPost, card+"/cancel", "")
	var gone economy.AgentCard
	if err := json.Unmarshal([]byte(body), &gone); code != http.StatusOK || err != nil || gone.ID != "ic_test_2" || gone.CancelledAt == nil {
		t.Fatalf("cancel = %d %s, want 200 and ic_test_2 cancelled", code, body)
	}
	if len(issuer.cancelled) != 2 || issuer.cancelled[1] != "ic_test_2" {
		t.Fatalf("cancelled at Stripe: %v, want ic_test_2 too", issuer.cancelled)
	}
	if d := purchase("ic_test_2", "cancelled"); d.Approved || d.Reason != economy.ErrAgentCardCancelled.Error() {
		t.Fatalf("a purchase on the cancelled card = %+v, want declined as cancelled", d)
	}
	if n, sum := postings(); n != 2 || sum != -10_000_000 {
		t.Fatalf("the cancelled card's purchase wrote postings: %d card entries taking %d; want still 2 of −10,000,000", n, sum)
	}
	var agentCardRows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'agent_card'`, ws).Scan(&agentCardRows); err != nil {
		t.Fatal(err)
	}
	if agentCardRows != 2 {
		t.Fatalf("lxc_ledger agent_card rows = %d, want 2: one for each approved purchase", agentCardRows)
	}
	if code, _ := current(); code != http.StatusNotFound {
		t.Fatalf("GET card after the cancel = %d, want 404: the agent holds no card", code)
	}
	if code, body := call(http.MethodPost, card+"/cancel", ""); code != http.StatusNotFound {
		t.Fatalf("cancelling again = %d %s, want 404", code, body)
	}
	if code, body := call(http.MethodPost, card, holder); code != http.StatusCreated {
		t.Fatalf("issuing a new card after the cancel = %d %s, want 201", code, body)
	}
}

// B28.321 — a replacement keeps what the owner set on the card it replaces: a card locked to the merchant of its first
// purchase is replaced by one locked to that merchant, and a frozen card by a frozen one, in the same transaction as
// the cancel. A purchase elsewhere on the first, and any purchase on the second until it is unfrozen, is declined and
// writes no posting.
func TestB28321_ReplacementKeepsMerchantLockAndFreeze(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28321-keep"
	for _, q := range []string{
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc, test_funded_ulxc) VALUES ('ws-b28321-keep', 1000000000, 1000000000, 1000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-b28321-keep', 'ws-b28321-keep', 'ws-b28321-keep')`,
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
	call := func(path, body string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("POST %s = %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	var agent economy.Agent
	_ = json.Unmarshal([]byte(call(base, `{"name":"buyer"}`)), &agent)
	card := base + "/" + agent.ID + "/card"
	call(base+"/"+agent.ID+"/fund", `{"amount_ulxc":800000000}`)
	call(card, `{"first_name":"Test","last_name":"Shopper","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`)
	replace := func() economy.AgentCard {
		t.Helper()
		var c economy.AgentCard
		_ = json.Unmarshal([]byte(call(card+"/replace", "")), &c)
		return c
	}
	purchase := func(cardID, id, merchantID string) economy.CardDecision {
		t.Helper()
		d, err := store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: "evt_keep_" + id, AuthorizationID: "iauth_keep_" + id,
			CardID: cardID, AmountMinor: 40, Currency: "gbp", MerchantName: "Shop " + merchantID, MerchantID: merchantID,
			At: time.Now(), USDMicros: 500_000})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	entries := func() (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(DISTINCT entry_id) FROM agent_postings WHERE workspace_id = $1 AND kind = 'card'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Locked to the merchant of its first purchase, which locks it to m-paper; its replacement pays m-paper only.
	call(card+"/merchant-lock", `{"lock":"first"}`)
	if d := purchase("ic_test_1", "first", "m-paper"); !d.Approved {
		t.Fatalf("the first purchase on the locked card = %+v, want approved", d)
	}
	next := replace()
	if next.ID != "ic_test_2" || next.MerchantLock != economy.CardMerchantLockFirst || next.LockedMerchantID != "m-paper" {
		t.Fatalf("the replacement = %+v, want ic_test_2 locked to m-paper", next)
	}
	if d := purchase("ic_test_2", "ink", "m-ink"); d.Approved || !strings.Contains(d.Reason, "m-paper") {
		t.Fatalf("a purchase at another merchant on the replacement = %+v, want declined by the lock to m-paper", d)
	}
	if n := entries(); n != 1 {
		t.Fatalf("after the declined purchase at m-ink: %d card entries, want still 1", n)
	}
	if d := purchase("ic_test_2", "paper", "m-paper"); !d.Approved {
		t.Fatalf("a purchase at m-paper on the replacement = %+v, want approved", d)
	}
	if n := entries(); n != 2 {
		t.Fatalf("after the replacement's purchase at m-paper: %d card entries, want 2", n)
	}

	// Frozen, then replaced: the replacement is frozen and declines every purchase until it is unfrozen.
	call(card+"/merchant-lock", `{"lock":"off"}`)
	call(card+"/freeze", "")
	if next = replace(); next.ID != "ic_test_3" || !next.Frozen {
		t.Fatalf("the frozen card's replacement = %+v, want ic_test_3 frozen", next)
	}
	if d := purchase("ic_test_3", "frozen", "m-ink"); d.Approved || d.Reason != economy.ErrAgentCardFrozen.Error() {
		t.Fatalf("a purchase on the frozen card's replacement = %+v, want declined as frozen", d)
	}
	if n := entries(); n != 2 {
		t.Fatalf("after the frozen replacement's purchase: %d card entries, want still 2", n)
	}
	call(card+"/unfreeze", "")
	if d := purchase("ic_test_3", "thawed", "m-ink"); !d.Approved {
		t.Fatalf("a purchase on the replacement once unfrozen = %+v, want approved", d)
	}
	if n := entries(); n != 3 {
		t.Fatalf("after the unfrozen replacement's purchase: %d card entries, want 3", n)
	}
}
