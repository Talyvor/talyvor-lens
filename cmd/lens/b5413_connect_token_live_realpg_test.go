package main

import (
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
	"github.com/talyvor/lens/internal/tenant"
)

// B54.13 — a Connect token stops paying the moment the person disconnects the assistant or the agent changes owner,
// pays only with an Idempotency-Key, and leaves at most economy.ConnectApprovalsWaiting approvals waiting.
func TestConnectToken_StopsWhenTheConnectionOrOwnerChanges(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, mintKey, plainMintKey = "ws-connect-live", "test-connect-mint-key-long-enough", "test-mint-key-long-enough-for-docs"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	am := auth.NewManager(mintTestGlobalKey, jwtKey, auth.New(pool), nil).WithMintKey(plainMintKey).WithConnectMintKey(mintKey)
	r := chi.NewRouter()
	r.Use(auth.AuthMiddleware(auth.New(pool), am))
	r.Post("/v1/auth/connect-token", newConnectTokenMintHandler(am, store))
	r.Post("/v1/auth/connect-revocations", newConnectRevocationHandler(am, store))
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	call := func(bearer, path, body, idem string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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
	mintToken := func() string {
		t.Helper()
		code, body := call(mintKey, "/v1/auth/connect-token",
			`{"workspace_id":"`+ws+`","user_id":"user-owner","client_id":"https://claude.ai","agent_id":"`+buyer+`","scopes":["payments:send"]}`, "")
		var minted struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(body), &minted); code != http.StatusCreated || err != nil || minted.Token == "" {
			t.Fatalf("mint a Connect token = %d %s", code, body)
		}
		return minted.Token
	}
	payPath := "/v1/workspaces/" + ws + "/agents/" + buyer + "/pay"
	small := `{"to_agent_id":"` + seller + `","amount_ulxc":100000}`
	postings := func() (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE kind = 'pay' AND account = $1`, "agent:"+buyer).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tok := mintToken()

	// Without an Idempotency-Key a Connect payment is refused and nothing moves; with one it pays.
	if code, body := call(tok, payPath, small, ""); code != http.StatusBadRequest {
		t.Errorf("a Connect payment without an Idempotency-Key = %d %s, want 400", code, body)
	}
	if n := postings(); n != 0 {
		t.Errorf("the payment without an Idempotency-Key moved money: %d pay postings, want 0", n)
	}
	if code, body := call(tok, payPath, small, "live-1"); code != http.StatusOK {
		t.Fatalf("a Connect payment with an Idempotency-Key = %d %s, want 200", code, body)
	}

	// Five payments above the approval amount wait; a retry of one still answers its approval; the sixth is refused.
	above := func(i int) string {
		return fmt.Sprintf(`{"to_agent_id":"%s","amount_ulxc":3000000,"memo":"invoice %d"}`, seller, i)
	}
	for i := 1; i <= economy.ConnectApprovalsWaiting; i++ {
		if code, body := call(tok, payPath, above(i), fmt.Sprintf("above-%d", i)); code != http.StatusAccepted {
			t.Fatalf("waiting approval %d = %d %s, want 202", i, code, body)
		}
	}
	if code, body := call(tok, payPath, above(1), "above-1"); code != http.StatusAccepted {
		t.Errorf("a retry of a payment already waiting = %d %s, want 202", code, body)
	}
	if code, body := call(tok, payPath, above(6), "above-6"); code != http.StatusTooManyRequests {
		t.Errorf("the sixth waiting approval = %d %s, want 429", code, body)
	}
	var waiting int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE agent_id = $1 AND status = 'pending'`, buyer).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if waiting != economy.ConnectApprovalsWaiting {
		t.Errorf("%d approvals wait, want %d", waiting, economy.ConnectApprovalsWaiting)
	}

	// The person disconnects the assistant: the token it holds stops at once, and only the Connect mint credential records it.
	revoke := `{"user_id":"user-owner","client_id":"https://claude.ai"}`
	if code, body := call(plainMintKey, "/v1/auth/connect-revocations", revoke, ""); code != http.StatusForbidden {
		t.Errorf("the Docs and Track mint key recorded a revocation: %d %s, want 403", code, body)
	}
	if code, body := call(mintKey, "/v1/auth/connect-revocations", revoke, ""); code != http.StatusCreated {
		t.Fatalf("record the disconnection = %d %s, want 201", code, body)
	}
	if code, body := call(tok, payPath, small, "live-2"); code != http.StatusForbidden {
		t.Errorf("a payment with a disconnected assistant's token = %d %s, want 403", code, body)
	}
	if n := postings(); n != 1 {
		t.Errorf("the disconnected assistant moved money: %d pay postings, want 1", n)
	}

	// Connected again, a fresh token pays; then the agent changes owner and that token stops at once.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second))) // a token's issued-at is whole seconds
	tok = mintToken()
	if code, body := call(tok, payPath, small, "live-3"); code != http.StatusOK {
		t.Fatalf("a payment after connecting again = %d %s, want 200", code, body)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_accounts SET owner_user_id = 'user-new-owner' WHERE id = $1`, buyer); err != nil {
		t.Fatal(err)
	}
	if code, body := call(tok, payPath, small, "live-4"); code != http.StatusForbidden {
		t.Errorf("a payment after the agent changed owner = %d %s, want 403", code, body)
	}
	if n := postings(); n != 2 {
		t.Errorf("the token outlived the agent's owner: %d pay postings, want 2", n)
	}
}
