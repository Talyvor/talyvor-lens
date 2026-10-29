package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B41.105 — an agent reads its own safe-to-spend figure with agent_balance: 100 LXC less a 30 LXC payment waiting for
// approval is 70 LXC.
func TestAgentTools_AgentBalanceShowsWhatIsSafeToSpend(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws = "ws-agent-safe"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 100000000, 100000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	buyer, err := store.CreateAgent(ctx, ws, "buyer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	seller, err := store.CreateAgent(ctx, ws, "seller", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, buyer.ID, "key-safe"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, buyer.ID, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer.ID, economy.AgentRules{ApprovalAboveULXC: 10_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestPaymentApproval(ctx, ws, buyer.ID, seller.ID, 30_000_000, "", "a large order"); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_balance","arguments":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-safe", WorkspaceID: ws}))
	w := httptest.NewRecorder()
	srv.HandleRPC(w, req)
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Result.Content) == 0 {
		t.Fatalf("agent_balance = %s", w.Body.String())
	}
	var out struct {
		SafeToSpend []economy.SafeToSpendFigure `json:"safe_to_spend"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	want := []economy.SafeToSpendFigure{{Currency: "LXC", Balance: 100_000_000, PendingApprovals: 30_000_000, SafeToSpend: 70_000_000}}
	if len(out.SafeToSpend) != 1 || out.SafeToSpend[0] != want[0] {
		t.Errorf("agent_balance safe_to_spend = %+v, want %+v", out.SafeToSpend, want)
	}
}
