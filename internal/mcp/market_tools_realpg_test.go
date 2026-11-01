package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

type marketTestDeps struct{ agents *economy.DualTokenStore }

type answersFour struct{}

func (answersFour) Run(context.Context, string, []market.Message) (string, error) { return "4", nil }

type acceptsEveryUse struct{}

func (acceptsEveryUse) MeterMarketUse(context.Context, string, string, int64, time.Time) error {
	return nil
}

func (d marketTestDeps) UseDeps(context.Context, string) market.UseDeps {
	return market.UseDeps{Runner: answersFour{}, Meter: acceptsEveryUse{}, Agents: d.agents}
}

func (d marketTestDeps) LicenceDeps(context.Context, string) market.LicenceDeps {
	return market.LicenceDeps{Meter: acceptsEveryUse{}, Agents: d.agents, Capabilities: d.agents}
}

// B32.23 — an agent shops the marketplace over MCP with its own key: it searches, rents a listing (one billed rent row
// with its id, and a licence) and uses it (a 'licensed' row, no new billed row); a rent above its max_commitment is an
// isError naming the rule and writes nothing; a use whose price was raised above its max_price writes no row; and every
// call is in agent_tool_calls.
func TestMarketTools_AnAgentSearchesRentsAndUsesWithinItsRules(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const seller, buyer = "ws-b3223-seller", "ws-b3223-buyer"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	n := func(n int) *int { return &n }
	prompt := json.RawMessage(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`)
	adder, err := store.Publish(ctx, seller, market.Draft{Kind: "prompt", Title: "Adder", Description: "adds two numbers", Visibility: "public",
		Artifact: prompt, Offers: []market.Offer{
			{Kind: market.OfferPerUse, Licence: market.LicenceCommercial, PriceUSDMicros: 50_000},
			{Kind: market.OfferRent, Licence: market.LicenceCommercial, PriceUSDMicros: 1_500_000, PeriodDays: n(30)},
			{Kind: market.OfferRent, Licence: market.LicenceEnterprise, PriceUSDMicros: 2_500_000, PeriodDays: n(30), Seats: n(5)},
		}})
	if err != nil {
		t.Fatal(err)
	}
	doubler, err := store.Publish(ctx, seller, market.Draft{Kind: "prompt", Title: "Doubler", Visibility: "public", Artifact: prompt,
		Offers: []market.Offer{{Kind: market.OfferPerUse, Licence: market.LicenceCommercial, PriceUSDMicros: 50_000}}})
	if err != nil {
		t.Fatal(err)
	}
	twenty := int64(20_000_000) // 20 LXC is $2.00: the $1.50 rent fits, the $2.50 one does not
	agent, err := bank.CreateAgent(ctx, buyer, "shopper", "owner-"+buyer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaxCommitmentULXC: &twenty}); err != nil {
		t.Fatal(err)
	}
	if err := bank.AttachAgentKey(ctx, buyer, agent.ID, "key-shopper"); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(bank)
	srv.SetMarket(store, marketTestDeps{agents: bank})

	calls := 0
	call := func(tool, args string, wantRefused bool) (map[string]any, string) {
		t.Helper()
		calls++
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-shopper", WorkspaceID: buyer}))
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
	rows := func() (licences, billed, licensed int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM market_licences WHERE agent_id = $1),
				(SELECT count(*) FROM market_uses WHERE agent_id = $1 AND charge = 'billed'),
				(SELECT count(*) FROM market_uses WHERE agent_id = $1 AND charge = 'licensed')`, agent.ID).Scan(&licences, &billed, &licensed); err != nil {
			t.Fatal(err)
		}
		return
	}

	found, _ := call("market_search", `{"text":"adds numbers","max_price_usd_micros":50000}`, false)
	if hits, _ := found["listings"].([]any); len(hits) != 1 || hits[0].(map[string]any)["id"] != adder.ID {
		t.Fatalf("searching for the adder = %v; want exactly it", found)
	}
	// B41.171: market_ask answers with the adder and what 20 uses of it cost.
	asked, _ := call("market_ask", `{"text":"adds numbers 20 times"}`, false)
	if picks, _ := asked["picks"].([]any); len(picks) != 1 || picks[0].(map[string]any)["id"] != adder.ID || asked["volume"] != float64(20) ||
		picks[0].(map[string]any)["estimate"].(map[string]any)["volume"] != float64(20) {
		t.Fatalf("asking for the adder = %v; want it, priced for 20 uses", asked)
	}
	listing, _ := call("market_listing", `{"listing_id":"`+adder.ID+`"}`, false)
	// B32.49: market_listing carries the trust panel's summary, as the trust read gives it.
	trust, err := store.Trust(ctx, buyer, adder.ID)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if raw, _ := json.Marshal(trust); json.Unmarshal(raw, &want) != nil || !reflect.DeepEqual(listing["trust"], any(want)) {
		t.Fatalf("market_listing's trust = %v; want the trust read's %v", listing["trust"], want)
	}
	offer := map[string]string{}
	for _, o := range listing["offers"].([]any) {
		o := o.(map[string]any)
		offer[o["kind"].(string)+" "+o["licence"].(string)] = o["id"].(string)
	}

	rent, _ := call("market_license", `{"listing_id":"`+adder.ID+`","offer_id":"`+offer["rent commercial"]+`","idempotency_key":"rent-1"}`, false)
	var useKind, useAgent string
	if err := pool.QueryRow(ctx, `SELECT use_kind, agent_id FROM market_uses WHERE id = $1 AND charge = 'billed' AND price_ulxc = 15000000`,
		rent["use_id"]).Scan(&useKind, &useAgent); err != nil || useKind != "rent" || useAgent != agent.ID {
		t.Fatalf("the rent's row = %q by %q, %v; want one billed rent row at 15,000,000 µLXC by the agent", useKind, useAgent, err)
	}
	if licences, billed, _ := rows(); licences != 1 || billed != 1 {
		t.Fatalf("after the rent the agent has %d licences and %d billed rows; want 1 and 1", licences, billed)
	}

	_, why := call("market_license", `{"listing_id":"`+adder.ID+`","offer_id":"`+offer["rent enterprise"]+`","idempotency_key":"rent-2"}`, true)
	if !strings.Contains(why, "max_commitment_ulxc") {
		t.Fatalf("a $2.50 rent under a $2.00 commitment answered %q; want the refusal to name max_commitment_ulxc", why)
	}
	if licences, billed, _ := rows(); licences != 1 || billed != 1 {
		t.Fatalf("after the refused rent the agent has %d licences and %d billed rows; want still 1 and 1", licences, billed)
	}

	used, _ := call("market_use", `{"listing_id":"`+adder.ID+`","variables":{"a":"2","b":"2"},"max_price_usd_micros":0}`, false)
	if used["charge"] != market.ChargeLicensed || used["output"] != "4" {
		t.Fatalf("a use under the rent = %v; want it run and charged licensed", used)
	}
	if _, billed, licensed := rows(); billed != 1 || licensed != 1 {
		t.Fatalf("after the covered use the agent has %d billed and %d licensed rows; want 1 and 1", billed, licensed)
	}

	billedUse, _ := call("market_use", `{"listing_id":"`+doubler.ID+`","variables":{"a":"1","b":"1"},"max_price_usd_micros":50000}`, false)
	if billedUse["charge"] != market.ChargeBilled {
		t.Fatalf("a $0.05 use within a $0.05 max_price = %v; want it billed", billedUse)
	}
	if _, err := store.ReplaceOffers(ctx, seller, doubler.ID, []market.Offer{{Kind: market.OfferPerUse, Licence: market.LicenceCommercial, PriceUSDMicros: 100_000}}); err != nil {
		t.Fatal(err)
	}
	_, why = call("market_use", `{"listing_id":"`+doubler.ID+`","variables":{"a":"1","b":"1"},"max_price_usd_micros":50000}`, true)
	if !strings.Contains(why, "max_price_usd_micros") {
		t.Fatalf("a use raised to $0.10 above a $0.05 max_price answered %q; want the refusal to name max_price_usd_micros", why)
	}
	var doublerUses int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE listing_id = $1`, doubler.ID).Scan(&doublerUses); err != nil || doublerUses != 1 {
		t.Fatalf("the doubler has %d uses, %v; want only the one within the max price", doublerUses, err)
	}
	// B17.158: the buyer's bill (GET .../marketplace/bill) holds the rent and the one billed use — nothing for the
	// refused rent, the licensed use or the use above the max price.
	bill, err := store.MonthBill(ctx, buyer, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	wantLines := map[string]string{rent["use_id"].(string): adder.ID + " 15000000", billedUse["id"].(string): doubler.ID + " 500000"}
	if len(bill.Lines) != len(wantLines) {
		t.Fatalf("the buyer's bill = %+v; want exactly the rent and the one billed use", bill.Lines)
	}
	for _, l := range bill.Lines {
		if wantLines[l.UseID] != fmt.Sprintf("%s %d", l.ListingID, l.PriceULXC) || l.AgentID != agent.ID {
			t.Fatalf("the buyer's bill line %+v; want one of %v by the agent", l, wantLines)
		}
	}

	mine, _ := call("market_licences", `{}`, false)
	if list, _ := mine["licences"].([]any); len(list) != 1 || list[0].(map[string]any)["id"] != rent["id"] {
		t.Fatalf("market_licences = %v; want the one rent", mine)
	}
	if c, _ := call("market_cancel", `{"licence_id":"`+rent["id"].(string)+`"}`, false); c["auto_renew"] != false {
		t.Fatalf("cancelling the rent = %v; want it not renewing", c)
	}

	var logged, refused int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE outcome = 'refused') FROM agent_tool_calls
		WHERE workspace_id = $1 AND agent_id = $2 AND tool LIKE 'market\_%'`, buyer, agent.ID).Scan(&logged, &refused); err != nil {
		t.Fatal(err)
	}
	if logged != calls || refused != 2 {
		t.Fatalf("agent_tool_calls has %d market calls (%d refused); want all %d, the 2 refusals among them", logged, refused, calls)
	}
}
