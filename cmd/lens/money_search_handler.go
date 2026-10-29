package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B41.107 — one search across the workspace's money (economy/search.go).
//
//	GET /v1/money/search?q=&limit= — the workspace's agents, payees, payments, bills, invoices, rules and listings
//	                          matching q (2 to 100 characters), grouped by kind, at most limit (5, up to 20) of each;
//	                          each result's kind and id (and agent_id) name the screen it opens
//
// The workspace is the key's; only its owner or an admin may search it. Mounted in the authed group.

type moneySearchBank interface {
	Search(ctx context.Context, workspaceID, query string, limit int) (economy.SearchResults, error)
}

func mountMoneySearchRoutes(r chi.Router, bank moneySearchBank) {
	r.Get("/v1/money/search", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "search is a workspace's: use a workspace's key")
			return
		}
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may search it")
			return
		}
		limit := 0
		if v := req.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				writeJSONErr(w, http.StatusBadRequest, "limit must be a positive whole number")
				return
			}
			limit = n
		}
		out, err := bank.Search(req.Context(), ws, req.URL.Query().Get("q"), limit)
		switch {
		case errors.Is(err, economy.ErrSearchQuery):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	})
}
