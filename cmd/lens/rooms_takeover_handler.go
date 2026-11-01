package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/rooms"
)

// B41.148 — a room's owner can change (internal/rooms/takeover.go):
//
//	POST   /v1/rooms/{roomID}/owner           {workspace_id}   the owner hands the room to a member
//	GET    /v1/rooms/{roomID}/takeover        the owner's last activity, the latest notice, each voting member's points
//	                                          and vote, and the tally — to the room's members
//	POST   /v1/rooms/{roomID}/takeover        a member gives notice to an owner inactive for 60 days
//	DELETE /v1/rooms/{roomID}/takeover        the owner answers the notice and keeps the room
//	PUT    /v1/rooms/{roomID}/takeover/vote   {candidate_workspace_id}   14 days after an unanswered notice; the first
//	                                          candidate whose voters hold most of the points becomes the owner
//	POST   /v1/rooms/{roomID}/fork            14 days after an unanswered notice: a new room the member owns, naming
//	                                          the original as forked_from_room_id
func mountRoomTakeoverRoutes(r chi.Router, store *rooms.Store) {
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
	answer := func(w http.ResponseWriter, status int, v any, err error) {
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, status, v)
	}
	r.Post("/v1/rooms/{roomID}/owner", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {workspace_id}: "+err.Error())
			return
		}
		d, err := store.TransferOwner(req.Context(), ws, chi.URLParam(req, "roomID"), in.WorkspaceID)
		answer(w, http.StatusOK, d, err)
	}))
	r.Get("/v1/rooms/{roomID}/takeover", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		t, err := store.Takeover(req.Context(), ws, chi.URLParam(req, "roomID"))
		answer(w, http.StatusOK, t, err)
	})
	r.Post("/v1/rooms/{roomID}/takeover", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		t, err := store.GiveNotice(req.Context(), ws, chi.URLParam(req, "roomID"))
		answer(w, http.StatusCreated, t, err)
	}))
	r.Delete("/v1/rooms/{roomID}/takeover", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		t, err := store.AnswerNotice(req.Context(), ws, chi.URLParam(req, "roomID"))
		answer(w, http.StatusOK, t, err)
	}))
	r.Put("/v1/rooms/{roomID}/takeover/vote", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			CandidateWorkspaceID string `json:"candidate_workspace_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {candidate_workspace_id}: "+err.Error())
			return
		}
		t, err := store.VoteLead(req.Context(), ws, chi.URLParam(req, "roomID"), in.CandidateWorkspaceID)
		answer(w, http.StatusOK, t, err)
	}))
	r.Post("/v1/rooms/{roomID}/fork", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		d, err := store.ForkRoom(req.Context(), ws, user, chi.URLParam(req, "roomID"))
		answer(w, http.StatusCreated, d, err)
	}))
}
