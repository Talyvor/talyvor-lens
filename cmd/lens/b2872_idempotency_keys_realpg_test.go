package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.72 — every money route takes an Idempotency-Key: a request, an owner's accept, an escrow, a cash-out, a schedule
// and a standing order, each sent twice with one key, write one row and one posting; the same key with another amount
// is answered 409 and writes nothing.
func TestB2872_EveryMoneyRouteSentTwiceWithOneKeyWritesOneRowAndOnePosting(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b2872-keys"
	store, ag := b501Agents(t, pool, ws)
	store.SetCashOutPartner(economy.TestCashOutPartner{})
	asked, err := store.RequestCredits(ctx, ws, ag[1].ID, ag[0].ID, 3_000_000, "design")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentTransferRoutes(r, store)
	mountAgentEscrowRoutes(r, store)
	mountCashOutRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-owner", Scopes: []string{auth.ScopeKeys}}
	post := func(path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	agentPath := "/v1/workspaces/" + ws + "/agents/" + ag[0].ID
	releaseAt := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct {
		name, path, body, other string
		rows, postings          string // the rows the route writes, and its postings on the payer's account
	}{
		{"request", agentPath + "/requests", fmt.Sprintf(`{"from":%q,"amount_ulxc":2000000,"memo":"lunch"}`, ag[1].ID),
			fmt.Sprintf(`{"from":%q,"amount_ulxc":9000000,"memo":"lunch"}`, ag[1].ID),
			`SELECT count(*) FROM agent_money_requests WHERE from_agent_id = '` + ag[0].ID + `'`, ""},
		{"accept", "/v1/workspaces/" + ws + "/money-requests/" + asked.ID + "/accept", `{}`, "",
			`SELECT count(*) FROM agent_transfers WHERE request_id = '` + asked.ID + `'`,
			`SELECT count(*) FROM agent_postings WHERE account = 'agent:` + ag[0].ID + `' AND kind = 'transfer'`},
		{"escrow", agentPath + "/escrows", fmt.Sprintf(`{"to":%q,"amount_ulxc":4000000,"release_at":%q,"memo":"logo"}`, ag[1].ID, releaseAt),
			fmt.Sprintf(`{"to":%q,"amount_ulxc":8000000,"release_at":%q,"memo":"logo"}`, ag[1].ID, releaseAt),
			`SELECT count(*) FROM agent_escrows WHERE payer_agent_id = '` + ag[0].ID + `'`,
			`SELECT count(*) FROM agent_postings WHERE account = 'agent:` + ag[0].ID + `' AND kind = 'escrow'`},
		{"cash-out", agentPath + "/cash-outs", `{"amount_ulxc":5000000,"destination":"Main account"}`,
			`{"amount_ulxc":6000000,"destination":"Main account"}`,
			`SELECT count(*) FROM agent_cash_outs WHERE agent_id = '` + ag[0].ID + `'`,
			`SELECT count(*) FROM agent_postings WHERE account = 'agent:` + ag[0].ID + `' AND kind = 'cash_out'`},
		{"schedule", agentPath + "/schedules", fmt.Sprintf(`{"to_agent_id":%q,"amount_ulxc":1000000,"every":"week"}`, ag[2].ID),
			fmt.Sprintf(`{"to_agent_id":%q,"amount_ulxc":7000000,"every":"week"}`, ag[2].ID),
			`SELECT count(*) FROM agent_payment_schedules WHERE from_agent_id = '` + ag[0].ID + `'`, ""},
	} {
		for i := range 2 {
			if w := post(c.path, c.body, c.name+"-1"); w.Code != http.StatusOK && w.Code != http.StatusCreated {
				t.Fatalf("%s, call %d with one key = %d %s, want it answered", c.name, i+1, w.Code, w.Body.String())
			}
		}
		if n := count(c.rows); n != 1 {
			t.Errorf("%s sent twice with one key wrote %d rows, want 1", c.name, n)
		}
		if c.postings != "" {
			if n := count(c.postings); n != 1 {
				t.Errorf("%s sent twice with one key wrote %d postings on the payer's account, want 1", c.name, n)
			}
		}
		if c.other != "" {
			if w := post(c.path, c.other, c.name+"-1"); w.Code != http.StatusConflict {
				t.Errorf("%s with the same key and another amount = %d %s, want 409", c.name, w.Code, w.Body.String())
			}
			if n := count(c.rows); n != 1 {
				t.Errorf("%s: the same key with another amount wrote a row: %d, want 1", c.name, n)
			}
		}
	}
	if n, sum := b501Postings(t, pool, "agent:"+ag[0].ID); n != 1 || sum != -3_000_000 {
		t.Errorf("the payer's transfer postings = %d summing %d, want the one accepted request of -3 LXC", n, sum)
	}
	b501ZeroSum(t, pool)

	t.Run("standing order", func(t *testing.T) {
		const ws = "ws-b2872-standing"
		b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
			func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry { return sandboxMoneyLayer(t, pool, store, reg) })
		r := chi.NewRouter()
		mountAgentAccountRoutes(r, b.store, tenant.NewStore(pool))
		path := "/v1/workspaces/" + ws + "/agents/" + b.acct.AgentID + "/schedules"
		owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
		send := func(amount int) int {
			body := fmt.Sprintf(`{"to_payee_id":%q,"amount_minor":%d,"currency":"GBP","memo":"Rent","every":"month"}`, b.payee.ID, amount)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Idempotency-Key", "rent-1")
			req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			return w.Code
		}
		for i := range 2 {
			if code := send(2000); code != http.StatusCreated {
				t.Fatalf("standing order, call %d with one key = %d, want 201", i+1, code)
			}
		}
		if code := send(9000); code != http.StatusConflict {
			t.Errorf("a standing order with the same key and another amount = %d, want 409", code)
		}
		if n := count(`SELECT count(*) FROM agent_payment_schedules WHERE workspace_id = $1`, ws); n != 1 {
			t.Errorf("a standing order sent twice with one key, then once more for another amount, wrote %d rows, want 1", n)
		}
	})
}
