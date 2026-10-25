package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// B46.4 — through the sandbox adapter, wired as main.go wires it: the owner creates an agent and it has a USDC wallet
// on Base with an address to pay into; money the sandbox reports arriving at that address posts to the wallet in the
// ledger once, and the owner's GET …/wallet and the agent's own agent_balance tool show the same balance; money out of
// the wallet above the agent's approval amount moves nothing and files an approval for its owner.
func TestB464_ANewAgentHasAWalletItsBalanceAgreesAndBigPaymentsWaitForApproval(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b464", "whsec_b464"
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
	mcpServer := mcp.New(pool, nil, nil, nil, nil, "test")
	mcpServer.SetAgentBank(store)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	r.Post("/mcp", mcpServer.HandleRPC)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(who *auth.AuthContext, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
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

	var agent economy.Agent
	if code := call(owner, http.MethodPost, base, `{"name":"Buyer"}`, &agent); code != http.StatusCreated {
		t.Fatalf("create the agent = %d", code)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE agent_id = $1 AND currency = 'USDC' AND purpose = 'agent'`,
		agent.ID).Scan(&ref); err != nil {
		t.Fatalf("the new agent's wallet in the ledger: %v", err)
	}
	var wallet economy.AgentWallet
	if code := call(owner, http.MethodGet, base+"/"+agent.ID+"/wallet", "", &wallet); code != http.StatusOK {
		t.Fatalf("GET wallet = %d", code)
	}
	if wallet.Asset != money.USDC || wallet.Partner != money.SandboxName || wallet.Receive.Network != money.SandboxNetwork ||
		!strings.HasPrefix(wallet.Receive.Address, "0x") || wallet.Mode != economy.DetailsTest || wallet.BalanceMinor != 0 {
		t.Fatalf("a new agent's wallet = %+v; want an empty sandbox USDC wallet with an address on Base, marked TEST", wallet)
	}

	// 2.5 USDC arrive at the wallet's address, reported by the sandbox twice.
	body := []byte(fmt.Sprintf(`{"id":"evt_b464","to":%q,"amount":{"amount_minor":2500000,"asset":"USDC"},"partner_ref":"test_in_b464","payer":"Acme Supplies Ltd"}`, ref))
	for i, want := range []int{http.StatusOK, http.StatusConflict} {
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
		req.Header.Set(money.SandboxSignatureHeader, money.SignSandbox(secret, body, time.Now()))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("deposit %d = %d %s, want %d", i+1, w.Code, w.Body.String(), want)
		}
	}
	var posted int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0) FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		WHERE a.agent_id = $1 AND a.purpose = 'agent' AND p.currency = 'USDC' AND p.funding = 'test'`, agent.ID).Scan(&posted); err != nil || posted != 2_500_000 {
		t.Fatalf("the wallet's ledger postings sum to %d (%v), want 2500000 test USDC posted once", posted, err)
	}
	if code := call(owner, http.MethodGet, base+"/"+agent.ID+"/wallet", "", &wallet); code != http.StatusOK || wallet.BalanceMinor != 2_500_000 {
		t.Fatalf("GET wallet = %d %+v; want a balance of 2500000", code, wallet)
	}

	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-b464"); err != nil {
		t.Fatal(err)
	}
	agentKey := &auth.AuthContext{WorkspaceID: ws, APIKeyID: "key-b464"}
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if code := call(agentKey, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_balance","arguments":{}}}`, &rpc); code != http.StatusOK ||
		rpc.Result.IsError || len(rpc.Result.Content) == 0 {
		t.Fatalf("agent_balance = %d %+v", code, rpc)
	}
	var balance struct {
		Wallet economy.AgentWallet `json:"wallet"`
	}
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &balance); err != nil || balance.Wallet != wallet {
		t.Fatalf("agent_balance's wallet = %+v (%v); want the owner's view %+v", balance.Wallet, err, wallet)
	}

	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	transfers, err := layer.Transfers(ctx, money.Route{Workspace: ws, Agent: agent.ID, Capability: economy.CapabilityStablecoins})
	if err != nil {
		t.Fatal(err)
	}
	_, err = transfers.Outbound(ctx, money.Outbound{Key: "pay-b464", From: ref, Amount: money.Amount{Minor: 2_000_000, Asset: money.USDC},
		Payee: money.Payee{Name: "Hosting Co", Network: money.SandboxNetwork, Address: "0x00000000000000000000000000000000000b4640"}})
	if !errors.Is(err, money.ErrApprovalNeeded) {
		t.Fatalf("2 USDC out, above the approval amount = %v; want ErrApprovalNeeded", err)
	}
	approvals, err := store.ListAgentApprovals(ctx, ws)
	if err != nil || len(approvals) != 1 || approvals[0].AgentID != agent.ID || approvals[0].Status != "pending" {
		t.Fatalf("approvals = %+v (%v); want one pending for the agent", approvals, err)
	}
	if code := call(owner, http.MethodGet, base+"/"+agent.ID+"/wallet", "", &wallet); code != http.StatusOK || wallet.BalanceMinor != 2_500_000 {
		t.Fatalf("after the held payment the wallet = %+v; want it untouched at 2500000", wallet)
	}
}
