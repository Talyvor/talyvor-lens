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

// B61.66 — the owner gives an agent $10.00 from the company's USD account now, and takes $4.00 back. Each is one entry
// under currency_accounts, read off the postings on both accounts and both statements; the same key again moves
// nothing; more than the company holds, or than the agent holds, is refused and posts nothing; another workspace's
// agent account is 404; a member who is not owner or admin is 403; live money is refused until it is cleared; and
// test money still goes into a company account only.
func TestAgentFunding_OwnerGivesAndTakesBackOnBothStatementsOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other = "ws-b6166-funding", "ws-b6166-other"
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screening.NewScreener(pool, registry))
	agent, err := store.CreateAgent(ctx, ws, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentAcct, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, company.ID, 5000, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, agentAcct.ID, 5000, "straight-in"); err == nil {
		t.Error("test money went straight into an agent's account; want it refused")
	}

	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	ownerOf := func(w string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: w, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	}
	member := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "member"}
	type moved struct {
		Entry               economy.MoneyEntry `json:"entry"`
		CompanyAccountID    string             `json:"company_account_id"`
		CompanyBalanceMinor int64              `json:"company_balance_minor"`
		AgentBalanceMinor   int64              `json:"agent_balance_minor"`
	}
	move := func(as *auth.AuthContext, account, body string) (int, moved) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/money/accounts/"+account+"/funding", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), as))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out moved
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	entries := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind LIKE 'agent_funding%'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	code, give := move(ownerOf(ws), agentAcct.ID, `{"direction":"to_agent","amount_minor":1000,"idempotency_key":"give-1"}`)
	if code != http.StatusCreated || give.Entry.ID == "" || give.CompanyAccountID != company.ID || give.AgentBalanceMinor != 1000 || give.CompanyBalanceMinor != 4000 {
		t.Fatalf("give $10.00: %d %+v; want 201, the agent holding 1000 and the company 4000", code, give)
	}
	if code, again := move(ownerOf(ws), agentAcct.ID, `{"direction":"to_agent","amount_minor":1000,"idempotency_key":"give-1"}`); code != http.StatusCreated || again.Entry.ID != give.Entry.ID {
		t.Fatalf("the same key again: %d, entry %s; want 201 and the first entry %s", code, again.Entry.ID, give.Entry.ID)
	}
	if code, _ := move(ownerOf(ws), agentAcct.ID, `{"direction":"to_agent","amount_minor":4001,"idempotency_key":"give-2"}`); code != http.StatusConflict {
		t.Errorf("more than the company holds: %d; want 409", code)
	}
	if code, _ := move(ownerOf(ws), agentAcct.ID, `{"direction":"to_company","amount_minor":1001,"idempotency_key":"back-2"}`); code != http.StatusConflict {
		t.Errorf("taking back more than the agent holds: %d; want 409", code)
	}
	if code, _ := move(ownerOf(ws), agentAcct.ID, `{"direction":"to_agent","amount_minor":100,"idempotency_key":"live","funding":"live"}`); code != http.StatusForbidden {
		t.Errorf("live money before currency_accounts is cleared: %d; want 403", code)
	}
	if code, _ := move(member, agentAcct.ID, `{"direction":"to_agent","amount_minor":100,"idempotency_key":"member"}`); code != http.StatusForbidden {
		t.Errorf("a member who is not owner or admin: %d; want 403", code)
	}
	if code, _ := move(ownerOf(other), agentAcct.ID, `{"direction":"to_company","amount_minor":100,"idempotency_key":"theirs"}`); code != http.StatusNotFound {
		t.Errorf("another workspace's agent account: %d; want 404", code)
	}
	if code, _ := move(ownerOf(ws), company.ID, `{"direction":"to_agent","amount_minor":100,"idempotency_key":"company"}`); code != http.StatusNotFound {
		t.Errorf("the company's own account as the agent's: %d; want 404", code)
	}
	if n := entries(); n != 1 {
		t.Fatalf("%d funding entries after the refusals; want the one that moved", n)
	}
	code, back := move(ownerOf(ws), agentAcct.ID, `{"direction":"to_company","amount_minor":400,"idempotency_key":"back-1"}`)
	if code != http.StatusCreated || back.AgentBalanceMinor != 600 || back.CompanyBalanceMinor != 4400 {
		t.Fatalf("take $4.00 back: %d %+v; want 201, the agent holding 600 and the company 4400", code, back)
	}

	// Both statements: each move once, in its words, summing to zero between the two accounts.
	lines := func(account string) string {
		t.Helper()
		st, err := store.AccountStatement(ctx, ws, "", account, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range st.Lines {
			if strings.HasPrefix(l.Kind, "agent_funding") {
				out = append(out, l.Memo+" "+jsonOf(l.AmountMinor))
			}
		}
		return strings.Join(out, ", ")
	}
	if got, want := lines(company.ID), "From the company -1000, Back to the company 400"; got != want {
		t.Errorf("company statement: %s; want %s", got, want)
	}
	if got, want := lines(agentAcct.ID), "From the company 1000, Back to the company -400"; got != want {
		t.Errorf("agent statement: %s; want %s", got, want)
	}
	for account, want := range map[string]int64{company.ID: 4400, agentAcct.ID: 600} {
		if b, err := store.Balance(ctx, ws, account); err != nil || b.TestMinor != want || b.LiveMinor != 0 {
			t.Errorf("%s holds %d test and %d live cents (%v); want %d test", account, b.TestMinor, b.LiveMinor, err, want)
		}
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_chain WHERE workspace_id = $1 AND subject = $2 AND kind LIKE 'agent.agent_funding%'`,
		ws, "agent:"+agent.ID).Scan(&audits); err != nil || audits != 2 {
		t.Errorf("%d audit entries (%v); want one per move", audits, err)
	}
}
