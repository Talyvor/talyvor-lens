package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// search_tools.go — B41.107: search_workspace, the owner's assistant's one search across the workspace's agents,
// payees, payments, bills, invoices, rules and listings — the same search as GET /v1/money/search, under the same gate.

// Searcher is what search_workspace needs of the economy store.
type Searcher interface {
	Search(ctx context.Context, workspaceID, query string, limit int) (economy.SearchResults, error)
}

// SetSearcher offers search_workspace; nil leaves it out.
func (s *Server) SetSearcher(x Searcher) { s.searcher = x }

func searchToolDefinitions() []map[string]any {
	return []map[string]any{{
		"name": "search_workspace",
		"description": "Searches the workspace's agents, payees, payments, bills, invoices, spending rules and listings for a name or reference " +
			"(\"Acme\" finds the payee Acme, its bills and the payments to it), grouped by kind. Each result has its kind and id.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"query": map[string]any{"type": "string", "minLength": 2, "maxLength": 100, "description": "What to search for."},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": economy.SearchMaxLimit, "description": "At most this many of each kind; 5 when left out."},
		}, "required": []string{"query"}},
	}}
}

func (s *Server) toolSearchWorkspace(ctx context.Context, raw json.RawMessage) (any, error) {
	ws := effectiveWorkspace(ctx, "")
	if ws == "" {
		return nil, &toolRefusal{"search is a workspace's: use a workspace's key"}
	}
	if _, owner := storedanswers.OwnerOrAdmin(ctx); !owner {
		return nil, &toolRefusal{"only the workspace's owner or an admin may search it"}
	}
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &toolRefusal{"the arguments are not the tool's: " + err.Error()}
		}
	}
	out, err := s.searcher.Search(ctx, ws, a.Query, a.Limit)
	if errors.Is(err, economy.ErrSearchQuery) {
		return nil, &toolRefusal{err.Error()}
	}
	return out, err
}
