package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/reqtrack"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B32.28 — ROOMS: open chats where people and their agents build a product together (internal/rooms).
//
//	POST  /v1/workspaces/{wsID}/rooms        {title, topic, description, visibility, terms: {split_rule, remix_share_bps,
//	                                         default_price_usd_micros, spend_policy}}   the workspace opens a room it owns
//	GET   /v1/rooms?topic=                   {rooms, joined}: the open public rooms by latest activity — the list of open
//	                                         chats — and the rooms the caller's workspace is in
//	GET   /v1/rooms/{roomID}                 the room, its current terms, its members and agents, and the caller's membership
//	POST  /v1/rooms/{roomID}/join            {terms_version}   join, accepting the room's current terms
//	PUT   /v1/rooms/{roomID}/terms           {split_rule, remix_share_bps, default_price_usd_micros, spend_policy}   the owner
//	                                         writes a new terms version; each member is asked again before its next contribution
//	PATCH /v1/rooms/{roomID}/members/{ws}    {role, may_spend, remove}   by the owner or an editor; a member may remove itself
//	POST  /v1/rooms/{roomID}/agents          {agent_id}   one of the caller's agents joins, as the caller's member
//
// B32.29 — private rooms, invites and rooms_plan_limits (internal/rooms/limits.go, invites.go):
//
//	POST   /v1/rooms/{roomID}/invites             {max_uses, expires_at}  an invite link to a private room, by its owner
//	                                              or an editor; its token is in this answer only
//	                                              {workspace_id}          the owner names a workspace, which then sees the
//	                                              room and joins it with POST /v1/rooms/{roomID}/join
//	GET    /v1/rooms/{roomID}/invites             the room's invites, without their tokens, for its owner and editors
//	DELETE /v1/rooms/{roomID}/invites/{inviteID}  revoke one: from then on its link answers 404
//	GET    /v1/room-invites/{token}               what a live link opens: the room and its current terms
//	POST   /v1/room-invites/{token}/join          {terms_version}   join the link's room, accepting its terms
//
// B32.30 — a room's messages (internal/rooms/messages.go):
//
//	POST   /v1/rooms/{roomID}/messages               {body}   a member that is not a viewer posts; 201 with the message
//	GET    /v1/rooms/{roomID}/messages?before=&after=&limit=   a page of messages, oldest first, with more and the room's
//	                                                 events_cursor; before and after are a message's cursor
//	PATCH  /v1/rooms/{roomID}/messages/{messageID}   {body}   its author edits a text message
//	DELETE /v1/rooms/{roomID}/messages/{messageID}   its author, or the room's owner or an editor; a tombstone stays
//	GET    /v1/rooms/{roomID}/events?after=          Server-Sent Events: the room's events after the cursor (or after
//	                                                 Last-Event-ID; from now when neither is sent), each with its message,
//	                                                 read once a second; each stream ends after roomEventStreamLife and the
//	                                                 client reconnects from its last id
//
// A public room's messages are read by everyone and a private room's by its members only; anyone else gets 404. In a
// public room a message carrying a secret or personal data is 422 with what the scan found, and writes nothing. A
// member's message past LENS_ROOM_MESSAGES_PER_MINUTE in a room is 429 with Retry-After.
//
// B32.31 — contributions: propose, fork with lineage, vote (internal/rooms/contributions.go):
//
//	POST  /v1/rooms/{roomID}/contributions        {kind, title, description, artifact, changelog, price_usd_micros, parents}
//	                                              a member that is not a viewer publishes a listing to the room — its
//	                                              price the room's default unless given — and the room gets a
//	                                              contribution message; 201 with the contribution
//	GET   /v1/rooms/{roomID}/contributions        the room's contributions, newest first, each with its tally and the
//	                                              caller's vote, for its members
//	GET   /v1/rooms/{roomID}/contributions/{id}   one, with its listing and the artifact of each version, for its members
//	POST  /v1/rooms/{roomID}/contributions/{id}/fork   {title, description, artifact, changelog, price_usd_micros}  the
//	                                              caller's own contribution built on it — the artifact as it is unless
//	                                              given — whose lineage pays the original the room's remix share
//	PUT   /v1/rooms/{roomID}/contributions/{id}/vote   {value: 1 or -1}   a member's vote; its next replaces it
//	PATCH /v1/rooms/{roomID}/contributions/{id}   {status: accepted or rejected}   by the room's owner or an editor
//
// A contribution is a marketplace listing with visibility room: the publish scan and review apply (422 with what the
// scan found), its owner and the room's members see it and open its artifact, anyone else gets 404, and the catalog
// never lists it. A member whose accepted terms are not the room's current ones is 409 until it accepts them.
// Contributing and forking take an Idempotency-Key, as publishing a listing does.
//
// B32.32 — the room's budget (internal/rooms/wallet.go). Opening a room opens its wallet: an agent of the owner's
// workspace, kind room, named "Room: <title>", with one proxy-scoped key nobody holds. GET /v1/rooms/{roomID} shows its
// members the wallet: its balance, its monthly limit — the room's budget — and what has been spent of it this month, its
// limit per request, its approval amount, the most the owner's plan allows, the spend policy and whether the caller may
// spend it, with why not. The owner funds it and sets its rules through Agent Wallets' own routes
// (/v1/workspaces/{ws}/agents/{wallet}/fund and /rules); a monthly limit above the plan's room_budget_max_usd, or none
// on a plan that sets one, is 402 naming rooms_plan_limits and the plan. Its approvals reach the owner's.
//
// B32.52 — reports, mutes and bans (internal/rooms/reports.go):
//
//	POST  /v1/rooms/{roomID}/reports                        {reason, details}   report the room
//	POST  /v1/rooms/{roomID}/messages/{messageID}/reports   {reason, details}   report one of its messages
//	PATCH /v1/rooms/{roomID}/members/{ws}                   {muted: true|false} or {banned: true|false}, by the owner or an
//	                                                        editor: a muted member posts nothing; a banned one is removed
//	                                                        and its join is 403 until it is unbanned
//
// The reason is malicious, injection, secret, personal_data, infringing, misleading, harassment, spam or other; a report is
// 201, or 200 with already_reported while the same workspace's report of it is still open. Anyone who can read the room
// reports it. A public room with LENS_ROOM_REPORTS_HIDE open reports from different workspaces leaves GET /v1/rooms, and
// GET /v1/rooms/{roomID} says under_review, until the operator reviews it (POST /v1/admin/rooms/{roomID}/moderate). A
// locked room is read-only: posting, editing, contributing, voting and running in it are 409; a closed room takes nothing.
//
// A private room answers 404 to everyone but its members and the workspaces its owner named. Joining with a
// terms_version that is not the room's current one, or joining a room that is locked or closed, is 409. Opening a room
// past the owner's plan's public_rooms or private_rooms, a member past members_per_room or an agent past
// agents_per_room is 402 naming rooms_plan_limits and the plan. Acting for a workspace in a room — opening, joining,
// changing members, inviting, bringing an agent — takes the workspace's owner or an admin, as publishing a listing
// does; reading takes any of its credentials.

// writeRoomPlanLimit writes a refusal under rooms_plan_limits as 402 naming the setting, the plan, the limit and the plan
// that allows more, and answers whether err was one. A room wallet's rules are refused with it too (B32.32).
func writeRoomPlanLimit(w http.ResponseWriter, err error) bool {
	var limit *rooms.PlanLimitError
	if !errors.As(err, &limit) {
		return false
	}
	writeJSONOK(w, http.StatusPaymentRequired, map[string]any{"error": limit.Detail, "setting": rooms.LimitsSetting,
		"plan": limit.Plan, "limit": limit.Limit, "max": limit.Max, "allows": limit.Allows})
	return true
}

func mountRoomRoutes(r chi.Router, store *rooms.Store) {
	writeErr := func(w http.ResponseWriter, err error) {
		var rate *rooms.RateError
		var refusal *rooms.ScanRefusal
		var refused *market.RefusedError
		switch {
		case writeRoomPlanLimit(w, err):
		case errors.As(err, &rate):
			w.Header().Set("Retry-After", strconv.Itoa(int(rate.RetryAfter.Seconds())))
			writeJSONOK(w, http.StatusTooManyRequests, map[string]any{"error": rate.Error(), "setting": rooms.MessagesPerMinuteSetting,
				"per_minute": rate.PerMinute})
		case errors.As(err, &refusal):
			writeJSONOK(w, http.StatusUnprocessableEntity, map[string]any{"error": refusal.Reason, "scan": refusal.Scan})
		case errors.As(err, &refused):
			writeJSONOK(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "scan": refused.Scan})
		case errors.Is(err, rooms.ErrInvalid), errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, rooms.ErrNotFound), errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, rooms.ErrForbidden):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, rooms.ErrConflict):
			writeJSONErr(w, http.StatusConflict, err.Error())
		default:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
	}
	// actor is the workspace acting in a room, and the user acting for it; "" when the credential names no workspace.
	actor := func(w http.ResponseWriter, req *http.Request) (ws, user string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusForbidden, "acting in a room needs a workspace's credential")
			return "", "", false
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			user = actx.UserID
		}
		return ws, user, true
	}
	status := func(created bool) int {
		if created {
			return http.StatusCreated
		}
		return http.StatusOK
	}

	r.Post("/v1/workspaces/{wsID}/rooms", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		var d rooms.Draft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10)).Decode(&d); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the room: "+err.Error())
			return
		}
		user := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			user = actx.UserID
		}
		room, err := store.Create(req.Context(), chi.URLParam(req, "wsID"), user, d)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, room)
	}))
	r.Get("/v1/rooms", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		open, err := store.OpenPublic(req.Context(), req.URL.Query().Get("topic"))
		if err != nil {
			writeErr(w, err)
			return
		}
		joined, err := store.Joined(req.Context(), ws)
		if err != nil {
			writeErr(w, err)
			return
		}
		invited, err := store.Invited(req.Context(), ws)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := map[string]any{"rooms": open, "joined": joined, "invited": invited}
		if ws != "" {
			// What the caller's plan lets it open, beside what it has open, so a new room can say why it cannot be.
			if out["limits"], err = store.Usage(req.Context(), ws); err != nil {
				writeErr(w, err)
				return
			}
		}
		writeJSONOK(w, http.StatusOK, out)
	})
	r.Get("/v1/rooms/{roomID}", func(w http.ResponseWriter, req *http.Request) {
		ws, admin := auth.WorkspaceIdentity(req.Context())
		room, err := store.Get(req.Context(), ws, admin, chi.URLParam(req, "roomID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, room)
	})
	report := func(w http.ResponseWriter, req *http.Request, messageID string) {
		reporter, _ := auth.WorkspaceIdentity(req.Context())
		if reporter == "" {
			writeJSONErr(w, http.StatusForbidden, "a report needs a workspace's credential")
			return
		}
		var in rooms.ReportDraft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {reason, details}: "+err.Error())
			return
		}
		rep, err := store.Report(req.Context(), reporter, chi.URLParam(req, "roomID"), messageID, in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(!rep.AlreadyMade), rep)
	}
	r.Post("/v1/rooms/{roomID}/reports", func(w http.ResponseWriter, req *http.Request) { report(w, req, "") })
	r.Post("/v1/rooms/{roomID}/messages/{messageID}/reports", func(w http.ResponseWriter, req *http.Request) {
		report(w, req, chi.URLParam(req, "messageID"))
	})
	r.Post("/v1/rooms/{roomID}/join", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			TermsVersion int `json:"terms_version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {terms_version}: "+err.Error())
			return
		}
		m, created, err := store.Join(req.Context(), ws, user, chi.URLParam(req, "roomID"), in.TermsVersion)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), m)
	}))
	r.Put("/v1/rooms/{roomID}/terms", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.TermsDraft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {split_rule, remix_share_bps, default_price_usd_micros, spend_policy}: "+err.Error())
			return
		}
		t, err := store.SetTerms(req.Context(), ws, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	}))
	r.Patch("/v1/rooms/{roomID}/members/{ws}", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.MemberChange
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {role, may_spend, remove} or {muted} or {banned}: "+err.Error())
			return
		}
		m, err := store.ChangeMember(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "ws"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	}))
	r.Post("/v1/rooms/{roomID}/agents", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {agent_id}: "+err.Error())
			return
		}
		a, created, err := store.AddAgent(req.Context(), ws, chi.URLParam(req, "roomID"), in.AgentID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), a)
	}))

	r.Post("/v1/rooms/{roomID}/invites", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.InviteDraft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {max_uses, expires_at} or {workspace_id}: "+err.Error())
			return
		}
		inv, created, err := store.CreateInvite(req.Context(), ws, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), inv)
	}))
	r.Get("/v1/rooms/{roomID}/invites", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		invites, err := store.Invites(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"invites": invites})
	})
	r.Delete("/v1/rooms/{roomID}/invites/{inviteID}", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		inv, err := store.RevokeInvite(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "inviteID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, inv)
	}))
	r.Get("/v1/room-invites/{token}", func(w http.ResponseWriter, req *http.Request) {
		p, err := store.PreviewInvite(req.Context(), chi.URLParam(req, "token"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, p)
	})
	r.Post("/v1/room-invites/{token}/join", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			TermsVersion int `json:"terms_version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {terms_version}: "+err.Error())
			return
		}
		m, roomID, created, err := store.JoinByInvite(req.Context(), ws, user, chi.URLParam(req, "token"), in.TermsVersion)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), map[string]any{"room_id": roomID, "member": m})
	}))

	readBody := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		var in struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {body}: "+err.Error())
			return "", false
		}
		return in.Body, true
	}
	cursor := func(v string) (int64, error) {
		if v == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("a cursor is a non-negative integer, got %q", v)
		}
		return n, nil
	}
	r.Post("/v1/rooms/{roomID}/messages", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		body, ok := readBody(w, req)
		if !ok {
			return
		}
		m, err := store.Post(req.Context(), ws, user, chi.URLParam(req, "roomID"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, m)
	}))
	r.Get("/v1/rooms/{roomID}/messages", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		q := req.URL.Query()
		before, err := cursor(q.Get("before"))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "before: "+err.Error())
			return
		}
		after, err := cursor(q.Get("after"))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "after: "+err.Error())
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		page, err := store.Messages(req.Context(), ws, chi.URLParam(req, "roomID"), before, after, limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, page)
	})
	r.Patch("/v1/rooms/{roomID}/messages/{messageID}", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		body, ok := readBody(w, req)
		if !ok {
			return
		}
		m, err := store.Edit(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "messageID"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	}))
	r.Delete("/v1/rooms/{roomID}/messages/{messageID}", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		m, err := store.Delete(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "messageID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	}))
	r.Get("/v1/rooms/{roomID}/events", func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		ws, _ := auth.WorkspaceIdentity(ctx)
		roomID := chi.URLParam(req, "roomID")
		from := req.URL.Query().Get("after")
		if from == "" {
			from = req.Header.Get("Last-Event-ID")
		}
		after, err := cursor(from)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "after: "+err.Error())
			return
		}
		if from == "" {
			after, err = store.EventsHead(ctx, ws, roomID)
		}
		var events []rooms.Event
		if err == nil {
			events, err = store.Events(ctx, ws, roomID, after)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		// Meant to stay open, so it is never /healthz's slowest request nor logged as slow (B27.11).
		reqtrack.MarkLongLived(ctx)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		send := func(events []rooms.Event) error {
			for _, e := range events {
				data, err := json.Marshal(e)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Cursor, data); err != nil { // nosemgrep: go.lang.security.audit.xss.no-fprintf-to-responsewriter.no-fprintf-to-responsewriter -- text/event-stream frame; data is json.Marshal output
					return err
				}
				after = e.Cursor
			}
			return rc.Flush()
		}
		if _, err := fmt.Fprintf(w, "retry: %d\n\n", roomEventRetry.Milliseconds()); err != nil { // nosemgrep: go.lang.security.audit.xss.no-fprintf-to-responsewriter.no-fprintf-to-responsewriter -- text/event-stream retry frame; the value is a constant
			return
		}
		if send(events) != nil {
			return
		}
		tick := time.NewTicker(roomEventPoll)
		defer tick.Stop()
		end := time.NewTimer(roomEventStreamLife)
		defer end.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-end.C:
				return
			case <-tick.C:
				events, err := store.Events(ctx, ws, roomID, after)
				if errors.Is(err, rooms.ErrNotFound) {
					// Removed from a private room: the stream says so once and ends.
					_, _ = fmt.Fprint(w, "event: gone\ndata: {}\n\n")
					_ = rc.Flush()
					return
				}
				if err != nil || send(events) != nil {
					return
				}
			}
		}
	})

	// readDraft reads a contribution's body and its Idempotency-Key.
	readDraft := func(w http.ResponseWriter, req *http.Request) (d rooms.ContributionDraft, key string, ok bool) {
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, market.MaxArtifactBytes+64<<10)).Decode(&d); err != nil && !errors.Is(err, io.EOF) {
			writeJSONErr(w, http.StatusBadRequest, "body must be the contribution: "+err.Error())
			return d, "", false
		}
		if key = req.Header.Get("Idempotency-Key"); len(key) > 128 {
			writeJSONErr(w, http.StatusBadRequest, "the Idempotency-Key must be at most 128 characters")
			return d, "", false
		}
		return d, key, true
	}
	r.Post("/v1/rooms/{roomID}/contributions", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		d, key, ok := readDraft(w, req)
		if !ok {
			return
		}
		c, created, err := store.Contribute(req.Context(), ws, user, chi.URLParam(req, "roomID"), key, d)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), c)
	}))
	r.Get("/v1/rooms/{roomID}/contributions", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		cs, err := store.Contributions(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"contributions": cs})
	})
	r.Get("/v1/rooms/{roomID}/contributions/{contributionID}", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		c, err := store.ReadContribution(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
	r.Post("/v1/rooms/{roomID}/contributions/{contributionID}/fork", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		d, key, ok := readDraft(w, req)
		if !ok {
			return
		}
		c, created, err := store.Fork(req.Context(), ws, user, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"), key, d)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), c)
	}))
	r.Put("/v1/rooms/{roomID}/contributions/{contributionID}/vote", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			Value int `json:"value"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {value: 1 or -1}: "+err.Error())
			return
		}
		c, err := store.Vote(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"), in.Value)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	}))
	r.Patch("/v1/rooms/{roomID}/contributions/{contributionID}", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {status: accepted or rejected}: "+err.Error())
			return
		}
		c, err := store.Decide(req.Context(), ws, user, chi.URLParam(req, "roomID"), chi.URLParam(req, "contributionID"), in.Status)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	}))
}

// A room's event stream reads room_events once a second — no LISTEN, which PgBouncer's transaction pooling cannot
// hold — and ends before the server's 30-second write timeout and the router's 60-second request timeout; the client
// reconnects after roomEventRetry from the last id it saw, and loses nothing.
var (
	roomEventPoll       = time.Second
	roomEventStreamLife = 25 * time.Second
	roomEventRetry      = time.Second
)

// roomActorOnly admits the workspace's owner or an admin — the rule publishing a listing uses — so an agent's own key
// never joins its workspace to a room or changes who is in one.
func roomActorOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin acts for it in a room")
			return
		}
		next(w, req)
	}
}

// B32.52 — the operator's half of room safety, registered in main.go behind requireAdminOrModerator:
//
//	GET  /v1/admin/rooms/reports              the rooms with an open report, most reporters first, each with its reports
//	                                          and whether it is off the public list
//	POST /v1/admin/rooms/{roomID}/moderate    {action: keep|lock|unlock|close, reason, actor}   keep resolves the room's
//	                                          open reports as kept and puts it back on the list; lock makes it read-only and
//	                                          close closes it, each resolving them; unlock opens a locked room again
//
// The operator is the one X-Talyvor-Operator names, or the body's actor; every action is an operator_audit row, written
// with it. Locking or closing a room that is not open, or unlocking one that is not locked, is 409.

func newRoomReportQueueHandler(store *rooms.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q, err := store.ReportQueue(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"rooms": q, "hide_at": store.ReportsHide(), "setting": rooms.ReportsHideSetting})
	})
}

func newRoomModerateHandler(store *rooms.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			rooms.Moderation
			Actor string `json:"actor"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {action, reason, actor}: "+err.Error())
			return
		}
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		out, err := store.Moderate(req.Context(), actor, chi.URLParam(req, "roomID"), in.Moderation)
		switch {
		case err == nil:
			writeJSONOK(w, http.StatusOK, out)
		case errors.Is(err, rooms.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, rooms.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, rooms.ErrConflict):
			writeJSONErr(w, http.StatusConflict, err.Error())
		default:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
	})
}
