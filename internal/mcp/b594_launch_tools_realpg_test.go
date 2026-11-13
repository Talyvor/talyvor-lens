package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/areas"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// B59.4 — the connector offers launch-pack tools only. A workspace the operator never switched an area on for lists
// no tool of the marketplace, Rooms, trading, credit or the old gateway, and calling one answers unknown tool; its
// agents, balances, payments prepared for approval and mandates stay. Switching the marketplace on for one workspace
// gives that workspace the marketplace's tools and nobody else. A listing's own URL is the marketplace's, so for a
// workspace without it, it lists nothing and calls nothing.

type b594World struct {
	t   *testing.T
	srv *Server
}

func newB594World(t *testing.T) (*b594World, *areas.Store, map[string]string) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	keys := map[string]string{}
	for _, ws := range []string{"ws-b594-default", "ws-b594-market"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	for _, ws := range []string{"ws-b594-default", "ws-b594-market"} {
		a, err := bank.CreateAgent(ctx, ws, "clerk", "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if err := bank.AttachAgentKey(ctx, ws, a.ID, "key-"+ws); err != nil {
			t.Fatal(err)
		}
		keys[ws] = "key-" + ws
	}
	mkt := market.NewStore(pool)
	rs := rooms.NewStore(pool, 3000)
	rs.SetMarket(mkt)
	store := areas.NewStore(pool)
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(bank)
	srv.SetMandates(bank)
	srv.SetSearcher(bank)
	srv.SetPaymentRequests(bank)
	srv.SetRecurringPayments(bank)
	srv.SetMarket(mkt, marketTestDeps{agents: bank})
	srv.SetRooms(rs, roomTestDeps{agents: bank})
	srv.SetAreas(store)
	return &b594World{t: t, srv: srv}, store, keys
}

// rpc posts one JSON-RPC message to path as ws's agent key and answers the decoded reply.
func (w *b594World) rpc(path, ws, key, method, params string) (result map[string]any, rpcErr map[string]any) {
	w.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
	rec := httptest.NewRecorder()
	if id, ok := strings.CutPrefix(path, "/mcp/listings/"); ok {
		w.srv.HandleListingRPC(rec, req, id)
	} else {
		w.srv.HandleRPC(rec, req)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		w.t.Fatalf("%s %s: %v: %s", method, params, err, rec.Body.String())
	}
	return resp.Result, resp.Error
}

func (w *b594World) toolNames(path, ws, key string) map[string]bool {
	w.t.Helper()
	res, e := w.rpc(path, ws, key, "tools/list", `{}`)
	if e != nil {
		w.t.Fatalf("tools/list for %s = %v", ws, e)
	}
	names := map[string]bool{}
	list, _ := res["tools"].([]any)
	for _, t := range list {
		names[t.(map[string]any)["name"].(string)] = true
	}
	return names
}

func (w *b594World) wantUnknownTool(path, ws, key, tool string) {
	w.t.Helper()
	_, e := w.rpc(path, ws, key, "tools/call", `{"name":"`+tool+`","arguments":{}}`)
	if e == nil || e["code"] != float64(rpcErrMethodNotFnd) || e["message"] != "unknown tool: "+tool {
		w.t.Fatalf("calling %s as %s answered error %v; want unknown tool", tool, ws, e)
	}
}

// hiddenTools is one tool of each area the MCP server has tools for.
var hiddenTools = []string{"market_search", "request_marketplace_purchase", "room_list", "wallet_order", "wallet_loans", "route_model", "get_cache_stats"}

func TestB594_ADefaultWorkspaceIsOfferedLaunchPackToolsOnly(t *testing.T) {
	w, _, keys := newB594World(t)
	const ws = "ws-b594-default"
	names := w.toolNames("/mcp", ws, keys[ws])
	for _, tool := range hiddenTools {
		if names[tool] {
			t.Errorf("a workspace with no areas is offered %s", tool)
		}
	}
	for _, tool := range []string{"agent_balance", "wallet_accounts", "wallet_pay_external", "prepare_payment", "get_request_status", "get_mandates"} {
		if !names[tool] {
			t.Errorf("a workspace with no areas is not offered the launch pack's %s", tool)
		}
	}
	for name := range names {
		if a, hidden := toolArea(name); hidden {
			t.Errorf("a workspace with no areas is offered %s, a tool of %s", name, a)
		}
	}
	for _, tool := range hiddenTools {
		w.wantUnknownTool("/mcp", ws, keys[ws], tool)
	}
}

func TestB594_TheMarketplaceSwitchedOnForOneWorkspaceOffersItsToolsThereOnly(t *testing.T) {
	w, store, keys := newB594World(t)
	if err := store.Switch(context.Background(), "ws-b594-market", areas.Marketplace, true, "operator@talyvor.com"); err != nil {
		t.Fatal(err)
	}
	on := w.toolNames("/mcp", "ws-b594-market", keys["ws-b594-market"])
	if !on["market_search"] || !on["request_marketplace_purchase"] {
		t.Fatalf("the workspace the marketplace is on for is not offered its tools: %v", on)
	}
	if on["room_list"] || on["wallet_order"] {
		t.Fatalf("switching the marketplace on offered another area's tools: %v", on)
	}
	if res, e := w.rpc("/mcp", "ws-b594-market", keys["ws-b594-market"], "tools/call", `{"name":"market_search","arguments":{"text":"anything"}}`); e != nil || res["isError"] == true {
		t.Fatalf("market_search where the marketplace is on = %v %v", res, e)
	}

	const ws = "ws-b594-default"
	if off := w.toolNames("/mcp", ws, keys[ws]); off["market_search"] {
		t.Fatal("another workspace's marketplace switch offered market_search to a workspace without it")
	}
	w.wantUnknownTool("/mcp", ws, keys[ws], "market_search")
	// A listing's URL is the marketplace's: without it there is no tool to list or call.
	if names := w.toolNames("/mcp/listings/lst_any", ws, keys[ws]); len(names) != 0 {
		t.Fatalf("a listing's URL offered %v to a workspace without the marketplace", names)
	}
	w.wantUnknownTool("/mcp/listings/lst_any", ws, keys[ws], "use_listing")
}

// everyArea is a workspace every area is switched on for: the tests of an area's own tools use it.
type everyArea struct{}

func (everyArea) List(context.Context, string) ([]areas.Area, error) { return areas.All, nil }
