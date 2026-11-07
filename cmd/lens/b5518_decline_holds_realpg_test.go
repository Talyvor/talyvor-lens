package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// b5518Pusher is a browser's push service: it counts the pushes sent.
type b5518Pusher struct{ sent atomic.Int64 }

func (p *b5518Pusher) Send(context.Context, string, string, string, []byte) error {
	p.sent.Add(1)
	return nil
}
func (p *b5518Pusher) Allows(string) bool { return true }
func (p *b5518Pusher) PublicKey() string  { return "" }

// B55.18 — once the owner declines, the agent's identical request is refused naming the owner's reason, with no fresh
// approval and no push, on the agent's pay route (403) and on its MCP agent_request_approval tool alike.
func TestB5518_ADeclinedRequestIsNotAskedAgain(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5518"
	store, ag := b501Agents(t, pool, ws)
	pusher := &b5518Pusher{}
	store.SetApprovalAuth(passkey.RelyingParty{}, pusher)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_push_subscriptions (endpoint, workspace_id, p256dh, auth) VALUES ($1, $2, 'k', 'a')`,
		"https://fcm.googleapis.com/b5518", ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, ag[0].ID, "key-b5518"); err != nil {
		t.Fatal(err)
	}
	mcpServer := mcp.New(pool, nil, nil, nil, nil, "test")
	mcpServer.SetAgentBank(store)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	r.Post("/mcp", mcpServer.HandleRPC)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-owner", Scopes: []string{auth.ScopeKeys}}
	agent := &auth.AuthContext{WorkspaceID: ws, APIKeyID: "key-b5518"}
	call := func(who *auth.AuthContext, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	pay := func(amount int64, memo string) (int, string) {
		return call(agent, "/v1/workspaces/"+ws+"/agents/"+ag[0].ID+"/pay", `{"to_agent_id": "`+ag[1].ID+`", "amount_ulxc": `+
			strconv.FormatInt(amount, 10)+`, "memo": "`+memo+`"}`)
	}
	ask := func(memo string) (isError bool, text string) {
		t.Helper()
		_, body := call(agent, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_request_approval","arguments":`+
			`{"to_agent_id":"`+ag[1].ID+`","amount_ulxc":7000000,"memo":"`+memo+`","reason":"a large order"}}}`)
		var rpc struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &rpc); err != nil || len(rpc.Result.Content) == 0 {
			t.Fatalf("agent_request_approval = %s", body)
		}
		return rpc.Result.IsError, rpc.Result.Content[0].Text
	}
	deny := func(id, body string) {
		t.Helper()
		if code, out := call(owner, "/v1/workspaces/"+ws+"/agents/approvals/"+id+"/deny", body); code != http.StatusOK {
			t.Fatalf("decline %s: %d %s", id, code, out)
		}
	}
	rows := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE agent_id = $1`, ag[0].ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	pushes := func(want int64) {
		t.Helper()
		time.Sleep(300 * time.Millisecond) // notifyApproval pushes in the background
		if got := pusher.sent.Load(); got != want {
			t.Errorf("pushes sent = %d, want %d", got, want)
		}
	}

	// The agent's pay route: the owner declines, and the identical retry is refused without asking again.
	if code, body := pay(5_000_000, "march"); code != http.StatusForbidden {
		t.Fatalf("the first payment: %d %s, want 403 waiting for approval", code, body)
	}
	pushes(1)
	deny(b5018Pending(t, pool, ag[0].ID, "march"), `{"reason": "not this month"}`)
	code, body := pay(5_000_000, "march")
	if code != http.StatusForbidden || !strings.Contains(body, "the workspace's owner declined this request on ") ||
		!strings.Contains(body, ": not this month; the same request is refused until ") || strings.Contains(body, "spending rules") {
		t.Errorf("the identical retry: %d %s, want 403 naming the owner's decline and reason, not the spending rules", code, body)
	}
	if n := rows(); n != 1 {
		t.Errorf("approvals after the retry = %d, want 1: nothing filed", n)
	}
	pushes(1)

	// The agent's MCP tool that asks for approval: the same.
	if isErr, text := ask("april"); isErr {
		t.Fatalf("agent_request_approval: %s", text)
	}
	pushes(2)
	deny(b5018Pending(t, pool, ag[0].ID, "april"), `{}`)
	if isErr, text := ask("april"); !isErr || !strings.Contains(text, "the workspace's owner declined this request on ") {
		t.Errorf("agent_request_approval after the decline: error %v, %s; want the owner's decline", isErr, text)
	}
	if n := rows(); n != 2 {
		t.Errorf("approvals after the asking again = %d, want 2: nothing filed", n)
	}
	pushes(2)
}
