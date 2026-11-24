package storedanswers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/workspace"
)

// The routes (B21.3), mounted in cmd/lens/main.go:
//
//	GET    /v1/workspaces/{wsID}/stored-answers      counts by scope
//	DELETE /v1/workspaces/{wsID}/stored-answers      {"scope":"shared"|"all","confirm":"<workspace>"}
//	POST   /v1/workspaces/{wsID}/deletion-requests   {"note":"…"} — ask Talyvor to delete everything
//	GET    /v1/workspaces/{wsID}/deletion-requests   their status
//	GET    /v1/admin/deletion-requests               the operator's queue (?all=true for done ones too)
//	POST   /v1/admin/deletion-requests/{id}/complete delete everything for that workspace, mark done
//
// workspaceIsolationMiddleware has already bound {wsID} to the caller's credential; the two admin
// routes are mounted behind requireAdmin. The DELETE and the request are further limited to the
// workspace's owner or admin (OwnerOrAdmin).

// Deleter is what the routes and `lens deletion-requests` need; *Store satisfies it.
type Deleter interface {
	Counts(ctx context.Context, wsID string) (Counts, error)
	Delete(ctx context.Context, wsID string, scope Scope, by string, requestID int64) (Counts, error)
	FileRequest(ctx context.Context, wsID, by, note string) (Request, error)
	Requests(ctx context.Context, wsID string) ([]Request, error)
	List(ctx context.Context, openOnly bool) ([]Request, error)
	Complete(ctx context.Context, id int64, by string) (Request, Counts, error)
}

// WorkspaceLookup finds a workspace, to check the typed confirmation against its name.
type WorkspaceLookup interface {
	GetWorkspace(id string) (*workspace.Workspace, bool)
}

// OwnerOrAdmin reports whether the caller may delete this workspace's data (or, B19.1, move its money
// between agents), and names it for the audit row. Allowed: the operator (the global admin key); the
// workspace's owner, whose provisioned session token carries auth.ScopeKeys — a scope a tenant cannot
// grant a key of its own (tenant.ValidScopes); and a workspace key its owner created with the admin
// scope. Refused: the browser chat's session key, and any proxy or analytics key.
func OwnerOrAdmin(ctx context.Context) (who string, ok bool) {
	actx := auth.GetAuthContext(ctx)
	if actx == nil {
		return "", false
	}
	switch {
	case actx.IsAdmin:
		return "operator", true
	case actx.AuthMethod == auth.MethodSessionKey:
		return "", false
	case actx.Role != "":
		// B28.342: a role session is never the owner. A handler that admits one names its roles (OwnerOrRole).
		return "", false
	case actx.HasScope(auth.ScopeKeys) || actx.HasScope(auth.ScopeAdmin):
		who := actx.AuthMethod
		if actx.UserID != "" {
			who += ":user:" + actx.UserID
		}
		if actx.APIKeyID != "" {
			who += ":key:" + actx.APIKeyID
		}
		return who, true
	}
	return "", false
}

// OwnerOrRole is OwnerOrAdmin that also admits a role session (B28.342) holding one of roles, named for the audit row
// as the person and their role. Only the handlers of the routes a role may reach (auth.RoleAllows) call it, each
// naming the roles it admits.
func OwnerOrRole(ctx context.Context, roles ...string) (who string, ok bool) {
	if actx := auth.GetAuthContext(ctx); actx != nil && actx.Role != "" && !actx.IsAdmin {
		if actx.AuthMethod != auth.MethodJWT || actx.UserID == "" || !slices.Contains(roles, actx.Role) {
			return "", false
		}
		return auth.MethodJWT + ":user:" + actx.UserID + ":role:" + actx.Role, true
	}
	return OwnerOrAdmin(ctx)
}

// CountsHandler serves GET /v1/workspaces/{wsID}/stored-answers.
func CountsHandler(store Deleter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := store.Counts(r.Context(), chi.URLParam(r, "wsID"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeOK(w, http.StatusOK, c)
	}
}

// DeleteHandler serves DELETE /v1/workspaces/{wsID}/stored-answers.
func DeleteHandler(store Deleter, wsm WorkspaceLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := chi.URLParam(r, "wsID")
		who, ok := OwnerOrAdmin(r.Context())
		if !ok {
			writeErr(w, http.StatusForbidden, "only the workspace's owner or an admin can delete its stored answers")
			return
		}
		var in struct {
			Scope   Scope  `json:"scope"`
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if !in.Scope.Valid() {
			writeErr(w, http.StatusBadRequest, `scope must be "shared" or "all"`)
			return
		}
		ws, found := wsm.GetWorkspace(wsID)
		if !found || in.Confirm == "" || (in.Confirm != ws.ID && in.Confirm != ws.Name) {
			writeErr(w, http.StatusBadRequest, "confirm must be this workspace's name, typed exactly — this cannot be undone")
			return
		}
		deleted, err := store.Delete(r.Context(), wsID, in.Scope, who, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("stored answers deleted", slog.String("workspace_id", wsID), slog.String("scope", string(in.Scope)),
			slog.String("by", who), slog.Any("deleted", deleted))
		writeOK(w, http.StatusOK, map[string]any{"scope": in.Scope, "deleted": deleted})
	}
}

// FileRequestHandler serves POST /v1/workspaces/{wsID}/deletion-requests.
func FileRequestHandler(store Deleter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := OwnerOrAdmin(r.Context())
		if !ok {
			writeErr(w, http.StatusForbidden, "only the workspace's owner or an admin can ask for its data to be deleted")
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
				writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
		req, err := store.FileRequest(r.Context(), chi.URLParam(r, "wsID"), who, in.Note)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("deletion request filed", slog.Int64("id", req.ID), slog.String("workspace_id", req.WorkspaceID),
			slog.String("by", who))
		writeOK(w, http.StatusCreated, req)
	}
}

// RequestStatusHandler serves GET /v1/workspaces/{wsID}/deletion-requests.
func RequestStatusHandler(store Deleter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqs, err := store.Requests(r.Context(), chi.URLParam(r, "wsID"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeOK(w, http.StatusOK, map[string]any{"requests": reqs})
	}
}

// AdminListHandler serves GET /v1/admin/deletion-requests. Mount it behind requireAdmin.
func AdminListHandler(store Deleter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqs, err := store.List(r.Context(), r.URL.Query().Get("all") != "true")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeOK(w, http.StatusOK, map[string]any{"requests": reqs})
	}
}

// AdminCompleteHandler serves POST /v1/admin/deletion-requests/{id}/complete. Mount it behind requireAdmin.
func AdminCompleteHandler(store Deleter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "id must be a positive integer")
			return
		}
		req, deleted, err := store.Complete(r.Context(), id, "operator")
		switch {
		case errors.Is(err, ErrNotFound):
			writeErr(w, http.StatusNotFound, err.Error())
			return
		case errors.Is(err, ErrAlreadyDone):
			writeErr(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("deletion request completed", slog.Int64("id", id), slog.String("workspace_id", req.WorkspaceID),
			slog.Any("deleted", deleted))
		writeOK(w, http.StatusOK, map[string]any{"request": req, "deleted": deleted, "kept": KeptRecords})
	}
}

func writeOK(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeOK(w, status, map[string]string{"error": msg})
}
