package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// B39.1 — behind PgBouncer (LENS_DB_PGBOUNCER=true) Lens speaks pgx's simple protocol (cmd/lens/main.go), where a
// []byte bound to agent_tool_calls.arguments goes out as bytea and Postgres refuses it with 22P02 — so every agent
// tool call over MCP ended in an error after the tool had run. This pool is built the way production builds it.
func TestB391_AnAgentsMCPToolIsAnsweredBehindPgBouncer(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(savingsTestPool(t).Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol // what LENS_DB_PGBOUNCER=true sets
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	const seller, buyer = "ws-b391-seller", "ws-b391-buyer"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	adder, err := store.Publish(ctx, seller, market.Draft{Kind: "prompt", Title: "Adder", Description: "adds two numbers", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
		Offers:   []market.Offer{{Kind: market.OfferPerUse, Licence: market.LicenceCommercial, PriceUSDMicros: 50_000}}})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := bank.CreateAgent(ctx, buyer, "shopper", "owner-"+buyer)
	if err != nil {
		t.Fatal(err)
	}
	if err := bank.AttachAgentKey(ctx, buyer, agent.ID, "key-b391"); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(bank)
	srv.SetMarket(store, marketTestDeps{agents: bank})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"market_search","arguments":{"text":"adds numbers"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-b391", WorkspaceID: buyer}))
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
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Error != nil || resp.Result.IsError || len(resp.Result.Content) == 0 {
		t.Fatalf("market_search behind PgBouncer = %s; want its answer", w.Body.String())
	}
	var found struct {
		Listings []struct {
			ID string `json:"id"`
		} `json:"listings"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &found); err != nil || len(found.Listings) != 1 || found.Listings[0].ID != adder.ID {
		t.Fatalf("market_search answered %s; want exactly the adder %s", resp.Result.Content[0].Text, adder.ID)
	}

	var outcome, text string
	if err := pool.QueryRow(ctx, `SELECT outcome, arguments->>'text' FROM agent_tool_calls WHERE agent_id = $1 AND tool = 'market_search'`,
		agent.ID).Scan(&outcome, &text); err != nil {
		t.Fatalf("agent_tool_calls row for the call: %v", err)
	}
	if outcome != "ok" || text != "adds numbers" {
		t.Fatalf("agent_tool_calls row = outcome %q, arguments.text %q; want ok and the call's arguments as a JSON object", outcome, text)
	}
}
