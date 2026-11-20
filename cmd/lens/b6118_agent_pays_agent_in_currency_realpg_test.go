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
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/tenant"
)

// B61.18 — an agent pays another agent from its euro account, through the real route on a migrated schema: inside its
// rules the amount moves exactly once and both accounts' statements show it, another workspace's agent included; above
// the amount its rules ask the owner for, it waits and moves nothing until the owner says yes; a replay moves nothing;
// over its limit, to an agent with no account in the currency, or in money the payee's workspace does not hold, it is
// refused and both balances stay as they were.
func TestB6118_AnAgentPaysAnotherAgentFromItsCurrencyAccount(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other, fiat = "ws-b6118", "ws-b6118-other", "ws-b6118-fiat"
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, r)
		})
	store, payer := b.store, b.acct.AgentID
	if _, err := store.SetAgentRules(ctx, ws, payer, economy.AgentRules{Currency: "EUR", MaxPerPaymentMinor: 20_000, ApprovalAboveMinor: 10_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'fiat')`, fiat); err != nil {
		t.Fatal(err)
	}
	// payee makes an agent whose own currency is currency, and answers its account in it.
	payee := func(w, name, currency string) (string, string) {
		t.Helper()
		a, err := store.CreateAgentOfKind(ctx, w, name, "owner-"+w, "", currency, economy.AgentLook{})
		if err != nil {
			t.Fatal(err)
		}
		var acct string
		_ = pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE agent_id = $1 AND purpose = 'agent' AND currency = $2`, a.ID, currency).Scan(&acct)
		if acct == "" {
			c, err := store.OpenCurrencyAccount(ctx, w, a.ID, currency, "test")
			if err != nil {
				t.Fatal(err)
			}
			acct = c.ID
		}
		return a.ID, acct
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", "USD", "test"); err != nil {
		t.Fatal(err)
	}
	same, sameAcct := payee(ws, "Ledger", "EUR")
	far, farAcct := payee(other, "Supplier", "EUR")
	dollars, _ := payee(ws, "Dollars", "USD")
	fiatAgent := func() string {
		a, err := store.CreateAgentOfKind(ctx, fiat, "Fiat only", "owner-"+fiat, "", "", economy.AgentLook{})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}()

	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	pay := func(key, to string, minor int64, currency string) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"to_agent_id": to, "amount_minor": minor, "currency": currency, "memo": "Invoice " + key})
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+payer+"/pay", strings.NewReader(string(body)))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	balances := func(want ...int64) {
		t.Helper()
		for i, a := range []struct{ ws, id string }{{ws, b.acct.ID}, {ws, sameAcct}, {other, farAcct}} {
			got, err := store.Balance(ctx, a.ws, a.id)
			if err != nil || got.TestMinor != want[i] {
				t.Fatalf("account %d holds %+v (%v), want %d", i, got, err, want[i])
			}
		}
	}
	balances(50_000, 0, 0)

	// Inside the rules: €50.00 moves once, as one entry of two postings; the same key again moves nothing.
	code, first := pay("k1", same, 5_000, "EUR")
	if code != http.StatusOK || first["id"] == nil || first["to_account_id"] != sameAcct {
		t.Fatalf("pay €50.00: %d %v, want 200 into the payee's euro account", code, first)
	}
	balances(45_000, 5_000, 0)
	if code, again := pay("k1", same, 5_000, "EUR"); code != http.StatusOK || again["id"] != first["id"] {
		t.Fatalf("the same key again: %d %v, want 200 and the first payment", code, again)
	}
	balances(45_000, 5_000, 0)
	var entries, postings int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT e.id), count(*) FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		WHERE e.kind = 'agent_payment'`).Scan(&entries, &postings); err != nil || entries != 1 || postings != 2 {
		t.Fatalf("the ledger has %d agent payments of %d postings (%v), want 1 of 2", entries, postings, err)
	}
	// Another workspace's agent: €20.00 lands in its euro account.
	if code, out := pay("k2", far, 2_000, "EUR"); code != http.StatusOK || out["to_workspace_id"] != other {
		t.Fatalf("pay another workspace's agent: %d %v, want 200", code, out)
	}
	balances(43_000, 5_000, 2_000)
	for _, s := range []struct{ ws, agent, acct string }{{ws, payer, b.acct.ID}, {ws, same, sameAcct}, {other, far, farAcct}} {
		st, err := store.AccountStatement(ctx, s.ws, s.agent, s.acct, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, l := range st.Lines {
			if l.Kind == "agent_payment" {
				n++
			}
		}
		if want := map[string]int{payer: 2, same: 1, far: 1}[s.agent]; n != want {
			t.Fatalf("%s's statement shows %d agent payments, want %d: %+v", s.agent, n, want, st.Lines)
		}
	}

	// Above the €100.00 the rules ask the owner for: it waits and moves nothing, then the owner's yes moves it once.
	code, held := pay("k3", same, 15_000, "EUR")
	if code != http.StatusAccepted || held["status"] != "pending_approval" {
		t.Fatalf("pay €150.00: %d %v, want 202 pending_approval", code, held)
	}
	balances(43_000, 5_000, 2_000)
	if _, err := store.DecideAgentApproval(ctx, ws, held["approval_id"].(string), true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, out := pay("k3", same, 15_000, "EUR"); code != http.StatusOK {
			t.Fatalf("pay €150.00 after the owner's yes: %d %v, want 200", code, out)
		}
	}
	balances(28_000, 20_000, 2_000)

	// Refused, and nothing moves: over the €200.00 limit, a payee with no account in the currency, money of a kind the
	// payee's workspace does not hold.
	for _, c := range []struct {
		name, to, currency, want string
		minor                    int64
		code                     int
	}{
		{"over the limit per payment", same, "EUR", "per payment", 20_001, http.StatusForbidden},
		{"no account in the currency", dollars, "EUR", "Convert or Instant transfer", 1_000, http.StatusConflict},
		{"another kind of money", fiatAgent, "USDC", "wrong_money_kind", 1_000_000, http.StatusConflict},
	} {
		code, out := pay("refused-"+c.name, c.to, c.minor, c.currency)
		body, _ := json.Marshal(out)
		if code != c.code || !strings.Contains(string(body), c.want) {
			t.Fatalf("%s: %d %s, want %d naming %q", c.name, code, body, c.code, c.want)
		}
	}
	balances(28_000, 20_000, 2_000)
}
