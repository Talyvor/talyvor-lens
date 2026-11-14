package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B59.18 on real Postgres — the connector's instant_transfer_quote and instant_transfer_send (launch pack 4.5). A fiat or
// a stablecoin workspace is offered neither tool, and calling one answers unknown tool. A hybrid workspace's agent
// quotes, and a send through the connector only prepares the payment for the owner to approve: it leaves an approval
// and moves nothing — even to an account the agent has paid before, which its rules alone would let through. An agent
// without "Can use Instant transfer" is refused with that field.
func TestB5918_InstantTransferConnectorTools(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const sender, payee = "ws-b5918-sender", "ws-b5918-payee"
	kinds := map[string]string{sender: "hybrid", payee: "hybrid", "ws-b5918-fiat": "fiat", "ws-b5918-coin": "stablecoin"}
	for ws, kind := range kinds {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', $2)`,
			ws, kind); err != nil {
			t.Fatal(err)
		}
	}
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screening.NewScreener(pool, registry))
	store.SetFXMargin(func(context.Context, economy.FeeQuerier, string) (int64, string, error) { return 50, "team", nil })
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(registry, "b5918"), Limits: store.MoneyLimits(), Audit: store,
		Hooks: store.InstantTransferHooks(&partners.TestTransferHooks{})})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	agents := map[string]string{}
	for ws := range kinds {
		a, err := store.CreateAgentOfKind(ctx, ws, "clerk", "owner-"+ws, "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AttachAgentKey(ctx, ws, a.ID, "key-"+ws); err != nil {
			t.Fatal(err)
		}
		agents[ws] = a.ID
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)
	srv.SetInstantTransfers(store)

	rpc := func(ws, method, params string) (map[string]any, map[string]any) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-" + ws, WorkspaceID: ws}))
		rec := httptest.NewRecorder()
		srv.HandleRPC(rec, req)
		var resp struct {
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: %v: %s", method, err, rec.Body.String())
		}
		return resp.Result, resp.Error
	}
	listed := func(ws string) map[string]bool {
		t.Helper()
		res, rerr := rpc(ws, "tools/list", `{}`)
		if rerr != nil {
			t.Fatalf("tools/list for %s: %v", ws, rerr)
		}
		names := map[string]bool{}
		for _, tool := range res["tools"].([]any) {
			names[tool.(map[string]any)["name"].(string)] = true
		}
		return names
	}
	// call answers the tool's text and whether it is an error.
	call := func(ws, tool string, args any) (string, bool) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
		res, rerr := rpc(ws, "tools/call", string(raw))
		if rerr != nil {
			t.Fatalf("%s for %s: %v", tool, ws, rerr)
		}
		isErr, _ := res["isError"].(bool)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), isErr
	}

	// A fiat or stablecoin workspace has neither tool; a hybrid one has both.
	for _, ws := range []string{"ws-b5918-fiat", "ws-b5918-coin"} {
		names := listed(ws)
		if names["instant_transfer_quote"] || names["instant_transfer_send"] || !names["agent_balance"] {
			t.Errorf("%s workspace's tools/list: quote %v, send %v, agent_balance %v; want neither Instant transfer tool", kinds[ws],
				names["instant_transfer_quote"], names["instant_transfer_send"], names["agent_balance"])
		}
		if _, rerr := rpc(ws, "tools/call", `{"name":"instant_transfer_quote","arguments":{"send_minor":10000,"send_currency":"GBP","receive_currency":"EUR"}}`); rerr == nil ||
			!strings.Contains(rerr["message"].(string), "unknown tool") {
			t.Errorf("%s workspace calling instant_transfer_quote: %v, want unknown tool", kinds[ws], rerr)
		}
	}
	if names := listed(sender); !names["instant_transfer_quote"] || !names["instant_transfer_send"] {
		t.Fatalf("a hybrid workspace's tools/list: quote %v, send %v; want both", names["instant_transfer_quote"], names["instant_transfer_send"])
	}

	agent := agents[sender]
	if _, err := store.OpenCurrencyAccount(ctx, sender, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	from, err := store.OpenCurrencyAccount(ctx, sender, agent, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	var topUp string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, sender).Scan(&topUp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: sender, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: topUp, AmountMinor: -1_000_00}, {AccountID: from.ID, AmountMinor: 1_000_00}}}); err != nil {
		t.Fatal(err)
	}
	euros, err := store.OpenCurrencyAccount(ctx, payee, "", "EUR", "test")
	if err != nil {
		t.Fatal(err)
	}
	quoteArgs := map[string]any{"send_minor": 100_00, "send_currency": "GBP", "receive_currency": "EUR"}
	quote := func() economy.InstantQuote {
		t.Helper()
		text, isErr := call(sender, "instant_transfer_quote", quoteArgs)
		var q economy.InstantQuote
		if err := json.Unmarshal([]byte(text), &q); isErr || err != nil || q.ID == "" || q.ReceiveMinor <= 0 {
			t.Fatalf("instant_transfer_quote: %s", text)
		}
		return q
	}

	// An agent without the rule is refused with its field.
	text, isErr := call(sender, "instant_transfer_send", map[string]any{"quote_id": quote().ID, "to_account_id": euros.ID, "idempotency_key": "no-rule"})
	var refusal struct {
		Code  string `json:"code"`
		Field string `json:"field"`
	}
	if _ = json.Unmarshal([]byte(text), &refusal); !isErr || refusal.Code != "agent_rule" || refusal.Field != economy.FieldMayInstantTransfer {
		t.Fatalf("a send by an agent without the rule: %q, want refused with field %s", text, economy.FieldMayInstantTransfer)
	}
	on := true
	if _, err := store.SetAgentRules(ctx, sender, agent, economy.AgentRules{MayInstantTransfer: &on}); err != nil {
		t.Fatal(err)
	}

	// The agent pays the account once outside the connector, with the owner's approval, so its rules alone would let
	// the next one through.
	direct := func(key string) error {
		q, err := store.QuoteInstantTransfer(ctx, sender, economy.InstantQuoteRequest{SendMinor: 100_00, SendCurrency: "GBP", ReceiveCurrency: "EUR",
			To: economy.InstantToAccount})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.SendInstantTransfer(ctx, sender, agent, economy.InstantSend{QuoteID: q.ID, ToAccountID: euros.ID, IdempotencyKey: key})
		return err
	}
	var need *economy.ApprovalNeededError
	if err := direct("direct-first"); !errors.As(err, &need) {
		t.Fatalf("a first send to a new account: %v, want it to wait for the owner's approval", err)
	}
	if _, err := store.DecideAgentApproval(ctx, sender, need.ApprovalID, true); err != nil {
		t.Fatal(err)
	}
	if err := direct("direct-approved"); err != nil {
		t.Fatalf("the approved send: %v", err)
	}

	balance := func(account string) int64 {
		t.Helper()
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1`, account).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	entries := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE kind LIKE 'instant_transfer_%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sent, received, moved := balance(from.ID), balance(euros.ID), entries()
	if sent != 1_000_00-100_00 || received <= 0 || moved != 3 {
		t.Fatalf("after the approved send outside the connector: sender %d, payee %d, %d entries; want 90000, more than 0, 3", sent, received, moved)
	}

	// Through the connector the same send leaves an approval for the owner and moves nothing.
	text, isErr = call(sender, "instant_transfer_send", map[string]any{"quote_id": quote().ID, "to_account_id": euros.ID, "idempotency_key": "connector"})
	var prepared struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal([]byte(text), &prepared); err != nil || isErr || prepared.Status != "awaiting_approval" || prepared.ApprovalID == "" {
		t.Fatalf("a send through the connector: %q (error %v), want it awaiting the owner's approval", text, isErr)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1 AND agent_id = $2`, prepared.ApprovalID, agent).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("the connector's approval %s: %q %v, want pending", prepared.ApprovalID, status, err)
	}
	if balance(from.ID) != sent || balance(euros.ID) != received || entries() != moved {
		t.Fatalf("a send through the connector moved money: sender %d → %d, payee %d → %d, %d → %d entries",
			sent, balance(from.ID), received, balance(euros.ID), moved, entries())
	}
	var transfers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instant_transfers WHERE idempotency_key = 'connector'`).Scan(&transfers); err != nil || transfers != 0 {
		t.Fatalf("a send through the connector left %d transfers (%v), want none", transfers, err)
	}
}
