package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/rooms"
)

// B41.141 — shared ownership in a room, intent only (internal/rooms/ownership.go):
//
//	GET /v1/rooms/{roomID}/shared-ownership    {enabled, pool_bps, notice}   to a member that accepted the room's
//	                                           current terms (409 to one that has not)
//	PUT /v1/rooms/{roomID}/shared-ownership    {enabled, pool_bps}   the room's owner; pool_bps 1000–5000, 2500 when left out
//
// B41.142 — the room's rate card, points for each size of work, fixed before the work starts (internal/rooms/ratecard.go):
//
//	GET /v1/rooms/{roomID}/rate-card     {pool_points, item_cap_points, sizes: [{size, hours, points_per_hour, points}]}
//	                                     to a member that accepted the room's current terms
//	PUT /v1/rooms/{roomID}/rate-card     {pool_points, sizes: [{size, hours, points_per_hour}]}   the room's owner; no
//	                                     size worth more than 5% of pool_points
//	PUT /v1/rooms/{roomID}/contributions/{contributionID}/size   {size}   its author or the room's owner or an editor:
//	                                     fixes the size's points on it; 409 for a raise once it is accepted
//
// B41.143 — the room's points ledger, size × quality × survival, append-only (internal/rooms/points.go):
//
//	GET /v1/rooms/{roomID}/points   {total_points, holders: [{workspace_id, workspace_name, points, identity_checked}],
//	                                entries: [...]} to a member that accepted the room's current terms
//
// Capability room_equity is RED: unless the room's owner and the caller are both synthetic workspaces, every route
// here answers 404 until an operator records its clearance, and so does anyone who is not a member.
// mountRoomOwnershipRoutes mounts shared ownership (B41.141), the rate card (B41.142) and the points ledger (B41.143).
func mountRoomOwnershipRoutes(r chi.Router, store *rooms.Store) {
	actor := func(w http.ResponseWriter, req *http.Request) (ws, user string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusNotFound, "no such room")
			return "", "", false
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			user = actx.UserID
		}
		return ws, user, true
	}
	r.Get("/v1/rooms/{roomID}/shared-ownership", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		o, err := store.SharedOwnership(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, o)
	})
	r.Put("/v1/rooms/{roomID}/shared-ownership", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.OwnershipChange
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"enabled", "pool_bps"}: `+err.Error())
			return
		}
		o, err := store.SetSharedOwnership(req.Context(), ws, user, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, o)
	}))
	r.Get("/v1/rooms/{roomID}/rate-card", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		rc, err := store.RateCard(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, rc)
	})
	r.Get("/v1/rooms/{roomID}/points", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		p, err := store.Points(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, p)
	})
	r.Put("/v1/rooms/{roomID}/rate-card", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.RateCardChange
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"pool_points", "sizes"}: `+err.Error())
			return
		}
		rc, err := store.SetRateCard(req.Context(), ws, user, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, rc)
	}))
	r.Put("/v1/rooms/{roomID}/contributions/{contributionID}/size", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			Size string `json:"size"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"size"}: `+err.Error())
			return
		}
		c, err := store.ClaimSize(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"), in.Size)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	}))
}
