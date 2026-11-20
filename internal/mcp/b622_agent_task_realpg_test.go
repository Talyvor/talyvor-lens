package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B62.2 — the connector's pay tools take the agent's task: every one offers it, one over 200 characters is refused in
// words and pays nothing, and a payment from the agent's currency account keeps it, key-shaped strings cut, on its
// money out as the account's statement reads it.
func TestB622_APayToolKeepsTheAgentsTaskWithThePayment(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws = "ws-b622-mcp"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	a, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, a.ID, "key-b622"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", "USD", "test"); err != nil {
		t.Fatal(err)
	}
	acct, err := store.OpenCurrencyAccount(ctx, ws, a.ID, "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Grace Hopper", Country: "US", RoutingNumber: "021000021", AccountNumber: "123456789"})
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -10000}, {AccountID: acct.ID, AmountMinor: 10000}}}); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)
	rpc := func(method, params string) []byte {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-b622", WorkspaceID: ws}))
		w := httptest.NewRecorder()
		srv.HandleRPC(w, req)
		return w.Body.Bytes()
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]any `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rpc("tools/list", `{}`), &list); err != nil {
		t.Fatal(err)
	}
	offered := 0
	for _, tool := range list.Result.Tools {
		if _, ok := tool.InputSchema.Properties["task"]; ok != payTools[tool.Name] {
			t.Errorf("tool %s offers a task: %v, want %v", tool.Name, ok, payTools[tool.Name])
		} else if ok {
			offered++
		}
	}
	if offered < 6 { // instant_transfer_send is a hybrid workspace's alone
		t.Errorf("%d pay tools offer a task, want at least 6", offered)
	}

	pay := func(task, key string) (string, bool) {
		t.Helper()
		b, _ := json.Marshal(task)
		var resp struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		got := rpc("tools/call", `{"name":"wallet_pay_external","arguments":{"payee_id":"`+payee.ID+
			`","amount_minor":2500,"currency":"USD","reference":"Hosting","idempotency_key":"`+key+`","task":`+string(b)+`}}`)
		if json.Unmarshal(got, &resp) != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("wallet_pay_external = %s", got)
		}
		return resp.Result.Content[0].Text, resp.Result.IsError
	}
	statement := func() []economy.MoneyStatementLine {
		t.Helper()
		st, err := store.AccountStatement(ctx, ws, a.ID, acct.ID, time.Unix(0, 0), time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return st.Lines
	}

	if text, refused := pay(strings.Repeat("é", 201), "k-long"); !refused || !strings.Contains(text, "the task is 201 characters, and it may be at most 200") {
		t.Errorf("a task of 201 characters = %q (refused %v), want refused saying it may be at most 200", text, refused)
	}
	if n := len(statement()); n != 1 {
		t.Errorf("after a refused task the account has %d lines, want only its top-up", n)
	}
	if text, refused := pay("Paying the hosting bill so the demo stays up; key sk-abcdefghijklmnopqrstu", "k-paid"); refused {
		t.Fatalf("wallet_pay_external with a task = %q, refused", text)
	}
	const kept = "Paying the hosting bill so the demo stays up; key [REDACTED]"
	lines := statement()
	out := slices.DeleteFunc(slices.Clone(lines), func(l economy.MoneyStatementLine) bool { return l.AmountMinor >= 0 })
	if len(out) == 0 || slices.ContainsFunc(out, func(l economy.MoneyStatementLine) bool { return l.Task != kept }) {
		t.Errorf("money out = %+v, want every line of it to keep %q", out, kept)
	}
	if lines[0].AmountMinor <= 0 || lines[0].Task != "" {
		t.Errorf("the top-up line = %+v, want money in with no task", lines[0])
	}
}
