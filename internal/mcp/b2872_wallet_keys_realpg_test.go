package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B28.72 — every wallet tool that moves credits or asks for them takes an idempotency_key: a request, an accepted
// request, an escrow and a move into a pot, each sent twice with one key, write one row and one posting; the same key
// with another amount is refused and writes nothing.
func TestB2872_EveryWalletToolSentTwiceWithOneKeyWritesOneRowAndOnePosting(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetOwnerVerifier(walletVerified{})
	const ws, lxc = "co-keys", int64(1_000_000)
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
	pot, err := store.CreatePot(ctx, ws, payer.ID, "Rainy day", "reserve", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	asked, err := store.RequestCredits(ctx, ws, payee.ID, payer.ID, 3*lxc, "design")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAreas(everyArea{})
	srv.SetAgentBank(store)

	call := func(tool, args string) (string, bool) {
		t.Helper()
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
			t.Fatalf("%s: no tool result: %s", tool, w.Body.String())
		}
		return resp.Result.Content[0].Text, resp.Result.IsError
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	releaseAt := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		tool, args, other string
		rows, postings    string // the rows the tool writes, and its postings on the payer's account
	}{
		{"wallet_request", `{"from":"` + payee.ID + `","amount_ulxc":2000000,"memo":"lunch","idempotency_key":"req-1"}`,
			`{"from":"` + payee.ID + `","amount_ulxc":9000000,"memo":"lunch","idempotency_key":"req-1"}`,
			`SELECT count(*) FROM agent_money_requests WHERE from_agent_id = '` + payer.ID + `'`, ""},
		{"wallet_answer_request", `{"request_id":"` + asked.ID + `","accept":true,"idempotency_key":"accept-1"}`, "",
			`SELECT count(*) FROM agent_transfers WHERE request_id = '` + asked.ID + `'`,
			`SELECT count(*) FROM agent_postings WHERE account = 'agent:` + payer.ID + `' AND kind = 'transfer'`},
		{"wallet_escrow_pay", `{"to":"` + payee.ID + `","amount_ulxc":4000000,"release_at":"` + releaseAt + `","memo":"logo","idempotency_key":"escrow-1"}`,
			`{"to":"` + payee.ID + `","amount_ulxc":8000000,"release_at":"` + releaseAt + `","memo":"logo","idempotency_key":"escrow-1"}`,
			`SELECT count(*) FROM agent_escrows WHERE payer_agent_id = '` + payer.ID + `'`,
			`SELECT count(*) FROM agent_postings WHERE account = 'agent:` + payer.ID + `' AND kind = 'escrow'`},
		{"wallet_pot_move", `{"pot_id":"` + pot.ID + `","amount_ulxc":5000000,"direction":"in","idempotency_key":"pot-1"}`, "",
			`SELECT count(*) FROM agent_postings WHERE account = 'pot:` + pot.ID + `'`,
			`SELECT count(*) FROM agent_postings WHERE account = 'agent:` + payer.ID + `' AND kind = 'pot_in'`},
	} {
		for i := range 2 {
			if text, refused := call(tc.tool, tc.args); refused {
				t.Fatalf("%s, call %d with one key = %s, want it answered", tc.tool, i+1, text)
			}
		}
		if n := count(tc.rows); n != 1 {
			t.Errorf("%s sent twice with one key wrote %d rows, want 1", tc.tool, n)
		}
		if tc.postings != "" {
			if n := count(tc.postings); n != 1 {
				t.Errorf("%s sent twice with one key wrote %d postings on the payer's account, want 1", tc.tool, n)
			}
		}
		if tc.other != "" {
			if text, refused := call(tc.tool, tc.other); !refused || !strings.Contains(text, "idempotency key") {
				t.Errorf("%s with the same key and another amount = %s, want refused", tc.tool, text)
			}
			if n := count(tc.rows); n != 1 {
				t.Errorf("%s: the same key with another amount wrote a row: %d, want 1", tc.tool, n)
			}
		}
	}
	// What the ledger says the payer has left: 100 LXC less one accepted request, one escrow and one move into the pot.
	var left int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE account = $1`,
		"agent:"+payer.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if want := (100 - 3 - 4 - 5) * lxc; left != want {
		t.Errorf("the payer's postings sum to %d µLXC, want %d", left, want)
	}
}
