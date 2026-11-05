package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B52.4 — an assistant connected through Claude or ChatGPT pays from one agent's wallet on a Connect token the BFF
// mints: inside the agent's rules the payment executes once, above its approval amount it waits for the owner, and
// every route that approves, changes rules, issues keys or cards refuses the token.
func TestConnectToken_PaysInsideTheAgentsRulesAndNothingElse(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, mintKey = "ws-connect", "test-mint-key-long-enough-for-connect"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	am := auth.NewManager(mintTestGlobalKey, jwtKey, auth.New(pool), nil).WithMintKey(mintKey)
	r := chi.NewRouter()
	r.Use(auth.AuthMiddleware(auth.New(pool), am))
	r.Post("/v1/auth/connect-token", newConnectTokenMintHandler(am, store))
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCardRoutes(r, store, &fakeCardIssuer{})
	call := func(bearer, method, path, body, idem string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	newAgent := func(name string, fund int64) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		if fund > 0 {
			if _, err := store.FundAgent(ctx, ws, a.ID, fund); err != nil {
				t.Fatal(err)
			}
		}
		return a.ID
	}
	buyer, seller := newAgent("buyer", 10_000_000), newAgent("seller", 0)
	if _, err := store.SetAgentRules(ctx, ws, buyer, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}

	mint := `{"workspace_id":"` + ws + `","user_id":"user-owner","client_id":"https://claude.ai","agent_id":"` + buyer + `","scopes":["payments:send"]}`
	if code, body := call(mintKey, http.MethodPost, "/v1/auth/connect-token", strings.Replace(mint, `"payments:send"`, `"admin"`, 1), ""); code != http.StatusBadRequest {
		t.Errorf("a Connect token asking for admin was minted: %d %s, want 400", code, body)
	}
	if code, body := call(mintKey, http.MethodPost, "/v1/auth/connect-token", strings.Replace(mint, "user-owner", "user-other", 1), ""); code != http.StatusForbidden {
		t.Errorf("a Connect token for someone else's agent was minted: %d %s, want 403", code, body)
	}
	code, body := call(mintKey, http.MethodPost, "/v1/auth/connect-token", mint, "")
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &minted); code != http.StatusCreated || err != nil || minted.Token == "" {
		t.Fatalf("mint a Connect token = %d %s", code, body)
	}
	tok := minted.Token
	payPath := "/v1/workspaces/" + ws + "/agents/" + buyer + "/pay"
	postings := func() (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE kind = 'pay' AND account = $1`, "agent:"+buyer).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Inside the rules: executes, and a retry with the same key lands once.
	for range 2 {
		if code, body := call(tok, http.MethodPost, payPath, `{"to_agent_id":"`+seller+`","amount_ulxc":500000}`, "connect-pay-1"); code != http.StatusOK {
			t.Fatalf("a 0.5 LXC payment inside the rules = %d %s, want 200", code, body)
		}
	}
	var paid int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE kind = 'pay' AND account = $1`, "agent:"+seller).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if n := postings(); n != 1 || paid != 500_000 {
		t.Errorf("the buyer has %d pay postings and the seller was paid %d µLXC — want one payment of 500000", n, paid)
	}

	// Above the approval amount: a pending approval, and nothing moves.
	code, body = call(tok, http.MethodPost, payPath, `{"to_agent_id":"`+seller+`","amount_ulxc":3000000,"memo":"invoice 19"}`, "connect-pay-2")
	var pending struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal([]byte(body), &pending); code != http.StatusAccepted || err != nil ||
		pending.Status != "pending_approval" || !strings.HasPrefix(pending.ApprovalID, "apr_") {
		t.Fatalf("a 3 LXC payment above the 1 LXC approval amount = %d %s, want 202 naming the pending approval", code, body)
	}
	if n := postings(); n != 1 {
		t.Errorf("the payment waiting for approval moved money: %d pay postings, want 1", n)
	}

	// The token never approves, changes rules, issues keys or cards, or pays from another agent.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/workspaces/" + ws + "/agents/approvals/" + pending.ApprovalID + "/approve", `{}`},
		{http.MethodPut, "/v1/workspaces/" + ws + "/agents/" + buyer + "/rules", `{"approval_above_ulxc":100000000}`},
		{http.MethodPost, "/v1/workspaces/" + ws + "/agents/" + buyer + "/keys", `{}`},
		{http.MethodPost, "/v1/workspaces/" + ws + "/agents/" + buyer + "/card", `{}`},
		{http.MethodPost, "/v1/workspaces/" + ws + "/agents/" + seller + "/pay", `{"to_agent_id":"` + buyer + `","amount_ulxc":1}`},
		{http.MethodPost, "/v1/auth/connect-token", mint},
	} {
		if code, body := call(tok, c.method, c.path, c.body, ""); code != http.StatusForbidden {
			t.Errorf("%s %s with a Connect token = %d %s, want 403", c.method, c.path, code, body)
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, pending.ApprovalID).Scan(&status); err != nil || status != "pending" {
		t.Errorf("the approval is %q (%v) after the Connect token tried to approve it, want pending", status, err)
	}
	if n := postings(); n != 1 {
		t.Errorf("the refused calls moved money: %d pay postings, want 1", n)
	}
}
