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

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// B48.1 — through the sandbox adapter: an agent is fiat, crypto or both, and owns one wallet of each kind it is, each
// with receive details of its own at the partner (shown only once a live partner serves them, B49.1); the owner changes its kind, and a kind whose wallet still holds money is not removed;
// the owner has a wallet of each asset their agents use.
func TestB481_AgentsAreFiatCryptoOrBothAndEachOwnsItsWallets(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b481", "whsec_b481"
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
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code
	}
	base := "/v1/workspaces/" + ws + "/agents"
	create := func(body string) economy.Agent {
		t.Helper()
		var a economy.Agent
		if code := call(http.MethodPost, base, body, &a); code != http.StatusCreated {
			t.Fatalf("create %s = %d", body, code)
		}
		return a
	}
	wallets := func(agentID string) economy.AgentWallets {
		t.Helper()
		var out economy.AgentWallets
		if code := call(http.MethodGet, base+"/"+agentID+"/wallets", "", &out); code != http.StatusOK {
			t.Fatalf("GET %s wallets = %d", agentID, code)
		}
		return out
	}
	assets := func(ws economy.AgentWallets) string {
		var s []string
		for _, w := range ws.Wallets {
			s = append(s, w.Asset)
		}
		return strings.Join(s, ",")
	}

	if code := call(http.MethodPost, base, `{"name":"Odd","wallet_kind":"gold"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("an agent of kind gold = %d, want 400", code)
	}
	fiat := create(`{"name":"Payroll","wallet_kind":"fiat","fiat_currency":"GBP"}`)
	crypto := create(`{"name":"Trader","wallet_kind":"crypto"}`)
	both := create(`{"name":"Ops","wallet_kind":"both"}`)
	if fiat.WalletKind != economy.WalletKindFiat || crypto.WalletKind != economy.WalletKindCrypto || both.WalletKind != economy.WalletKindBoth {
		t.Fatalf("kinds = %q %q %q", fiat.WalletKind, crypto.WalletKind, both.WalletKind)
	}

	seen := map[string]string{}
	own := func(agentID string, w economy.AgentWallet) {
		t.Helper()
		if w.AgentID != agentID || w.Mode != economy.DetailsTest || w.Receive != (money.Details{}) || w.PaymentReference != "" {
			t.Fatalf("%s's %s wallet = %+v; want a TEST wallet showing no made-up details", agentID, w.Asset, w)
		}
		// Its receive details are its own at the partner; the sandbox's reach nothing, so they are read there, never shown.
		var ref string
		if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE id = $1`, w.AccountID).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		c := economy.CapabilityCurrencyAccounts
		if w.Asset == money.USDC {
			c = economy.CapabilityStablecoins
		}
		at, err := layer.Wallets(ctx, money.Route{Workspace: ws, Agent: agentID, Capability: c})
		if err != nil {
			t.Fatal(err)
		}
		d, err := at.Receive(ctx, ref)
		key := d.Address + d.IBAN + d.AccountNumber
		if err != nil || key == "" {
			t.Fatalf("%s's %s wallet has no receive details of its own at the partner: %+v (%v)", agentID, w.Asset, d, err)
		}
		if other, ok := seen[key]; ok {
			t.Fatalf("%s's %s wallet pays into the same details as %s's", agentID, w.Asset, other)
		}
		seen[key] = agentID
	}
	for _, c := range []struct {
		agent economy.Agent
		want  string
	}{{fiat, "GBP"}, {crypto, "USDC"}, {both, "EUR,USDC"}} {
		got := wallets(c.agent.ID)
		if assets(got) != c.want {
			t.Fatalf("%s (%s) has wallets %s, want %s", c.agent.Name, c.agent.WalletKind, assets(got), c.want)
		}
		for _, w := range got.Wallets {
			own(c.agent.ID, w)
		}
	}
	var accounts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_accounts WHERE workspace_id = $1 AND purpose = 'agent' AND status = 'open'`, ws).Scan(&accounts); err != nil || accounts != 4 {
		t.Fatalf("open agent wallets in the ledger = %d (%v), want 4", accounts, err)
	}

	// 1 USDC arrives at Ops's crypto wallet: Ops can no longer stop being crypto.
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE agent_id = $1 AND currency = 'USDC' AND purpose = 'agent'`, both.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf(`{"id":"evt_b481","to":%q,"amount":{"amount_minor":1000000,"asset":"USDC"},"partner_ref":"test_in_b481","payer":"Acme"}`, ref))
	req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
	req.Header.Set(money.SandboxSignatureHeader, money.SignSandbox(secret, body, time.Now()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("deposit = %d %s", w.Code, w.Body.String())
	}
	if code := call(http.MethodPut, base+"/"+both.ID+"/wallet-kind", `{"wallet_kind":"fiat"}`, nil); code != http.StatusConflict {
		t.Fatalf("removing crypto from Ops with 1 USDC in its wallet = %d, want 409", code)
	}
	var posted int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0) FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		WHERE a.agent_id = $1 AND a.purpose = 'agent' AND a.status = 'open' AND p.currency = 'USDC'`, both.ID).Scan(&posted); err != nil || posted != 1_000_000 {
		t.Fatalf("after the refused change Ops's open USDC wallet holds %d (%v), want 1000000", posted, err)
	}
	if got := wallets(both.ID); got.WalletKind != economy.WalletKindBoth || assets(got) != "EUR,USDC" {
		t.Fatalf("after the refused change Ops = %s %s, want both EUR,USDC", got.WalletKind, assets(got))
	}

	// Its empty fiat wallet goes; Trader adds fiat and gets a wallet of its own.
	var changed economy.AgentWallets
	if code := call(http.MethodPut, base+"/"+both.ID+"/wallet-kind", `{"wallet_kind":"crypto"}`, &changed); code != http.StatusOK ||
		changed.WalletKind != economy.WalletKindCrypto || assets(changed) != "USDC" {
		t.Fatalf("Ops to crypto = %d %s %s", code, changed.WalletKind, assets(changed))
	}
	if code := call(http.MethodPut, base+"/"+crypto.ID+"/wallet-kind", `{"wallet_kind":"both","fiat_currency":"USD"}`, &changed); code != http.StatusOK ||
		assets(changed) != "USD,USDC" {
		t.Fatalf("Trader to both = %d %s", code, assets(changed))
	}
	own(crypto.ID, changed.Wallets[0])
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_chain WHERE workspace_id = $1 AND kind = 'agent.wallet_kind'`, ws).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("wallet-kind changes in the audit trail = %d (%v), want 2", audited, err)
	}

	// The owner's wallets: one of each asset their agents use — GBP, USD and USDC now that Ops's EUR wallet is gone.
	var ownerWallets economy.AgentWallets
	if code := call(http.MethodGet, base+"/owner-wallets", "", &ownerWallets); code != http.StatusOK || assets(ownerWallets) != "GBP,USD,USDC" {
		t.Fatalf("owner wallets = %d %s, want GBP,USD,USDC", code, assets(ownerWallets))
	}
	for _, w := range ownerWallets.Wallets {
		if w.AgentID != "" || w.AccountID == "" || w.Mode != economy.DetailsTest || w.Receive != (money.Details{}) {
			t.Fatalf("owner's %s wallet = %+v; want the workspace's, showing no made-up details", w.Asset, w)
		}
	}
}
