package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// listing_tool.go — B41.178: A LISTING AS ITS OWN TOOL.
//
// /mcp/listings/{id} is this same server offering exactly one tool: the listing, named after its title. The Talyvor
// connector (internal/connector) adds that URL to Claude, ChatGPT or Cursor; a call is market_use with the listing
// fixed, on the agent's own key, so its rules judge it, it is logged in agent_tool_calls and a per-use price is one
// billed use on its owner's marketplace bill.

type listingKey struct{}

// HandleListingRPC answers /mcp/listings/{listingID} as HandleRPC answers /mcp, with only the listing's tool.
func (s *Server) HandleListingRPC(w http.ResponseWriter, r *http.Request, listingID string) {
	s.HandleRPC(w, r.WithContext(context.WithValue(r.Context(), listingKey{}, listingID)))
}

func listingOf(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(listingKey{}).(string)
	return id, ok
}

var notToolChars = regexp.MustCompile(`[^a-z0-9]+`)

// listingToolName is the tool's name from the listing's title: MCP names are [a-zA-Z0-9_-]{1,64}.
func listingToolName(title string) string {
	name := strings.Trim(notToolChars.ReplaceAllString(strings.ToLower(title), "_"), "_")
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "_")
	}
	if name == "" {
		return "use_listing"
	}
	return name
}

// listingTool is the one tool /mcp/listings/{id} lists; the listing must be one the caller's workspace may see.
func (s *Server) listingTool(ctx context.Context, listingID string) (map[string]any, error) {
	if s.market == nil || s.agentBank == nil {
		return nil, &toolRefusal{"the marketplace is not configured"}
	}
	l, err := s.market.Get(ctx, effectiveWorkspace(ctx, ""), listingID)
	if err != nil {
		return nil, err
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	desc := strings.TrimSpace(l.Title + ": " + l.Description)
	t := map[string]any{"name": listingToolName(l.Title), "title": l.Title,
		"description": desc + " — from the Talyvor marketplace. Each use runs on your agent's key under its rules; a licence covers it or its per-use price goes on your marketplace bill.",
		"inputSchema": map[string]any{"type": "object", "required": []string{}, "properties": map[string]any{
			"input":                str("your message, for an agent or a skill"),
			"variables":            map[string]any{"type": "object", "description": "a prompt's {{variables}}", "additionalProperties": map[string]any{"type": "string"}},
			"model":                str("the model to run it on, when the listing names none"),
			"max_price_usd_micros": map[string]any{"type": "integer", "description": "the most this use may cost, in µUSD (1 USD = 1,000,000)"},
		}},
		"annotations": map[string]any{"title": l.Title, "readOnlyHint": false, "destructiveHint": true, "openWorldHint": false},
	}
	return t, nil
}

func (s *Server) dispatchListing(ctx context.Context, req rpcRequest, listingID string) rpcResponse {
	tool, err := s.listingTool(ctx, listingID)
	if err != nil {
		return rpcErr(req.ID, rpcErrInvalidParam, err.Error())
	}
	if req.Method == "tools/list" {
		return rpcOK(req.ID, map[string]any{"tools": []map[string]any{tool}})
	}
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcErr(req.ID, rpcErrInvalidParam, "Invalid params: "+err.Error())
	}
	if params.Name != tool["name"] {
		return rpcErr(req.ID, rpcErrMethodNotFnd, "unknown tool: "+params.Name)
	}
	var args map[string]any
	if len(params.Arguments) > 0 && string(params.Arguments) != "null" {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return rpcOK(req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "invalid arguments: " + err.Error()}}, "isError": true})
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	args["listing_id"] = listingID // the URL's listing, never the model's
	raw, _ := json.Marshal(map[string]any{"name": "market_use", "arguments": args})
	return s.handleToolsCall(ctx, req.ID, raw)
}
