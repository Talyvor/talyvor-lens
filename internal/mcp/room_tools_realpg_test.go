package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

type roomTestDeps struct{ agents *economy.DualTokenStore }

func (d roomTestDeps) RunDeps(context.Context) rooms.DepsFor {
	return func(rooms.Payer) market.UseDeps {
		return market.UseDeps{Runner: answersFour{}, Meter: acceptsEveryUse{}, Agents: d.agents}
	}
}

// B32.36 — an agent takes part in a public room over MCP with its own key, as its owner's member: it joins, posts (its
// message carries its name), proposes a skill, votes, forks, and runs another member's $0.10 contribution paying itself
// within its rules — one billed row with its id and the room; a $0.50 run above its per-request cap is an isError naming
// the rule and writes no row; paying a members_with_spend room is refused, naming may_spend, while its owner's
// membership was never given it (B17.174, the testers' room-agent-mcp); every call is in
// agent_tool_calls and counts against LENS_ROOM_MESSAGES_PER_MINUTE.
func TestRoomTools_AnAgentTakesPartInARoomWithinItsRules(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const host, author, agentCo = "ws-b3236-host", "ws-b3236-author", "ws-b3236-agentco"
	for _, ws := range []string{host, author, agentCo} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)

	room, err := store.Create(ctx, host, "user-host", rooms.Draft{Title: "Release notes", Topic: "writing", Visibility: rooms.Public,
		Terms: rooms.TermsDraft{RemixShareBPS: 2000, DefaultPriceUSDMicros: 100_000, SpendPolicy: rooms.SpendMembersWithSpend}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{author, agentCo} {
		if _, _, err := store.Join(ctx, ws, "user-"+ws, room.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	prompt := json.RawMessage(`{"template":"Release notes for {{change}}.","model":"gpt-4o-mini"}`)
	cheap, _, err := store.Contribute(ctx, author, "user-author", room.ID, "", rooms.ContributionDraft{Kind: "prompt", Title: "Notes", Artifact: prompt})
	if err != nil {
		t.Fatal(err)
	}
	fifty := int64(500_000)
	dear, _, err := store.Contribute(ctx, author, "user-author", room.ID, "", rooms.ContributionDraft{Kind: "prompt", Title: "Long notes",
		Artifact: prompt, PriceUSDMicros: &fifty})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := bank.CreateAgent(ctx, agentCo, "Scout", "owner-"+agentCo)
	if err != nil {
		t.Fatal(err)
	}
	perRequest := int64(2_000_000) // 2 LXC is $0.20 a request: the $0.10 contribution fits, the $0.50 one does not
	if _, err := bank.SetAgentRules(ctx, agentCo, agent.ID, economy.AgentRules{MaxPerRequestULXC: perRequest}); err != nil {
		t.Fatal(err)
	}
	if err := bank.AttachAgentKey(ctx, agentCo, agent.ID, "key-scout"); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(bank)
	srv.SetRooms(store, roomTestDeps{agents: bank})

	calls := 0
	call := func(tool, args string, wantRefused bool) (map[string]any, string) {
		t.Helper()
		calls++
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-scout", WorkspaceID: agentCo}))
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
		var out map[string]any
		if !wantRefused {
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatalf("%s: %s", tool, text)
			}
		}
		return out, text
	}
	in := `"room_id":"` + room.ID + `"`

	listed, _ := call("room_list", `{"topic":"writing"}`, false)
	if open, _ := listed["open"].([]any); len(open) != 1 || open[0].(map[string]any)["id"] != room.ID {
		t.Fatalf("room_list = %v; want the open room", listed)
	}
	if _, why := call("room_post", `{`+in+`,"body":"hello"}`, true); !strings.Contains(why, "room_join") {
		t.Fatalf("posting before joining answered %q; want it to say to join first", why)
	}
	if joined, _ := call("room_join", `{`+in+`}`, false); joined["agent_id"] != agent.ID || joined["workspace_id"] != agentCo {
		t.Fatalf("room_join = %v; want the agent in the room as its owner's", joined)
	}

	posted, _ := call("room_post", `{`+in+`,"body":"I can draft these."}`, false)
	if posted["author_agent_id"] != agent.ID || posted["author_agent_name"] != "Scout" || posted["author_workspace_id"] != agentCo {
		t.Fatalf("room_post = %v; want the message by the agent, carrying its name, for its owner", posted)
	}
	proposed, _ := call("room_propose", `{`+in+`,"kind":"skill","title":"Changelog skill","artifact":{"instructions":"Group the changes.","model":"gpt-4o-mini"}}`, false)
	if proposed["author_workspace_id"] != agentCo || proposed["status"] != rooms.ContributionProposed {
		t.Fatalf("room_propose = %v; want a proposed contribution of the agent's owner", proposed)
	}
	if v, _ := call("room_vote", `{`+in+`,"contribution_id":"`+cheap.ID+`","vote":1}`, false); v["tally"] != float64(1) {
		t.Fatalf("room_vote = %v; want a tally of 1", v)
	}
	if f, _ := call("room_fork", `{`+in+`,"contribution_id":"`+cheap.ID+`","title":"Short notes"}`, false); f["forked_from"] != cheap.ID {
		t.Fatalf("room_fork = %v; want a fork of the author's contribution", f)
	}

	ran, _ := call("room_run", `{`+in+`,"target":"`+cheap.ID+`","variables":{"change":"v2"},"pay":"self"}`, false)
	use, _ := ran["use"].(map[string]any)
	var buyer, useAgent, useRoom, actor string
	var price int64
	if err := pool.QueryRow(ctx, `SELECT buyer_workspace_id, agent_id, room_id, actor_workspace_id, price_ulxc FROM market_uses
		WHERE listing_id = $1 AND charge = 'billed'`, cheap.ListingID).Scan(&buyer, &useAgent, &useRoom, &actor, &price); err != nil {
		t.Fatalf("the run's billed row: %v (run = %v)", err, ran)
	}
	if use["id"] == nil || buyer != agentCo || useAgent != agent.ID || useRoom != room.ID || actor != agentCo || price != 1_000_000 {
		t.Fatalf("the run's row = buyer %s, agent %s, room %s, actor %s, %d µLXC; want the agent's owner buying as the agent in the room, 1,000,000 µLXC",
			buyer, useAgent, useRoom, actor, price)
	}
	if msg, _ := ran["message"].(map[string]any); msg["author_agent_id"] != agent.ID || msg["kind"] != rooms.KindRun {
		t.Fatalf("the run's message = %v; want a run message by the agent", msg)
	}

	_, why := call("room_run", `{`+in+`,"target":"`+dear.ID+`","variables":{"change":"v2"},"pay":"self"}`, true)
	if !strings.Contains(why, "limit per request") {
		t.Fatalf("a $0.50 run under a $0.20 cap answered %q; want the refusal to name the agent's limit per request", why)
	}
	var dearUses int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE listing_id = $1`, dear.ListingID).Scan(&dearUses); err != nil || dearUses != 0 {
		t.Fatalf("the refused run wrote %d rows, %v; want none", dearUses, err)
	}
	if _, why := call("room_run", `{`+in+`,"target":"`+cheap.ID+`","variables":{"change":"v3"},"pay":"room"}`, true); !strings.Contains(why, "may_spend") {
		t.Fatalf("paying room without may_spend answered %q; want it refused", why)
	}

	page, _ := call("room_messages", `{`+in+`}`, false)
	byAgent := 0
	for _, m := range page["messages"].([]any) {
		if m.(map[string]any)["author_agent_name"] == "Scout" {
			byAgent++
		}
	}
	if byAgent != 4 { // its post, its proposal, its fork and its run
		t.Fatalf("room_messages has %d messages carrying the agent's name; want 4: %v", byAgent, page)
	}

	store.SetMessagesPerMinute(calls)
	if _, why := call("room_messages", `{`+in+`}`, true); !strings.Contains(why, rooms.MessagesPerMinuteSetting) {
		t.Fatalf("a call past the minute's limit answered %q; want it to name %s", why, rooms.MessagesPerMinuteSetting)
	}

	var logged, refused int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE outcome = 'refused') FROM agent_tool_calls
		WHERE workspace_id = $1 AND agent_id = $2 AND tool LIKE 'room\_%'`, agentCo, agent.ID).Scan(&logged, &refused); err != nil {
		t.Fatal(err)
	}
	if logged != calls || refused != 4 {
		t.Fatalf("agent_tool_calls has %d room calls (%d refused); want all %d, the 4 refusals among them", logged, refused, calls)
	}
}
