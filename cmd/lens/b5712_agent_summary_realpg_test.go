package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B57.12 — GET …/agents/{id}/summary gives an agent's page in one read: its balance less what waits is its safe to
// spend, its card says active, frozen or none and its last decline in words, a paused agent reads paused, and another
// workspace's agent is 404.
func TestAgentSummary_OneReadGivesThePageAtOneMoment(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other = "ws-b5712", "ws-b5712-other"
	const lxc = int64(1_000_000)
	for _, q := range []string{
		// Test-funded, which a card (class RED, B22.1) may spend without a clearance.
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc, test_funded_ulxc) VALUES ('ws-b5712', 2000000000, 2000000000, 2000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-b5712', 'ws-b5712', 'ws-b5712')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	as := func(who *auth.AuthContext, path string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	summary := func(agentID string) economy.AgentSummary {
		t.Helper()
		code, body := as(owner, "/v1/workspaces/"+ws+"/agents/"+agentID+"/summary")
		if code != http.StatusOK {
			t.Fatalf("summary = %d %s", code, body)
		}
		var s economy.AgentSummary
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	research, err := store.CreateAgent(ctx, ws, "Research", "owner")
	if err != nil {
		t.Fatal(err)
	}
	sales, err := store.CreateAgent(ctx, ws, "Sales", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, research.ID, 1_000*lxc); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, research.ID, economy.AgentRules{MonthlyLimitULXC: 500 * lxc, ApprovalAboveULXC: 100 * lxc,
		Timezone: "Europe/London", AllowedPayees: []string{sales.ID, "acct_lead_finder"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, research.ID, sales.ID, 40*lxc, "leads"); err != nil {
		t.Fatal(err)
	}
	var need *economy.ApprovalNeededError
	if _, err := store.PayAgent(ctx, ws, research.ID, sales.ID, 200*lxc, "a bigger list"); !errors.As(err, &need) {
		t.Fatalf("a payment above the approval amount = %v; want it waiting for approval", err)
	}
	if _, err := store.SaveAgentCard(ctx, ws, economy.AgentCard{ID: "ic_b5712", AgentID: research.ID, StripeCardholderID: "ich_b5712",
		Last4: "4242", ExpMonth: 12, ExpYear: 2030, Currency: "gbp"}); err != nil {
		t.Fatal(err)
	}
	d, err := store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: "evt_b5712", AuthorizationID: "iauth_b5712", CardID: "ic_b5712",
		AmountMinor: 2400, Currency: "gbp", MerchantName: "Compute shop", MerchantID: "acct_compute", At: time.Now(), USDMicros: 30_000_000})
	if err != nil || d.Approved {
		t.Fatalf("a purchase from a payee its rules do not name = %+v, %v; want declined", d, err)
	}

	s := summary(research.ID)
	if s.AsOf.IsZero() || s.Agent.ID != research.ID || s.Agent.Colour == "" || s.Agent.Face == "" || s.Agent.PausedAt != nil {
		t.Fatalf("agent = %+v as of %s; want Research with its colour and face, not paused", s.Agent, s.AsOf)
	}
	if !s.MadeAt.Equal(research.CreatedAt) || s.OwnerUserID != "owner" {
		t.Errorf("made %s by %q; want %s by owner", s.MadeAt, s.OwnerUserID, research.CreatedAt)
	}
	// The figures agree: the balance less what waits is what is safe to spend.
	if len(s.SafeToSpend) == 0 || s.SafeToSpend[0].Currency != "LXC" {
		t.Fatalf("safe to spend = %+v; want LXC first", s.SafeToSpend)
	}
	waits := int64(0)
	for _, a := range s.Approvals {
		waits += a.AmountULXC
	}
	lxcSafe := s.SafeToSpend[0]
	if s.Agent.BalanceULXC != 960*lxc || lxcSafe.Balance != s.Agent.BalanceULXC || waits != 200*lxc || lxcSafe.PendingApprovals != waits ||
		lxcSafe.SafeToSpend != s.Agent.BalanceULXC-waits {
		t.Errorf("balance %d, waits %d, safe to spend %+v; want 960 LXC less 200 LXC waiting", s.Agent.BalanceULXC, waits, lxcSafe)
	}
	if len(s.Approvals) != 1 || s.Approvals[0].PaidFrom == nil || s.Approvals[0].PaidFrom.Kind != "lxc" {
		t.Errorf("approvals = %+v; want the one waiting, with what pays it", s.Approvals)
	}
	if s.Month.MonthlyLimitULXC != 500*lxc || s.Month.Start.IsZero() || !s.Month.End.After(s.AsOf) {
		t.Errorf("month = %+v; want its 500 LXC limit and this month", s.Month)
	}
	if len(s.Statement) == 0 || len(s.Statement) > economy.AgentSummaryLines || s.StatementTimeZone != "Europe/London" {
		t.Errorf("statement = %+v in %q; want its newest lines in its zone", s.Statement, s.StatementTimeZone)
	}
	if s.Rules == nil || s.Rules.Version < 1 || s.Rules.SavedAt.After(s.AsOf) {
		t.Errorf("rules = %+v; want the version in force", s.Rules)
	}
	if s.Card.Status != "active" || s.Card.Currency != "gbp" || s.Card.LastDeclined == nil ||
		s.Card.LastDeclined.Words != "Declined: not one of its payees" || s.Card.LastDeclined.MerchantName != "Compute shop" ||
		s.Card.LastDeclined.AmountMinor != 2400 {
		t.Errorf("card = %+v, last declined %+v; want active in gbp, Compute shop's £24.00 declined: not one of its payees",
			s.Card, s.Card.LastDeclined)
	}

	// A frozen card and a paused agent read as they are.
	if _, err := store.SetAgentCardFrozen(ctx, ws, research.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.PauseAgent(ctx, ws, research.ID, "the owner paused it"); err != nil {
		t.Fatal(err)
	}
	if s := summary(research.ID); s.Card.Status != "frozen" || s.Agent.PausedAt == nil {
		t.Errorf("card %q, paused at %v; want frozen and paused", s.Card.Status, s.Agent.PausedAt)
	}

	// No card, no rules, nothing waiting.
	if s := summary(sales.ID); s.Card.Status != "none" || s.Card.LastDeclined != nil || s.Rules != nil || len(s.Approvals) != 0 {
		t.Errorf("Sales: card %+v, rules %+v, approvals %+v; want none of them", s.Card, s.Rules, s.Approvals)
	}

	// Another workspace's owner gets 404 for this agent, and a key that is not the owner's is refused.
	otherOwner := &auth.AuthContext{WorkspaceID: other, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	if code, body := as(otherOwner, "/v1/workspaces/"+other+"/agents/"+research.ID+"/summary"); code != http.StatusNotFound {
		t.Errorf("another workspace's agent = %d %s; want 404", code, body)
	}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	if code, _ := as(proxyKey, "/v1/workspaces/"+ws+"/agents/"+research.ID+"/summary"); code != http.StatusForbidden {
		t.Errorf("a proxy key = %d; want 403", code)
	}
}

// B57.12 — a declined purchase's recorded reason reads as words.
func TestDeclineWords(t *testing.T) {
	for reason, want := range map[string]string{
		economy.ErrAgentRule.Error() + ": the agent may pay only the payees its rules name, and Compute shop is not one": "Declined: not one of its payees",
		economy.ErrAgentCardFrozen.Error():                    "Declined: the card is frozen",
		"the agent holds 1 LXC and this purchase costs 2 LXC": "Declined: not enough in its balance",
		"the request carries no amount":                       "Declined: the request carries no amount",
	} {
		if got := economy.DeclineWords(reason); got != want {
			t.Errorf("DeclineWords(%q) = %q; want %q", reason, got, want)
		}
	}
}
