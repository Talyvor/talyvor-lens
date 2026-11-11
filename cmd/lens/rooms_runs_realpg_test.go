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
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/tenant"
)

// B32.33 — a member with may_spend runs another member's $0.10 contribution on the room: one billed market_uses row with
// the owner as buyer, the room wallet as agent and the room and member recorded, its model call made with the wallet's
// key, and a run message naming it; a run past the room wallet's monthly limit is refused naming the rule and writes no
// row; pay self bills the member's own workspace; the room's AI answers a question with the room's messages before it.
func TestRooms_RunsInARoom_OnTheRoomOrOnYourself(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, author, runner = "ws-b3233-owner", "ws-b3233-author", "ws-b3233-runner"
	for _, ws := range []string{owner, author, runner} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, owner); err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
	store.SetKeys(tenant.NewStore(pool))
	bank.SetRoomBudgets(store)

	// The models: a call made with the room wallet's key reaches the proxy through roomWalletProxy; any other through
	// the router, with the member's own credential.
	type seen struct{ workspace, key, auth, prompt string }
	var calls []seen
	model := func(answer string) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				Messages []market.Message `json:"messages"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			s := seen{workspace: req.Header.Get("X-Talyvor-Workspace"), auth: req.Header.Get("Authorization")}
			if actx := auth.GetAuthContext(req.Context()); actx != nil {
				s.key = actx.APIKeyID
			}
			for _, m := range body.Messages {
				s.prompt += m.Content + "\n"
			}
			calls = append(calls, s)
			writeJSONOK(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
		}
	}
	lens := chi.NewRouter()
	lens.Post("/v1/proxy/openai/*", model("Bring your own words."))
	var metered roomWalletMeter
	r := chi.NewRouter()
	mountRoomRoutes(r, store)
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountRoomRunRoutes(r, store, lens, roomWalletProxy(model("Rooms that ship."), model("Rooms that ship.")), &metered, bank)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer key-of-"+ws)
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	must := func(want int, ws, method, path, body string) string {
		t.Helper()
		code, out := call(ws, method, path, body)
		if code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, code, out, want)
		}
		return out
	}

	// The owner opens a room whose members with may_spend spend its budget, at $0.10 a use, and gives it a budget of
	// 1.5 LXC ($0.15) a month: one $0.10 run fits, a second does not.
	var room rooms.Detail
	_ = json.Unmarshal([]byte(must(http.StatusCreated, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms",
		`{"title":"Launch copy","terms":{"spend_policy":"members_with_spend","default_price_usd_micros":100000}}`)), &room)
	wallet := room.Wallet.AgentID
	must(http.StatusOK, owner, http.MethodPost, "/v1/workspaces/"+owner+"/agents/"+wallet+"/fund", `{"amount_ulxc":50000000}`)
	must(http.StatusOK, owner, http.MethodPut, "/v1/workspaces/"+owner+"/agents/"+wallet+"/rules",
		`{"monthly_limit_ulxc":1500000,"max_per_request_ulxc":10000000,"approval_above_ulxc":10000000}`)
	for _, ws := range []string{author, runner} {
		must(http.StatusCreated, ws, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`)
	}
	must(http.StatusOK, owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+runner, `{"may_spend":true}`)
	var c rooms.Contribution
	_ = json.Unmarshal([]byte(must(http.StatusCreated, author, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions",
		`{"kind":"prompt","title":"Launch tagline","artifact":{"template":"A tagline for {{product}}.","model":"gpt-4o-mini"}}`)), &c)
	must(http.StatusCreated, runner, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"We need a tagline for the launch."}`)

	var walletKey string
	if err := pool.QueryRow(ctx, `SELECT scoped_key_id FROM agent_account_keys WHERE agent_id = $1`, wallet).Scan(&walletKey); err != nil {
		t.Fatal(err)
	}
	type useRow struct {
		buyer, agent, room, actor, charge string
		price                             int64
	}
	uses := func() []useRow {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT buyer_workspace_id, agent_id, room_id, actor_workspace_id, charge, price_ulxc FROM market_uses
			WHERE listing_id = $1 ORDER BY used_at, id`, c.ListingID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []useRow
		for rows.Next() {
			var u useRow
			if err := rows.Scan(&u.buyer, &u.agent, &u.room, &u.actor, &u.charge, &u.price); err != nil {
				t.Fatal(err)
			}
			out = append(out, u)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	runMessages := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM room_messages WHERE room_id = $1 AND kind = 'run'`, room.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The member with may_spend runs the author's contribution on the room: the owner buys it, on the room's wallet, with
	// the room and the member recorded; its model call is made with the wallet's key; the room is told who paid.
	var res rooms.RunResult
	_ = json.Unmarshal([]byte(must(http.StatusOK, runner, http.MethodPost, "/v1/rooms/"+room.ID+"/runs",
		`{"target":"`+c.ID+`","variables":{"product":"rooms"},"pay":"room"}`)), &res)
	got := uses()
	if len(got) != 1 || got[0] != (useRow{buyer: owner, agent: wallet, room: room.ID, actor: runner, charge: market.ChargeBilled, price: 1_000_000}) {
		t.Fatalf("the room's run wrote %+v, want one billed use of 1,000,000 µLXC by %s on %s in %s for %s", got, owner, wallet, room.ID, runner)
	}
	if len(metered) != 1 || res.Use == nil || metered[0] != res.Use.ID || res.Use.Output != "Rooms that ship." {
		t.Fatalf("the run = %+v, metered %v; want its use on the owner's bill and the wallet's answer", res, metered)
	}
	if len(calls) != 1 || calls[0].key != walletKey || calls[0].workspace != owner || calls[0].auth != "" {
		t.Fatalf("the run's model call = %+v, want it made with the wallet's key %s in %s", calls, walletKey, owner)
	}
	if res.Message == nil || res.Message.Kind != rooms.KindRun || !strings.Contains(string(res.Message.Refs), `"use_id":"`+res.Use.ID+`"`) ||
		!strings.Contains(string(res.Message.Refs), `"pay":"room"`) || !strings.Contains(res.Message.Body, "on the room's budget — $0.10") {
		t.Fatalf("the run message = %+v, want one naming the use, the charge and who paid", res.Message)
	}

	// A second run would take the wallet past its monthly limit: refused naming the rule, and nothing is written.
	if code, body := call(runner, http.MethodPost, "/v1/rooms/"+room.ID+"/runs",
		`{"target":"`+c.ID+`","variables":{"product":"rooms"},"pay":"room"}`); code != http.StatusForbidden || !strings.Contains(body, "monthly limit") {
		t.Fatalf("a run past the monthly limit = %d %s, want 403 naming it", code, body)
	}
	if n := len(uses()); n != 1 || runMessages() != 1 || len(calls) != 1 {
		t.Fatalf("a refused run left %d uses, %d run messages and %d model calls; want 1, 1 and 1", n, runMessages(), len(calls))
	}

	// B58.2: a run above the wallet's approval amount is held for the owner: 202 naming the approval it filed, and
	// nothing is written.
	must(http.StatusOK, owner, http.MethodPut, "/v1/workspaces/"+owner+"/agents/"+wallet+"/rules", `{"monthly_limit_ulxc":10000000,"max_per_request_ulxc":10000000,"approval_above_ulxc":500000}`)
	code, body := call(runner, http.MethodPost, "/v1/rooms/"+room.ID+"/runs", `{"target":"`+c.ID+`","variables":{"product":"rooms"},"pay":"room"}`)
	var held struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		AmountULXC int64  `json:"amount_ulxc"`
	}
	var filed string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending'`, wallet).Scan(&filed); err != nil {
		t.Fatalf("the held run's approval: %v (the answer was %d %s)", err, code, body)
	}
	if _ = json.Unmarshal([]byte(body), &held); code != http.StatusAccepted || held.Status != "pending_approval" || held.ApprovalID != filed || held.AmountULXC != 1_000_000 {
		t.Fatalf("a run above the approval amount = %d %s, want 202 naming %s", code, body, filed)
	}
	if n := len(uses()); n != 1 || runMessages() != 1 || len(calls) != 1 {
		t.Fatalf("a held run left %d uses, %d run messages and %d model calls; want 1, 1 and 1", n, runMessages(), len(calls))
	}
	must(http.StatusOK, owner, http.MethodPut, "/v1/workspaces/"+owner+"/agents/"+wallet+"/rules",
		`{"monthly_limit_ulxc":1500000,"max_per_request_ulxc":10000000,"approval_above_ulxc":10000000}`)

	// Paying self, the member's own workspace buys it and its own credential calls the model.
	_ = json.Unmarshal([]byte(must(http.StatusOK, runner, http.MethodPost, "/v1/rooms/"+room.ID+"/runs",
		`{"target":"`+c.ID+`","variables":{"product":"rooms"},"pay":"self"}`)), &res)
	if got := uses(); len(got) != 2 || got[1] != (useRow{buyer: runner, room: room.ID, actor: runner, charge: market.ChargeBilled, price: 1_000_000}) {
		t.Fatalf("the self-paid run wrote %+v, want a second billed use bought by %s", got, runner)
	}
	if last := calls[len(calls)-1]; last.auth != "Bearer key-of-"+runner || last.key != "" || !strings.Contains(res.Message.Body, "on their own account") {
		t.Fatalf("the self-paid run's model call = %+v, message %q; want the member's own credential", last, res.Message.Body)
	}

	// The room's AI reads the room's messages with the question, on the room's wallet, and the room sees the answer.
	_ = json.Unmarshal([]byte(must(http.StatusOK, runner, http.MethodPost, "/v1/rooms/"+room.ID+"/ask",
		`{"question":"Which tagline is best?","model":"gpt-4o-mini","pay":"room"}`)), &res)
	ask := calls[len(calls)-1]
	if ask.key != walletKey || !strings.Contains(ask.prompt, "We need a tagline for the launch.") || !strings.Contains(ask.prompt, "Which tagline is best?") {
		t.Fatalf("the ask's model call = %+v, want the room's messages and the question, with the wallet's key", ask)
	}
	if res.Answer != "Rooms that ship." || res.Message == nil || !strings.Contains(res.Message.Body, "Which tagline is best?\n\nRooms that ship.") ||
		!strings.Contains(string(res.Message.Refs), `"run":"ask"`) || runMessages() != 3 {
		t.Fatalf("the ask = %+v, want its answer posted to the room as the third run message", res)
	}
}
