package main

import (
	"context"
	"encoding/json"
	"errors"
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

// B55.18 — a decline keeps its reason (up to 200 characters, from the single or the batch route) and holds for the
// approval life only; a different request asks at once, and a retry racing the owner's decline sees the decline.
func TestB5518_ADeclineSaysWhyAndHoldsForTheApprovalLife(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5518-life"
	store, ag := b501Agents(t, pool, ws)
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/approvals/"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	pay := func(amount int64, memo string) error {
		_, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, amount, memo)
		return err
	}
	rows := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE agent_id = $1`, ag[0].ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := pay(5_000_000, "march"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Fatalf("the first request: %v, want ErrApprovalRequired", err)
	}
	march := b5018Pending(t, pool, ag[0].ID, "march")
	if code, body := call(march+"/deny", `{"reason": "`+strings.Repeat("x", 201)+`"}`); code != http.StatusBadRequest {
		t.Errorf("a 201-character reason: %d %s, want 400", code, body)
	}
	if code, body := call(march+"/approve", `{"reason": "fine"}`); code != http.StatusBadRequest {
		t.Errorf("a reason with approve: %d %s, want 400", code, body)
	}
	// From the batch route, as Approvals' "decline all" sends it.
	if code, body := call("decide", `{"approval_ids": ["`+march+`"], "decision": "deny", "reason": "not this month"}`); code != http.StatusOK {
		t.Fatalf("decline with a reason: %d %s", code, body)
	}
	list, err := store.ListAgentApprovals(ctx, ws)
	if err != nil || len(list) != 1 || list[0].Status != "denied" || list[0].DenyReason != "not this month" {
		t.Errorf("the list = %+v (%v), want the declined approval with its reason", list, err)
	}
	if err := pay(5_000_000, "march"); !errors.Is(err, economy.ErrApprovalDeclined) || !errors.Is(err, economy.ErrAgentRule) ||
		!strings.Contains(err.Error(), ": not this month; the same request is refused until ") {
		t.Errorf("the identical retry: %v, want ErrApprovalDeclined naming the reason", err)
	}
	if err := pay(6_000_000, "march"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Errorf("a different amount: %v, want ErrApprovalRequired at once", err)
	}
	if n := rows(); n != 2 {
		t.Errorf("approvals = %d, want 2: the declined one and the different amount's", n)
	}

	// A retry racing the owner's decline: it waits for the decline, then sees it, and files nothing.
	if err := pay(5_000_000, "may"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Fatal(err)
	}
	may := b5018Pending(t, pool, ag[0].ID, "may")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_approvals SET status = 'denied', decided_at = now(), expires_at = now() + interval '1 day',
		deny_reason = 'racing' WHERE id = $1`, may); err != nil {
		t.Fatal(err)
	}
	retried := make(chan error, 1)
	go func() { retried <- pay(5_000_000, "may") }()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-retried; !errors.Is(err, economy.ErrApprovalDeclined) || !strings.Contains(err.Error(), ": racing;") {
		t.Errorf("the retry racing the decline: %v, want ErrApprovalDeclined", err)
	}
	if n := rows(); n != 3 {
		t.Errorf("approvals after the racing retry = %d, want 3: nothing filed", n)
	}

	// With the approval life set to 1 second, the identical retry after it files a fresh approval.
	store.SetApprovalTTL(time.Second)
	if err := pay(5_000_000, "april"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Fatal(err)
	}
	april := b5018Pending(t, pool, ag[0].ID, "april")
	if code, body := call(april+"/deny", ``); code != http.StatusOK {
		t.Fatalf("decline without a reason: %d %s", code, body)
	}
	if err := pay(5_000_000, "april"); !errors.Is(err, economy.ErrApprovalDeclined) || strings.Contains(err.Error(), ": ;") {
		t.Errorf("the retry within the second: %v, want ErrApprovalDeclined with no empty reason", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := pay(5_000_000, "april"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Errorf("the retry after the approval's life: %v, want ErrApprovalRequired", err)
	}
	if fresh := b5018Pending(t, pool, ag[0].ID, "april"); fresh == april {
		t.Error("no fresh approval was filed after the decline's life")
	}
	if n, _ := b501Postings(t, pool, "agent:"+ag[1].ID); n != 0 {
		t.Errorf("the payee's postings = %d, want none", n)
	}
	b501ZeroSum(t, pool)
}
