package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B41.115 — connected access: the workspace's permission mandates, device keys and passkeys on one list, each with
// what it may do, when it was last used, when it expires and the route that revokes it (economy/connected_access.go).
//
//	GET /v1/money/access — {"items": [{kind, id, name, may, created_at, last_used_at, expires_at, status, revoke}]}
//
// The owner's or an admin's. Mounted in the authed group.

type connectedAccessBank interface {
	ConnectedAccess(ctx context.Context, workspaceID string) ([]economy.AccessItem, error)
}

func mountConnectedAccessRoutes(r chi.Router, bank connectedAccessBank) {
	r.Get("/v1/money/access", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); ws == "" || !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may list what may act for it")
			return
		}
		items, err := bank.ConnectedAccess(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"items": items})
	})
}
