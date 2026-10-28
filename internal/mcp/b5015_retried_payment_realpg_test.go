package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B50.15 — an agent's payment through MCP, retried with its idempotency_key, is paid once: agent_pay and wallet_send
// sent 20 times at once with one key move the money once, the same key with another amount is refused and moves
// nothing, and without a key each call is its own payment.
func TestB5015_ARetriedMCPPaymentIsPaidOnce(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetOwnerVerifier(walletVerified{})
	const ws, lxc = "co-retry", int64(1_000_000)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreditLXC(ctx, ws, 100*lxc, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
		t.Fatal(err)
	}
	payer, err := store.CreateAgent(ctx, ws, "payer", "owner-"+ws)
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "payee", "owner-"+ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, payer.ID, 100*lxc); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, payer.ID, "key-payer"); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)

	// call answers the tool's text and whether it was refused; "" text for a protocol failure.
	call := func(tool, args string) (string, bool) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-payer", WorkspaceID: ws}))
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
			return "", true
		}
		return resp.Result.Content[0].Text, resp.Result.IsError
	}
	// paid is what the ledger says the payer has paid out: every posting on its account after its funding.
	paid := func() int64 {
		t.Helper()
		var out int64
		if err := pool.QueryRow(ctx, `SELECT 100000000 - COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE account = $1`,
			"agent:"+payer.ID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	for _, tc := range []struct{ tool, to string }{
		{"agent_pay", `"to_agent_id":"` + payee.ID + `"`},
		{"wallet_send", `"to":"` + payee.ID + `"`},
	} {
		before := paid()
		args := func(amount, key string) string {
			a := `{` + tc.to + `,"amount_ulxc":` + amount + `,"memo":"invoice 9"`
			if key != "" {
				a += `,"idempotency_key":"` + key + `"`
			}
			return a + `}`
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var answers []string
		start := make(chan struct{})
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				text, refused := call(tc.tool, args("1000000", tc.tool+"-retry-1"))
				if refused {
					mu.Lock()
					answers = append(answers, text)
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(answers) != 0 {
			t.Errorf("%s: %d of 20 retries were refused, want each answered as the payment made: %q", tc.tool, len(answers), answers[0])
		}
		if got := paid() - before; got != lxc {
			t.Fatalf("%s sent 20 times at once with one key paid %d µLXC, want 1,000,000 once", tc.tool, got)
		}
		if text, refused := call(tc.tool, args("2000000", tc.tool+"-retry-1")); !refused || !strings.Contains(text, "idempotency key") {
			t.Errorf("%s with the same key and another amount = %s, want refused", tc.tool, text)
		}
		if got := paid() - before; got != lxc {
			t.Fatalf("%s: the same key with another amount moved money: paid %d µLXC, want 1,000,000", tc.tool, got)
		}
		for range 2 {
			if text, refused := call(tc.tool, args("1000000", "")); refused {
				t.Fatalf("%s without a key = %s", tc.tool, text)
			}
		}
		if got := paid() - before; got != 3*lxc {
			t.Fatalf("%s: two calls without a key paid %d µLXC in all, want each its own payment (3,000,000)", tc.tool, got)
		}
	}
}
