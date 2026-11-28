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

// B28.73 — the owner tools over MCP: the owner, signed in, creates an agent (a workspace key cannot: B19.11); the
// owner's key asks to fund it, reads it and sets its rules. Asking to fund answers an approval link and posts nothing;
// an agent's own key is refused every owner tool. Funding is checked on agent_postings.
func TestB2873_TheOwnerCreatesAsksToFundRulesAndReadsAgentsOverMCP(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	const ws = "ws-owner-tools"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, company) VALUES ($1, $1, $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAreas(everyArea{})
	srv.SetAgentBank(store)
	srv.SetOwnerBank(store)
	owner := &auth.AuthContext{APIKeyID: "key-owner", WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, Scopes: []string{auth.ScopeAdmin}}
	call := func(as *auth.AuthContext, tool, args string) (string, bool) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), as))
		w := httptest.NewRecorder()
		srv.HandleRPC(w, req)
		var resp struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(w.Body.Bytes(), &resp) != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("%s: no tool result: %s", tool, w.Body.String())
		}
		return resp.Result.Content[0].Text, resp.Result.IsError
	}

	if text, isErr := call(owner, "owner_create_agent", `{"name":"researcher"}`); !isErr || !strings.Contains(text, "an agent needs an owner") {
		t.Fatalf("owner_create_agent on a workspace key = %s (isError %v), want refused: every agent is a person's (B19.11)", text, isErr)
	}
	person := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-owner", Scopes: []string{auth.ScopeKeys}}
	text, isErr := call(person, "owner_create_agent", `{"name":"researcher"}`)
	var agent economy.Agent
	if isErr || json.Unmarshal([]byte(text), &agent) != nil || agent.ID == "" || agent.Name != "researcher" {
		t.Fatalf("owner_create_agent = %s (isError %v), want the new agent", text, isErr)
	}
	text, isErr = call(owner, "owner_fund_agent", `{"agent_id":"`+agent.ID+`","amount_ulxc":4000000,"idempotency_key":"mcp-fund-1"}`)
	var filed economy.AgentFundRequest
	if isErr || json.Unmarshal([]byte(text), &filed) != nil || filed.Status != economy.FundRequestPending ||
		filed.ApprovalURL != economy.FundRequestLinkBase+agent.ID+"?fund_request="+filed.ID {
		t.Fatalf("owner_fund_agent = %s (isError %v), want a pending request with its approval link", text, isErr)
	}
	var posted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1`, ws).Scan(&posted); err != nil || posted != 0 {
		t.Fatalf("postings after owner_fund_agent = %d (%v), want none: the link is not used yet", posted, err)
	}
	if text, isErr = call(owner, "owner_fund_request", `{"request_id":"`+filed.ID+`"}`); isErr || !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("owner_fund_request = %s (isError %v), want it pending", text, isErr)
	}
	if text, isErr = call(owner, "owner_propose_rules", `{"agent_id":"`+agent.ID+`","rules":{"max_per_request_ulxc":1000000}}`); isErr ||
		!strings.Contains(text, `"status":"applied"`) {
		t.Fatalf("owner_propose_rules tightening = %s (isError %v), want applied", text, isErr)
	}
	if text, isErr = call(owner, "owner_agent", `{"agent_id":"`+agent.ID+`"}`); isErr || !strings.Contains(text, `"max_per_request_ulxc":1000000`) {
		t.Fatalf("owner_agent = %s (isError %v), want its page with the rules set", text, isErr)
	}

	// An agent's own key is not its owner.
	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-agent"); err != nil {
		t.Fatal(err)
	}
	agentKey := &auth.AuthContext{APIKeyID: "key-agent", WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, Scopes: []string{auth.ScopeProxy}}
	if text, isErr = call(agentKey, "owner_fund_agent", `{"agent_id":"`+agent.ID+`","amount_ulxc":4000000,"idempotency_key":"mcp-fund-2"}`); !isErr {
		t.Fatalf("owner_fund_agent on an agent's key = %s, want refused", text)
	}
}
