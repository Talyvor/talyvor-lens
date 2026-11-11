package mcp

import (
	"context"

	"github.com/talyvor/lens/internal/areas"
	"github.com/talyvor/lens/internal/auth"
)

// areas.go — B59.4: THE CONNECTOR OFFERS LAUNCH-PACK TOOLS ONLY.
//
// A tool of an area (internal/areas) the caller's workspace does not have is not there: tools/list leaves it out and
// tools/call answers unknown tool, as for a name no tool has. A listing's own URL (/mcp/listings/{id}) is the
// marketplace's, so without it that URL lists nothing and calls nothing. The operator's admin key sees every tool.
// With no area store set, or a workspace that cannot be named, every area's tools are hidden.

// AreaReader is what the gate asks of internal/areas' Store.
type AreaReader interface {
	List(ctx context.Context, workspaceID string) ([]areas.Area, error)
}

// SetAreas gates the area tools on the caller's workspace's areas; nil hides them all.
func (s *Server) SetAreas(a AreaReader) { s.areas = a }

// toolAreas are the tools of an area that are not named for it; market_ and room_ tools are the marketplace's
// and Rooms'.
var toolAreas = map[string]areas.Area{
	"request_marketplace_purchase": areas.Marketplace,

	"wallet_quotes":         areas.Trading,
	"wallet_portfolio_open": areas.Trading,
	"wallet_portfolios":     areas.Trading,
	"wallet_order":          areas.Trading,
	"wallet_order_cancel":   areas.Trading,

	"wallet_credit_line": areas.Credit,
	"wallet_offer_loan":  areas.Credit,
	"wallet_loans":       areas.Credit,
	"wallet_answer_loan": areas.Credit,

	// The old gateway's own tools: spend and cache figures of model calls, routing, circuit breakers.
	"get_spend_summary":         areas.Gateway,
	"get_cache_stats":           areas.Gateway,
	"get_model_recommendations": areas.Gateway,
	"check_circuit_breakers":    areas.Gateway,
	"get_session_summary":       areas.Gateway,
	"route_model":               areas.Gateway,
}

// toolArea returns the area a tool belongs to, and false for a launch-pack tool.
func toolArea(name string) (areas.Area, bool) {
	switch {
	case isMarketTool(name):
		return areas.Marketplace, true
	case isRoomTool(name):
		return areas.Rooms, true
	}
	a, ok := toolAreas[name]
	return a, ok
}

// openAreas returns the areas whose tools the caller is offered.
func (s *Server) openAreas(ctx context.Context) (map[areas.Area]bool, error) {
	open := map[areas.Area]bool{}
	ws, isAdmin := auth.WorkspaceIdentity(ctx)
	if isAdmin {
		for _, a := range areas.All {
			open[a] = true
		}
		return open, nil
	}
	if ws == "" || s.areas == nil {
		return open, nil
	}
	list, err := s.areas.List(ctx, ws)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		open[a] = true
	}
	return open, nil
}

// offered leaves out of tools every tool of an area not in open.
func offered(tools []map[string]any, open map[areas.Area]bool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		name, _ := t["name"].(string)
		if a, ok := toolArea(name); ok && !open[a] {
			continue
		}
		out = append(out, t)
	}
	return out
}
