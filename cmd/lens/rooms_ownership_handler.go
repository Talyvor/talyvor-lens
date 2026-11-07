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
// B41.145 — the room's provisional leaderboard (internal/rooms/leaderboard.go):
//
//	GET /v1/rooms/{roomID}/leaderboard   {enabled, pool_bps, total_points, rows: [{workspace_id, workspace_name, points,
//	                                identity_checked, share_bps}], note, notice} to a member that accepted the room's
//	                                current terms; the identity-checked holders' share_bps add up to 10000
//
// B41.147 — objections to the frozen leaderboard, decided by three random members (internal/rooms/objections.go):
//
//	POST /v1/rooms/{roomID}/leaderboard/freeze           the room's owner, once: opens the 14-day objection window
//	GET  /v1/rooms/{roomID}/objections                   {frozen_at, objections_close_at, rules, objections: [{id,
//	                                                     contribution_id, holder_workspace_id, objector_workspace_id,
//	                                                     points, reason, status, raised_at, decide_by, decided_at,
//	                                                     panel: [{workspace_id, workspace_name, uphold, voted_at}]}]}
//	                                                     to a member that accepted the room's current terms
//	POST /v1/rooms/{roomID}/objections                   {contribution_id, points, reason}   any member, in the window:
//	                                                     Lens draws three members linked to neither side; 409 if it cannot
//	PUT  /v1/rooms/{roomID}/objections/{objectionID}/vote   {uphold}   a drawn panelist, within 14 days; two votes the
//	                                                     same way decide it, and upheld it changes the points
//
// B41.146 — the room's Gate tracker: shipped, used, paid (internal/rooms/gate.go):
//
//	GET /v1/rooms/{roomID}/gate              {thresholds, shipped: {status, note, weeks}, used: {status, people},
//	                                         paid: {status, note, usd_micros, payers}, met, notice} to a member that
//	                                         accepted the room's current terms; shipped and paid read
//	                                         needs_published_build until a room can publish a build
//	PUT /v1/rooms/{roomID}/gate-thresholds   {shipped_weeks, used_people, used_active_weeks, paid_usd_micros,
//	                                         paid_payers}   the room's owner; 409 for any value lowered
//
// B41.144 — a contribution's quality scored by two random reviewers not linked to its author (internal/rooms/reviews.go):
//
//	GET  /v1/rooms/{roomID}/contributions/{contributionID}/reviews     {quality_bps, reviews: [{reviewer_workspace_id,
//	                                     reviewer_workspace_name, score_bps, ring, drawn_at, scored_at}]} to a member
//	POST /v1/rooms/{roomID}/contributions/{contributionID}/reviewers   any member: Lens draws members at random into the
//	                                     proposed contribution's empty seats; nobody picks them
//	PUT  /v1/rooms/{roomID}/contributions/{contributionID}/review      {score_bps}   a drawn reviewer: 5000–15000 while it
//	                                     is proposed; once both scored, quality_bps is the median
//
// B41.150 — the room's contributor agreement, e-signed before points count (internal/rooms/agreement.go):
//
//	GET  /v1/rooms/{roomID}/agreement    {version, draft, marked, text, text_sha256, signature} to a member that accepted
//	                                     the room's current terms; signature is null until its workspace signs
//	POST /v1/rooms/{roomID}/agreement    {version, signed_name}   the workspace's owner or an admin, signed in as a
//	                                     person, signs the version it read: 201, or 200 with the signature already kept;
//	                                     409 for any version but the current one
//
// Capability room_equity is RED: unless the room's owner and the caller are both synthetic workspaces, every route
// here answers 404 until an operator records its clearance, and so does anyone who is not a member.
// mountRoomOwnershipRoutes mounts shared ownership (B41.141), the rate card (B41.142), the points ledger (B41.143), its
// reviewers (B41.144), the leaderboard (B41.145), the Gate tracker (B41.146), objections (B41.147) and the contributor
// agreement (B41.150).
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
	r.Get("/v1/rooms/{roomID}/agreement", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		a, err := store.Agreement(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, a)
	})
	r.Post("/v1/rooms/{roomID}/agreement", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			Version    string `json:"version"`
			SignedName string `json:"signed_name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"version", "signed_name"}: `+err.Error())
			return
		}
		a, created, err := store.SignAgreement(req.Context(), ws, user, chi.URLParam(req, "roomID"), in.Version, in.SignedName)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSONOK(w, status, a)
	}))
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
	r.Get("/v1/rooms/{roomID}/leaderboard", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		l, err := store.Leaderboard(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})
	r.Post("/v1/rooms/{roomID}/leaderboard/freeze", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		o, err := store.FreezeLeaderboard(req.Context(), ws, user, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, o)
	}))
	r.Get("/v1/rooms/{roomID}/objections", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		o, err := store.RoomObjections(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, o)
	})
	r.Post("/v1/rooms/{roomID}/objections", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.ObjectionRaise
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"contribution_id", "points", "reason"}: `+err.Error())
			return
		}
		o, err := store.RaiseObjection(req.Context(), ws, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, o)
	}))
	r.Put("/v1/rooms/{roomID}/objections/{objectionID}/vote", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			Uphold *bool `json:"uphold"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<10)).Decode(&in); err != nil || in.Uphold == nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"uphold": true or false}`)
			return
		}
		o, err := store.VoteObjection(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "objectionID"), *in.Uphold)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, o)
	}))
	r.Get("/v1/rooms/{roomID}/gate", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		g, err := store.GateTracker(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, g)
	})
	r.Put("/v1/rooms/{roomID}/gate-thresholds", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.GateThresholdsChange
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"shipped_weeks", "used_people", "used_active_weeks", "paid_usd_micros", "paid_payers"}: `+err.Error())
			return
		}
		t, err := store.SetGateThresholds(req.Context(), ws, user, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	}))
	r.Get("/v1/rooms/{roomID}/contributions/{contributionID}/reviews", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		v, err := store.ContributionReviews(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, v)
	})
	r.Post("/v1/rooms/{roomID}/contributions/{contributionID}/reviewers", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		v, err := store.DrawReviewers(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"))
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, v)
	}))
	r.Put("/v1/rooms/{roomID}/contributions/{contributionID}/review", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			ScoreBPS int `json:"score_bps"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"score_bps"}: `+err.Error())
			return
		}
		v, err := store.Review(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"), in.ScoreBPS)
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, v)
	}))
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
