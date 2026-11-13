package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// B59.6 — a workspace is Business or Personal, and Fiat only, Stablecoin and crypto only, or Hybrid; its kind is the
// limit for its agents' wallet kinds, and changes only as launch pack 3.5 says.

// b596Bank is a store on pool with the sandbox money layer behind it, so agents' wallets open and take deposits.
func b596Bank(t *testing.T, pool *pgxpool.Pool, secret string) (*economy.DualTokenStore, *money.Registry) {
	t.Helper()
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, secret), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	return store, layer
}

// A new workspace is the type and kind it was made with, and its first agent follows the kind: a fiat workspace's
// first agent owns no crypto wallet. A later sign-in changes neither, and a type or kind outside the lists is refused.
func TestB596_NewWorkspaceIsItsTypeAndKindAndItsFirstAgentFollowsIt(t *testing.T) {
	mgr, pool := provisionManager(t)
	ctx := context.Background()
	bank, _ := b596Bank(t, pool, "whsec_b596_start")
	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, newWorkspaceStarter(bank, 0), newKeyFixture(t).mint, nil)
	stamp := time.Now().Format("150405.000000")

	for _, bad := range []string{`"account_type":"charity"`, `"money_kind":"gold"`} {
		if rec, _ := doProvision(t, r, provisionSecret, `{"identity":"b596-bad-`+stamp+`",`+bad+`}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("provision with %s = %d %s, want 400", bad, rec.Code, rec.Body.String())
		}
	}

	for _, c := range []struct {
		accountType, moneyKind, walletKind string
		assets                             string
	}{
		{"personal", "fiat", economy.WalletKindFiat, "EUR"},
		{"business", "stablecoin", economy.WalletKindCrypto, "USDC"},
		{"personal", "hybrid", economy.WalletKindBoth, "EUR,USDC"},
	} {
		body := fmt.Sprintf(`{"identity":"b596-%s-%s-%s","account_type":%q,"money_kind":%q}`, c.accountType, c.moneyKind, stamp,
			c.accountType, c.moneyKind)
		rec, out := doProvision(t, r, provisionSecret, body)
		if rec.Code != http.StatusOK || out.FirstAgent == nil {
			t.Fatalf("provision %s %s: %d %s", c.accountType, c.moneyKind, rec.Code, rec.Body.String())
		}
		acct, err := bank.WorkspaceAccount(ctx, out.WorkspaceID)
		if err != nil || acct.AccountType != c.accountType || acct.MoneyKind != c.moneyKind {
			t.Fatalf("workspace made %s %s reads %+v (%v)", c.accountType, c.moneyKind, acct, err)
		}
		var kind, assets string
		if err := pool.QueryRow(ctx, `SELECT a.wallet_kind, COALESCE((SELECT string_agg(m.currency, ',' ORDER BY m.currency) FROM money_accounts m
			WHERE m.agent_id = a.id AND m.purpose = 'agent' AND m.status = 'open'), '') FROM agent_accounts a WHERE a.id = $1`,
			out.FirstAgent.Agent.ID).Scan(&kind, &assets); err != nil {
			t.Fatal(err)
		}
		if kind != c.walletKind || assets != c.assets {
			t.Fatalf("a %s workspace's first agent is %s with wallets %q, want %s with %q", c.moneyKind, kind, assets, c.walletKind, c.assets)
		}

		// Signing in again, naming another type and kind, changes neither.
		other := strings.NewReplacer(`"personal"`, `"business"`, `"business"`, `"personal"`, `"fiat"`, `"hybrid"`, `"stablecoin"`, `"hybrid"`,
			`"hybrid"`, `"fiat"`).Replace(body)
		if rec, _ := doProvision(t, r, provisionSecret, other); rec.Code != http.StatusOK {
			t.Fatalf("second sign-in: %d %s", rec.Code, rec.Body.String())
		}
		if again, err := bank.WorkspaceAccount(ctx, out.WorkspaceID); err != nil || again != acct {
			t.Fatalf("after a second sign-in naming another type and kind the workspace reads %+v (%v), want %+v", again, err, acct)
		}
	}
}

// Every allowed and every refused change of kind, on real balances: fiat to stablecoin is refused; fiat becomes hybrid
// and its agents both; hybrid becomes fiat only once no stablecoin is held, refused naming the balance until then;
// hybrid becomes stablecoin once the fiat side is empty, closing the agents' fiat wallets; stablecoin becomes hybrid
// again. An agent of a kind the workspace does not allow is refused, made or changed. Each change is audited.
func TestB596_TheKindChangesOnlyAsTheSpecSays(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b596", "whsec_b596"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'fiat')`, ws); err != nil {
		t.Fatal(err)
	}
	store, layer := b596Bank(t, pool, secret)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountWorkspaceMoneyKindRoutes(r, store)
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	kindOf := func() string {
		t.Helper()
		code, out := call(http.MethodGet, "/v1/workspaces/"+ws+"/account", "")
		if code != http.StatusOK || out["account_type"] != "business" {
			t.Fatalf("GET account = %d %v", code, out)
		}
		return out["money_kind"].(string)
	}
	setKind := func(kind string) (int, map[string]any) {
		t.Helper()
		return call(http.MethodPut, "/v1/workspaces/"+ws+"/account/money-kind", `{"money_kind":"`+kind+`"}`)
	}
	refused := func(what string, code int, out map[string]any, field string) {
		t.Helper()
		if code != http.StatusConflict || out["code"] != "wrong_money_kind" || out["field"] != field {
			t.Fatalf("%s = %d %v, want 409 wrong_money_kind on %s", what, code, out, field)
		}
	}
	base := "/v1/workspaces/" + ws + "/agents"
	walletAssets := func(agentID string) string {
		t.Helper()
		var assets string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(currency, ',' ORDER BY currency), '') FROM money_accounts
			WHERE agent_id = $1 AND purpose = 'agent' AND status = 'open'`, agentID).Scan(&assets); err != nil {
			t.Fatal(err)
		}
		return assets
	}

	// A fiat workspace: its agents are fiat, by default and only.
	code, out := call(http.MethodPost, base, `{"name":"Trader","wallet_kind":"crypto"}`)
	refused("a crypto agent in a fiat workspace", code, out, "wallet_kind")
	code, out = call(http.MethodPost, base, `{"name":"Payroll"}`)
	if code != http.StatusCreated || out["wallet_kind"] != economy.WalletKindFiat {
		t.Fatalf("an agent made in a fiat workspace = %d %v, want fiat", code, out)
	}
	agent := out["id"].(string)
	if got := walletAssets(agent); got != "EUR" {
		t.Fatalf("the fiat agent's wallets = %q, want EUR", got)
	}
	code, out = call(http.MethodPut, base+"/"+agent+"/wallet-kind", `{"wallet_kind":"both"}`)
	refused("a fiat workspace's agent made both", code, out, "wallet_kind")

	// Fiat cannot become stablecoin directly; it can become hybrid at any time, and its agents then hold both.
	code, out = setKind(economy.MoneyKindStablecoin)
	refused("fiat to stablecoin", code, out, "money_kind")
	if code, out = setKind(economy.MoneyKindHybrid); code != http.StatusOK || out["money_kind"] != economy.MoneyKindHybrid || kindOf() != economy.MoneyKindHybrid {
		t.Fatalf("fiat to hybrid = %d %v", code, out)
	}
	if code, out = call(http.MethodGet, base+"/"+agent+"/wallets", ""); code != http.StatusOK || out["wallet_kind"] != economy.WalletKindBoth ||
		walletAssets(agent) != "EUR,USDC" {
		t.Fatalf("after fiat to hybrid the agent = %d %v with wallets %q, want both, EUR,USDC", code, out, walletAssets(agent))
	}

	// 1 USDC arrives at the agent's stablecoin wallet: hybrid cannot become fiat, and the refusal names the balance.
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE agent_id = $1 AND currency = 'USDC' AND purpose = 'agent'
		AND status = 'open'`, agent).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf(`{"id":"evt_b596","to":%q,"amount":{"amount_minor":1000000,"asset":"USDC"},"partner_ref":"test_in_b596","payer":"Acme"}`, ref))
	req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
	req.Header.Set(money.SandboxSignatureHeader, money.SignSandbox(secret, body, time.Now()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("deposit = %d %s", w.Code, w.Body.String())
	}
	code, out = setKind(economy.MoneyKindFiat)
	refused("hybrid to fiat with 1 USDC held", code, out, "money_kind")
	if held, _ := out["balances"].(map[string]any); held["USDC"] != float64(1_000_000) || out["money_kind"] != economy.MoneyKindHybrid {
		t.Fatalf("the refusal names %v as the workspace's kind and %v to empty, want hybrid and USDC 1000000", out["money_kind"], out["balances"])
	}
	var posted int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0) FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		WHERE a.agent_id = $1 AND a.purpose = 'agent' AND a.status = 'open' AND p.currency = 'USDC'`, agent).Scan(&posted); err != nil || posted != 1_000_000 {
		t.Fatalf("after the refused change the agent's open USDC wallet holds %d (%v), want 1000000", posted, err)
	}
	if kindOf() != economy.MoneyKindHybrid || walletAssets(agent) != "EUR,USDC" {
		t.Fatalf("after the refused change the workspace is %s and the agent holds %q, want hybrid and EUR,USDC", kindOf(), walletAssets(agent))
	}

	// The fiat side is empty: hybrid becomes stablecoin, and the agent's EUR wallet closes.
	if code, out = setKind(economy.MoneyKindStablecoin); code != http.StatusOK || kindOf() != economy.MoneyKindStablecoin {
		t.Fatalf("hybrid to stablecoin with no fiat held = %d %v", code, out)
	}
	if code, out = call(http.MethodGet, base+"/"+agent+"/wallets", ""); out["wallet_kind"] != economy.WalletKindCrypto || walletAssets(agent) != "USDC" {
		t.Fatalf("after hybrid to stablecoin the agent = %d %v with wallets %q, want crypto with USDC", code, out, walletAssets(agent))
	}
	code, out = call(http.MethodPost, base, `{"name":"Clerk","wallet_kind":"fiat"}`)
	refused("a fiat agent in a stablecoin workspace", code, out, "wallet_kind")

	// And stablecoin becomes hybrid again.
	if code, out = setKind(economy.MoneyKindHybrid); code != http.StatusOK || kindOf() != economy.MoneyKindHybrid {
		t.Fatalf("stablecoin to hybrid = %d %v", code, out)
	}
	if code, out = setKind("gold"); code != http.StatusBadRequest {
		t.Fatalf("a kind of gold = %d %v, want 400", code, out)
	}

	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_chain WHERE workspace_id = $1 AND subject = $2 AND kind = 'workspace.money_kind'`,
		ws, "workspace:"+ws).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("kind changes in the audit trail = %d (%v), want 3", audited, err)
	}
	var accountType string
	if err := pool.QueryRow(ctx, `SELECT account_type FROM workspaces WHERE id = $1`, ws).Scan(&accountType); err != nil || accountType != "business" {
		t.Fatalf("the type after every change = %q (%v), want business", accountType, err)
	}
}
