package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/areas"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/operatoraudit"
)

// areas_handler.go — B59.1: the areas a workspace has. Everything outside the launch pack is an area, off for
// every workspace until the operator switches it on for one.
//
//	GET /v1/workspaces/{wsID}/areas                 {"areas":[...]} — the areas this workspace has; the web app and
//	                                                the phone show an area only when it is listed here
//	PUT /v1/admin/workspaces/{wsID}/areas/{area}    {"on":true|false, "actor"} — the operator switches one area on
//	                                                or off for one workspace, recorded in the operator audit trail

// areaChecker is what areaGate asks; *areas.Store answers it.
type areaChecker interface {
	Has(ctx context.Context, workspaceID string, a areas.Area) (bool, error)
}

// areaGate answers 404 for a route of an area the caller's workspace does not have, exactly as a route that is
// not there answers, so the answer never names the area. It runs in the authed group after auth and the tenant
// check, where the route is already matched and the caller known. The operator's admin key passes.
func areaGate(store areaChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			area, ok := areas.Of(chi.RouteContext(req.Context()).RoutePattern())
			if !ok {
				next.ServeHTTP(w, req)
				return
			}
			actx := auth.GetAuthContext(req.Context())
			if actx != nil && actx.IsAdmin {
				next.ServeHTTP(w, req)
				return
			}
			if actx == nil || actx.WorkspaceID == "" {
				http.NotFound(w, req)
				return
			}
			has, err := store.Has(req.Context(), actx.WorkspaceID, area)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "could not read the workspace's areas")
				return
			}
			if !has {
				http.NotFound(w, req)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// mountAreaRoutes mounts the workspace's read of its own areas; the authed group's tenant check keeps it to the
// workspace's own credentials and the operator.
func mountAreaRoutes(r chi.Router, store *areas.Store) {
	r.Get("/v1/workspaces/{wsID}/areas", func(w http.ResponseWriter, req *http.Request) {
		on, err := store.List(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the workspace's areas")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"areas": on})
	})
}

// newAreaSwitchHandler — PUT /v1/admin/workspaces/{wsID}/areas/{area} {"on", "actor"}: behind requireAdmin, so no
// workspace key, session or role reaches it, whatever its scopes.
func newAreaSwitchHandler(store *areas.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			On    *bool  `json:"on"`
			Actor string `json:"actor"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.On == nil {
			writeJSONErr(w, http.StatusBadRequest, "on is required")
			return
		}
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		wsID := chi.URLParam(req, "wsID")
		area, err := areas.Parse(chi.URLParam(req, "area"))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := store.Switch(req.Context(), wsID, area, *in.On, actor); err != nil {
			if errors.Is(err, operatoraudit.ErrInvalid) {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONErr(w, http.StatusInternalServerError, "could not switch the area")
			return
		}
		on, err := store.List(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the workspace's areas")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"areas": on})
	})
}
