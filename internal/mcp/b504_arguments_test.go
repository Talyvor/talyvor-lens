package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// everyToolServer offers every tool /mcp has, so its schemas can be read; it runs none of them.
func everyToolServer() *Server {
	s := newServer(nil, nil, nil, nil, nil, "test")
	var bank *economy.DualTokenStore
	s.SetAgentBank(bank)
	s.SetMandates(bank)
	s.SetPaymentRequests(bank)
	s.SetRecurringPayments(bank)
	s.SetMarket(&market.Store{}, nil)
	s.SetRooms(&rooms.Store{}, nil)
	return s
}

// validFor builds the smallest arguments a schema accepts: each required property, of its type.
func validFor(schema map[string]any) any {
	switch schema["type"] {
	case "object":
		out := map[string]any{}
		props, _ := schema["properties"].(map[string]any)
		for _, k := range required(schema["required"]) {
			out[k] = validFor(props[k].(map[string]any))
		}
		return out
	case "array":
		items, _ := schema["items"].(map[string]any)
		return []any{validFor(items)}
	case "integer", "number":
		return 1
	case "boolean":
		return true
	default:
		if enum := required(schema["enum"]); len(enum) > 0 {
			return enum[0]
		}
		return "x"
	}
}

// wrongFor is a value of another type than schema names.
func wrongFor(schema map[string]any) any {
	if schema["type"] == "string" {
		return 7
	}
	return "seven"
}

// B50.4 — a model fills a tool's arguments, so /mcp holds them to the tool's schema as strictly as a money route
// holds its body: for every tool, its smallest valid arguments pass, and an unknown property, a property of the wrong
// type and a missing required one are each refused. A property a model invents ("approved": true) cannot reach a tool.
func TestB504_EveryToolsArgumentsAreHeldToItsSchema(t *testing.T) {
	s := everyToolServer()
	tools := s.tools()
	if len(tools) != len(toolAnnotations) {
		t.Fatalf("%d tools offered, %d annotated: the server under test does not offer every tool", len(tools), len(toolAnnotations))
	}
	check := func(name string, args any) error {
		raw, _ := json.Marshal(args)
		return s.checkArguments(name, raw)
	}
	for _, tool := range tools {
		name := tool["name"].(string)
		schema := tool["inputSchema"].(map[string]any)
		valid := validFor(schema).(map[string]any)
		if err := check(name, valid); err != nil {
			t.Errorf("%s: its smallest valid arguments %v were refused: %v", name, valid, err)
			continue
		}
		withExtra := map[string]any{"approved_by_owner": true}
		for k, v := range valid {
			withExtra[k] = v
		}
		if err := check(name, withExtra); err == nil || !strings.Contains(err.Error(), "approved_by_owner") {
			t.Errorf("%s: an argument the schema does not name was accepted (%v)", name, err)
		}
		props, _ := schema["properties"].(map[string]any)
		for k, p := range props {
			bad := map[string]any{}
			for vk, vv := range valid {
				bad[vk] = vv
			}
			bad[k] = wrongFor(p.(map[string]any))
			if err := check(name, bad); err == nil {
				t.Errorf("%s: %s of the wrong type was accepted", name, k)
			}
		}
		for _, k := range required(schema["required"]) {
			missing := map[string]any{}
			for vk, vv := range valid {
				if vk != k {
					missing[vk] = vv
				}
			}
			if err := check(name, missing); err == nil {
				t.Errorf("%s: arguments without the required %s were accepted", name, k)
			}
		}
		if err := check(name, []any{valid}); err == nil {
			t.Errorf("%s: arguments that are not an object were accepted", name)
		}
	}
	for _, c := range []struct{ tool, args string }{
		{"agent_pay", `{"to_agent_id":"agt_1","amount_ulxc":1.5}`},
		{"agent_pay", `{"to_agent_id":"agt_1","amount_ulxc":99999999999999999999}`},
		{"prepare_payment", `{"idempotency_key":"k","payee_id":"p","currency":"JPY","amount_minor":100}`},
		{"prepare_payment", `{"idempotency_key":"k","payee_id":"p","currency":"GBP","amount_minor":0}`},
		{"prepare_payment", `{"idempotency_key":"k","payee_id":"p","currency":"GBP","amount_minor":100,"reference":"` + strings.Repeat("r", 141) + `"}`},
		{"prepare_batch", `{"idempotency_key":"k","items":[]}`},
		{"prepare_batch", `{"idempotency_key":"k","items":[{"payee_id":"p","currency":"GBP","amount_minor":1,"note":"x"}]}`},
		{"market_use", `{"listing_id":"lst_1","variables":{"a":1}}`},
		{"agent_balance", `{} {}`},
	} {
		if err := s.checkArguments(c.tool, json.RawMessage(c.args)); err == nil {
			t.Errorf("%s %s was accepted", c.tool, c.args)
		}
	}
}
