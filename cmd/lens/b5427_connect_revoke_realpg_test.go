package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B54.27 — signing out everywhere stops every assistant's token for that person and nobody else's, and ending a
// connection expires the approvals its assistant left waiting.
func TestConnectRevoke_SignOutEverywhereAndApprovals(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, mintKey = "ws-connect-revoke", "test-connect-mint-key-long-enough"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 50000000, 50000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	am := auth.NewManager(mintTestGlobalKey, jwtKey, auth.New(pool), nil).WithConnectMintKey(mintKey)
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
	newAgent := func(name, owner string, fund int64) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, owner)
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
	alice, bob, seller := newAgent("alice's", "user-alice", 10_000_000), newAgent("bob's", "user-bob", 10_000_000), newAgent("seller", "user-seller", 0)
	if _, err := store.SetAgentRules(ctx, ws, alice, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	mint := func(user, client, agent string) string {
		t.Helper()
		code, body := call(mintKey, "/v1/auth/connect-token",
			`{"workspace_id":"`+ws+`","user_id":"`+user+`","client_id":"`+client+`","agent_id":"`+agent+`","scopes":["payments:send"]}`, "")
		var minted struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(body), &minted); code != http.StatusCreated || err != nil || minted.Token == "" {
			t.Fatalf("mint a Connect token = %d %s", code, body)
		}
		return minted.Token
	}
	pay := func(tok, agent, body, idem string) int {
		t.Helper()
		code, _ := call(tok, "/v1/workspaces/"+ws+"/agents/"+agent+"/pay", body, idem)
		return code
	}
	paid := func(agent string) (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE kind = 'pay' AND account = $1`, "agent:"+agent).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	pending := func(client string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE agent_id = $1 AND connect_client_id = $2 AND status = 'pending'`,
			alice, client).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	small := `{"to_agent_id":"` + seller + `","amount_ulxc":100000}`
	above := func(memo string) string { // a different memo each, or the second joins the first's approval
		return `{"to_agent_id":"` + seller + `","amount_ulxc":3000000,"memo":"` + memo + `"}`
	}

	// Alice connects Claude and ChatGPT, Bob connects Claude; each assistant leaves one approval waiting on Alice's agent.
	aliceClaude, aliceGPT, bobClaude := mint("user-alice", "https://claude.ai", alice), mint("user-alice", "https://chatgpt.com", alice), mint("user-bob", "https://claude.ai", bob)
	if code := pay(aliceClaude, alice, above("claude"), "a-claude-above"); code != http.StatusAccepted {
		t.Fatalf("Claude's payment above the approval amount = %d, want 202", code)
	}
	if code := pay(aliceGPT, alice, above("chatgpt"), "a-gpt-above"); code != http.StatusAccepted {
		t.Fatalf("ChatGPT's payment above the approval amount = %d, want 202", code)
	}

	// Alice disconnects ChatGPT alone: its approval expires, Claude's still waits.
	if code, body := call(mintKey, "/v1/auth/connect-revocations", `{"user_id":"user-alice","client_id":"https://chatgpt.com"}`, ""); code != http.StatusCreated {
		t.Fatalf("disconnect ChatGPT = %d %s", code, body)
	}
	if n := pending("https://chatgpt.com"); n != 0 {
		t.Errorf("%d of ChatGPT's approvals still wait after it was disconnected, want 0", n)
	}
	if n := pending("https://claude.ai"); n != 1 {
		t.Errorf("%d of Claude's approvals wait after only ChatGPT was disconnected, want 1", n)
	}

	// Alice signs out everywhere: every assistant's token of hers stops and every approval they left expires; Bob's still pays.
	if code, body := call(mintKey, "/v1/auth/connect-revocations", `{"user_id":"user-alice"}`, ""); code != http.StatusCreated {
		t.Fatalf("sign out everywhere = %d %s", code, body)
	}
	if code := pay(aliceClaude, alice, small, "a-claude-after"); code != http.StatusForbidden {
		t.Errorf("Alice's Claude token after she signed out everywhere = %d, want 403", code)
	}
	if code := pay(aliceGPT, alice, small, "a-gpt-after"); code != http.StatusForbidden {
		t.Errorf("Alice's ChatGPT token after she signed out everywhere = %d, want 403", code)
	}
	if n := paid(alice); n != 0 {
		t.Errorf("Alice's agent paid after she signed out everywhere: %d pay postings, want 0", n)
	}
	if n := pending("https://claude.ai"); n != 0 {
		t.Errorf("%d of Claude's approvals still wait after Alice signed out everywhere, want 0", n)
	}
	if code := pay(bobClaude, bob, small, "b-claude-after"); code != http.StatusOK {
		t.Errorf("Bob's Claude token after Alice signed out everywhere = %d, want 200", code)
	}
	if n := paid(bob); n != 1 {
		t.Errorf("Bob's agent: %d pay postings after Alice signed out everywhere, want 1", n)
	}

	// A token's issued-at is the database's clock, the one a revocation is recorded on.
	var dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	claims := &auth.TokenClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(mint("user-bob", "https://chatgpt.com", bob), claims); err != nil || claims.IssuedAt == nil {
		t.Fatalf("read the minted token: %v", err)
	}
	if d := claims.IssuedAt.Sub(dbNow.Truncate(time.Second)); d < 0 || d > 5*time.Second {
		t.Errorf("the token's issued-at %v is %v from the database's clock", claims.IssuedAt.Time, d)
	}
}

// B54.27 — a revocation the database refuses answers a plain 500 without its text.
func TestConnectRevoke_ErrorHidesTheDatabase(t *testing.T) {
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	am := auth.NewManager(mintTestGlobalKey, jwtKey, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/connect-revocations", strings.NewReader(`{"user_id":"u1"}`))
	req.Header.Set("Authorization", "Bearer "+mintTestGlobalKey)
	w := httptest.NewRecorder()
	newConnectRevocationHandler(am, failingRevoker{}).ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "connect_revocations") {
		t.Errorf("a failed revocation = %d %s, want 500 without the database's text", w.Code, w.Body.String())
	}
}

type failingRevoker struct{}

func (failingRevoker) RevokeConnect(context.Context, string, string) error {
	return errors.New(`ERROR: relation "connect_revocations" does not exist (SQLSTATE 42P01)`)
}
