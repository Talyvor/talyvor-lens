package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

type walletVerified struct{}

func (walletVerified) MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT earn_verified FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&ok)
	return ok, err
}

// B22.11 — an agent of company A, using only the MCP tools with its own key and test money, sends credits,
// requests some, takes a loan company B offered it, pays into escrow, fills a pot and places a simulated order;
// each is refused when its rules or its class say no — a send and an escrow over its limit per request, a loan
// offered to a private user (RED), a withdrawal from a locked pot, a live order (RED). The money is on the
// ledger, and every call is logged.
func TestWalletTools_AnAgentUsesEveryWalletCapabilityThroughMCP(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetOwnerVerifier(walletVerified{})
	const lxc = int64(1_000_000)
	for ws, company := range map[string]bool{"co-a": true, "co-b": true, "person": false} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, company) VALUES ($1, $1, $1, true, $2)`, ws, company); err != nil {
			t.Fatal(err)
		}
	}
	agent := func(ws, handle string, credits int64) economy.Agent {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, handle, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetAgentHandle(ctx, ws, a.ID, handle); err != nil {
			t.Fatal(err)
		}
		if credits > 0 {
			if _, err := store.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FundAgent(ctx, ws, a.ID, credits); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	a := agent("co-a", "alpha", 200*lxc)
	b := agent("co-b", "bravo", 500*lxc)
	agent("person", "papa", 0)
	if err := store.AttachAgentKey(ctx, "co-a", a.ID, "key-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, "co-a", a.ID, economy.AgentRules{MaxPerRequestULXC: 50 * lxc}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES (current_date, 'USD', 1.1), (current_date, 'GBP', 0.85)`); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAreas(everyArea{})
	srv.SetAgentBank(store)

	calls, refusals := 0, 0
	call := func(tool, args string, wantRefused bool) map[string]any {
		t.Helper()
		calls++
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-a", WorkspaceID: "co-a"}))
		w := httptest.NewRecorder()
		srv.HandleRPC(w, req)
		var resp struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error any `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Error != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("%s %s = %s", tool, args, w.Body.String())
		}
		text := resp.Result.Content[0].Text
		if resp.Result.IsError != wantRefused {
			t.Fatalf("%s %s: refused %v, want %v: %s", tool, args, resp.Result.IsError, wantRefused, text)
		}
		if wantRefused {
			refusals++
			return map[string]any{"refusal": text}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%s: %s", tool, text)
		}
		return out
	}
	holds := func(ws string, ag economy.Agent) int64 {
		t.Helper()
		var bal int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = $1 AND account = $2`,
			ws, "agent:"+ag.ID).Scan(&bal); err != nil {
			t.Fatal(err)
		}
		return bal
	}

	// Send, within its rules and not beyond them.
	call("wallet_send", `{"to":"@bravo","amount_ulxc":10000000,"memo":"hosting"}`, false)
	if r := call("wallet_send", `{"to":"@bravo","amount_ulxc":60000000}`, true); !strings.Contains(r["refusal"].(string), "per request") {
		t.Errorf("a send over the limit per request was refused for %q", r["refusal"])
	}
	// Request.
	if r := call("wallet_request", `{"from":"@bravo","amount_ulxc":5000000,"memo":"refund of the deposit"}`, false); r["status"] != "pending" {
		t.Errorf("request = %v", r)
	}
	// Take a loan company B offers; offering one to a private user is class RED.
	offer, err := store.OfferLoan(ctx, "co-b", b.ID, "@alpha", economy.LoanTerms{PrincipalULXC: 30 * lxc, InterestBPS: 500, Instalments: 3, Every: "week"})
	if err != nil {
		t.Fatal(err)
	}
	if l := call("wallet_answer_loan", `{"loan_id":"`+offer.ID+`","accept":true}`, false); l["status"] != "active" {
		t.Errorf("accepting the loan = %v", l)
	}
	if r := call("wallet_offer_loan", `{"to":"@papa","principal_ulxc":1000000,"instalments":1,"every":"month"}`, true); !strings.Contains(r["refusal"].(string), "class RED") {
		t.Errorf("a loan to a private user was refused for %q, want class RED", r["refusal"])
	}
	// Pay into escrow, within its rules and not beyond them.
	release := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if e := call("wallet_escrow_pay", `{"to":"@bravo","amount_ulxc":20000000,"release_at":"`+release+`","memo":"logo"}`, false); e["status"] != "held" {
		t.Errorf("escrow = %v", e)
	}
	call("wallet_escrow_pay", `{"to":"@bravo","amount_ulxc":60000000,"release_at":"`+release+`"}`, true)
	// Fill a pot; a locked one refuses a withdrawal.
	pot := call("wallet_pot_create", `{"name":"reserve","kind":"reserve","locked_until":"`+release+`"}`, false)
	call("wallet_pot_move", `{"pot_id":"`+pot["id"].(string)+`","amount_ulxc":10000000,"direction":"in"}`, false)
	call("wallet_pot_move", `{"pot_id":"`+pot["id"].(string)+`","amount_ulxc":5000000,"direction":"out"}`, true)
	// Place a simulated order; a live one is class RED.
	pf := call("wallet_portfolio_open", `{"name":"fx","cash_uusd":1000000000}`, false)
	if o := call("wallet_order", `{"portfolio_id":"`+pf["id"].(string)+`","instrument":"EUR","side":"buy","type":"market","quantity_micros":100000000}`, false); o["status"] != "filled" || o["simulated"] != true {
		t.Errorf("order = %v", o)
	}
	if r := call("wallet_order", `{"portfolio_id":"`+pf["id"].(string)+`","instrument":"EUR","side":"buy","type":"market","quantity_micros":1000000,"mode":"live"}`, true); !strings.Contains(r["refusal"].(string), "class RED") {
		t.Errorf("a live order was refused for %q, want class RED", r["refusal"])
	}

	// On the ledger: A sent 10, borrowed 30, paid 20 into escrow and put 10 in its pot; B lent 30 and received 10.
	if holds("co-a", a) != 190*lxc || holds("co-b", b) != 480*lxc {
		t.Errorf("A holds %d, B %d; want 190 and 480 LXC", holds("co-a", a), holds("co-b", b))
	}
	var logged, refused int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE outcome = 'refused') FROM agent_tool_calls
		WHERE scoped_key_id = 'key-a' AND agent_id = $1`, a.ID).Scan(&logged, &refused); err != nil || logged != calls || refused != refusals {
		t.Errorf("logged %d calls (%d refused), %v; want %d (%d)", logged, refused, err, calls, refusals)
	}
}
