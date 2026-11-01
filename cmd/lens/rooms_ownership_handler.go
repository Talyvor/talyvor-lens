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
// Capability room_equity is RED: unless the room's owner and the caller are both synthetic workspaces, both routes
// answer 404 until an operator records its clearance, and so does anyone who is not a member.
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
}
