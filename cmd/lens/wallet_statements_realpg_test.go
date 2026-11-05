package main

import (
	"context"
	"encoding/csv"
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
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// B48.14 through the routes on the real schema: a scripted month — money in from a customer, a move to an agent, a
// card charge and Talyvor's fee — gives a statement whose every closing balance is the account's balance in the
// ledger, as JSON, CSV and PDF; the activity lists it newest first; and the accountant's export finds the one real
// disposal among the movements on the chain and its gain. (Its chain side against a node is in TestB486_.)
func TestB4814_WalletHistoryStatementsAndTaxExport(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4814"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	agent, err := store.CreateAgent(ctx, ws, "treasurer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateAgent(ctx, ws, "buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	open := func(agentID, purpose string) string {
		a, err := store.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: ws, AgentID: agentID, Currency: economy.CurrencyUSD, Purpose: purpose, Name: purpose})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	company, agentUSD, partner, revenue := open("", economy.MoneyCompany), open(agent.ID, economy.MoneyAgent), open("", economy.MoneyPartner), open("", economy.MoneyRevenue)
	post := func(key, kind, counterparty, to, from string, minor int64) {
		t.Helper()
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityCurrencyAccounts, Kind: kind,
			IdempotencyKey: key, Funding: economy.FundingTest, Counterparty: counterparty, Postings: []economy.MoneyPosting{
				{AccountID: to, AmountMinor: minor, Currency: economy.CurrencyUSD}, {AccountID: from, AmountMinor: -minor, Currency: economy.CurrencyUSD}}}); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	post("in", "payment_in", "Acme Ltd", company, partner, 100000)
	mid := time.Now().UTC()
	post("fund", "agent_fund", "", agentUSD, company, 30000)
	post("card", "card_charge", "Cloud Co", partner, agentUSD, 4550)
	post("fee", "platform_fee", "", revenue, agentUSD, 100)

	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	get := func(who *auth.AuthContext, path string, into any) (int, *httptest.ResponseRecorder) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if into != nil {
			_ = json.Unmarshal(w.Body.Bytes(), into)
		}
		return w.Code, w
	}
	base := "/v1/workspaces/" + ws

	// Each closing balance is the ledger's; partner mirrors and Talyvor's fees are not the owner's money.
	ledger := func(account string) int64 {
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1`, account).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	var st economy.WalletStatement
	if code, _ := get(owner, base+"/wallet/statement", &st); code != http.StatusOK {
		t.Fatalf("the owner's statement = %d", code)
	}
	closing := map[string]int64{}
	for _, a := range st.Accounts {
		closing[a.AccountID] = a.ClosingMinor
	}
	if len(st.Accounts) != 2 || closing[company] != 70000 || closing[agentUSD] != 25350 || closing[company] != ledger(company) ||
		closing[agentUSD] != ledger(agentUSD) || len(st.Lines) != 5 {
		t.Fatalf("the owner's statement = %+v; want company 70000 and the agent 25350, as the ledger, by 5 lines", st)
	}
	var mine economy.WalletStatement
	if code, _ := get(owner, base+"/agents/"+agent.ID+"/wallet/statement", &mine); code != http.StatusOK || len(mine.Accounts) != 1 ||
		mine.Accounts[0].ClosingMinor != 25350 || len(mine.Lines) != 3 || mine.Lines[2].BalanceAfterMinor != 25350 {
		t.Fatalf("the agent's statement = %d %+v; want its own account, 3 lines, closing 25350", code, mine)
	}
	// Two periods meet: what the first closes at, the second opens at.
	first, err := store.WalletStatement(ctx, ws, "", mid.Add(-time.Hour), mid)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.WalletStatement(ctx, ws, "", mid, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for i := range first.Accounts {
		if a, b := first.Accounts[i], second.Accounts[i]; a.AccountID != b.AccountID || a.ClosingMinor != b.OpeningMinor {
			t.Fatalf("%s closes the first period at %d and opens the next at %d", a.AccountID, a.ClosingMinor, b.OpeningMinor)
		}
	}

	var act struct{ Lines []economy.WalletLine }
	if code, _ := get(owner, base+"/wallet/activity?limit=2", &act); code != http.StatusOK || len(act.Lines) != 2 ||
		act.Lines[0].Kind != "platform_fee" || act.Lines[0].BalanceAfterMinor != 25350 || act.Lines[1].Counterparty != "Cloud Co" ||
		act.Lines[1].BalanceAfterMinor != 25450 {
		t.Fatalf("the activity = %d %+v; want the fee, then the card charge to Cloud Co, each with the balance after it", code, act.Lines)
	}
	if code, _ := get(proxyKey, base+"/wallet/activity", nil); code != http.StatusForbidden {
		t.Fatalf("an agent's key read the owner's activity: %d, want 403", code)
	}
	if code, _ := get(owner, base+"/agents/ag_nobody/wallet/statement", nil); code != http.StatusNotFound {
		t.Fatalf("an agent not in the workspace: %d, want 404", code)
	}

	_, w := get(owner, base+"/wallet/statement?format=csv", nil)
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil || w.Header().Get("Content-Type") != "text/csv; charset=utf-8" || len(rows) != 1+2+5+2 ||
		rows[len(rows)-1][6] != "closing" || rows[len(rows)-1][12] != fmt.Sprint(closing[rows[len(rows)-1][3]]) {
		t.Fatalf("the CSV statement = %v, %v", rows, err)
	}
	_, w = get(owner, base+"/wallet/statement?format=pdf", nil)
	if w.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(w.Body.String(), "%PDF-") ||
		!strings.Contains(w.Body.String(), "Cloud Co") {
		t.Fatalf("the PDF statement: %q, %.40q", w.Header().Get("Content-Type"), w.Body.String())
	}

	// The accountant's export: of six movements on the chain only one is a disposal — not the reversed one, the one
	// not yet confirmed, the stablecoin, or the move to the owner's other agent.
	const eth = 1_000_000_000_000_000_000
	wallets := map[string]string{agent.ID: "0x00000000000000000000000000000000000b4814", other.ID: "0x00000000000000000000000000000000000b4815"}
	for id, addr := range wallets {
		if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
			owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x', '0x', 'cred', $4, '0x', 'cdp', '')`,
			id, ws, addr, make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
	}
	for i, m := range []struct {
		dir, to, asset, status string
		qty, value             int64
	}{
		{"in", "0xabc", "ETH", "final", 2 * eth, 600000},
		{"out", "0xdef", "ETH", "confirmed", eth, 400000},
		{"out", "0xdef", "ETH", "reversed", eth / 2, 999999},
		{"out", "0xdef", "ETH", "seen", eth / 2, 999999},
		{"out", "0xdef", "USDC", "final", 5_000000, 500},
		{"out", wallets[other.ID], "ETH", "final", eth / 2, 150000},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO chain_movements (id, workspace_id, agent_id, network, tx_hash, log_position, direction, wallet,
			counterparty, token, asset, amount_minor, funding, source, status, value_minor, value_currency, seen_at)
			VALUES ($1, $2, $3, 'base', $4, 0, $5, $6, $7, '0x', $8, $9, 'test', 'webhook', $10, $11, 'USD', now() - make_interval(hours => 10 - $12))`,
			fmt.Sprintf("cm-%d", i), ws, agent.ID, fmt.Sprintf("0x%064x", i), m.dir, wallets[agent.ID], m.to, m.asset, m.qty, m.status, m.value, i); err != nil {
			t.Fatal(err)
		}
	}
	var x economy.TaxExport
	if code, _ := get(owner, base+"/wallet/tax", &x); code != http.StatusOK || x.Currency != "USD" || len(x.Disposals) != 1 ||
		x.Disposals[0].ID != "cm-1" || x.Disposals[0].CostBasisMinor != 300000 || x.GainMinor != 100000 {
		t.Fatalf("the tax export = %d %+v; want cm-1, half the lot's 600000 as its basis, a gain of 100000 USD", code, x)
	}
	_, w = get(owner, base+"/wallet/tax?format=csv", nil)
	if rows, err := csv.NewReader(w.Body).ReadAll(); err != nil || len(rows) != 3 || rows[1][0] != "disposal" || rows[2][0] != "total" || rows[2][10] != "100000" {
		t.Fatalf("the tax CSV = %v, %v", rows, err)
	}
}
