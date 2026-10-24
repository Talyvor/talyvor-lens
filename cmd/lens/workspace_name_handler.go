package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/storedanswers"
)

// maxWorkspaceNameLen is the longest name a workspace may take, in characters (B46.15).
const maxWorkspaceNameLen = 80

// mountWorkspaceNameRoutes is B46.15: the workspace's owner names the workspace and reads the name back.
// Another workspace's credential never reaches these — workspaceIsolationMiddleware refuses it first.
//
//	GET /v1/workspaces/{wsID}/name → {"id", "name"}
//	PUT /v1/workspaces/{wsID}/name  {"name"} → {"id", "name"}; owner or admin only; the change is in the audit trail.
func mountWorkspaceNameRoutes(r chi.Router, pool *pgxpool.Pool, cache interface{ CacheName(wsID, name string) }) {
	r.Get("/v1/workspaces/{wsID}/name", func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		var name string
		err := pool.QueryRow(req.Context(), `SELECT name FROM workspaces WHERE id = $1`, wsID).Scan(&name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeJSONErr(w, http.StatusNotFound, "workspace not found")
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the name could not be read")
		default:
			writeJSONOK(w, http.StatusOK, map[string]string{"id": wsID, "name": name})
		}
	})

	r.Put("/v1/workspaces/{wsID}/name", func(w http.ResponseWriter, req *http.Request) {
		who, ok := storedanswers.OwnerOrAdmin(req.Context())
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may name it")
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name": "<workspace name>"}`)
			return
		}
		name := strings.TrimSpace(body.Name)
		if n := utf8.RuneCountInString(name); n < 1 || n > maxWorkspaceNameLen {
			writeJSONErr(w, http.StatusBadRequest, fmt.Sprintf("a workspace name is 1 to %d characters", maxWorkspaceNameLen))
			return
		}
		if strings.IndexFunc(name, unicode.IsControl) >= 0 {
			writeJSONErr(w, http.StatusBadRequest, "a workspace name cannot contain control characters")
			return
		}

		wsID := chi.URLParam(req, "wsID")
		ctx := req.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the name could not be saved")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var old string
		err = tx.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = $1 FOR UPDATE`, wsID).Scan(&old)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSONErr(w, http.StatusNotFound, "workspace not found")
			return
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE workspaces SET name = $2, updated_at = NOW() WHERE id = $1`, wsID, name)
		}
		if err == nil {
			_, err = operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{
				Actor:  who,
				Action: "workspace.rename",
				Target: "workspace:" + wsID,
				Detail: fmt.Sprintf("%q → %q", old, name),
			})
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the name could not be saved")
			return
		}
		cache.CacheName(wsID, name)
		writeJSONOK(w, http.StatusOK, map[string]string{"id": wsID, "name": name})
	})
}
