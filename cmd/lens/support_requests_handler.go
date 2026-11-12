package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/support"
)

// support_requests_handler.go — B59.27: support and complaints, closing the account, and the workspace's data
// (internal/support):
//
//	POST /v1/workspaces/{wsID}/requests                {"kind", "message"} — files a request, received; a complaint
//	                                                   answers with its reference. Closing the account answers 409
//	                                                   balances_not_empty, naming what each currency holds, while
//	                                                   any account holds money
//	GET  /v1/workspaces/{wsID}/requests                {"requests":[...]} — the owner reads every request of the
//	                                                   workspace, anyone else signed in to it the ones they filed
//	GET  /v1/workspaces/{wsID}/requests/{id}/export    the owner downloads a data export once Talyvor marked it done
//	GET  /v1/admin/requests                            ?state=&kind=&workspace= — the operator's inbox, oldest first
//	PUT  /v1/admin/requests/{id}                       {"state", "answer", "actor"} — the operator answers, recorded in
//	                                                   the operator audit trail
//
// Support and complaints are anyone's who signs in to the workspace; closing the account, the export and deletion are
// the owner's. No agent key, session key or assistant connection files one. The authed group's tenant check binds
// {wsID} to the caller's own workspace.

type supportRequests interface {
	File(ctx context.Context, workspaceID string, kind support.Kind, by, message string) (support.Request, error)
	List(ctx context.Context, workspaceID, by string) ([]support.Request, error)
	Export(ctx context.Context, w io.Writer, workspaceID string, id int64) error
}

// requestFiler is who is filing or reading, as kept on the request, whether they are the workspace's owner, and false
// for a caller who is neither the owner nor a person signed in to the workspace.
func requestFiler(ctx context.Context) (by string, owner, ok bool) {
	who, owner := storedanswers.OwnerOrAdmin(ctx)
	actx := auth.GetAuthContext(ctx)
	if actx != nil && actx.UserID != "" {
		by = "user:" + actx.UserID
	}
	switch {
	case owner && by == "":
		return who, true, true
	case owner:
		return by, true, true
	case actx != nil && actx.AuthMethod == auth.MethodJWT && by != "":
		return by, false, true
	}
	return "", false, false
}

func mountSupportRequestRoutes(r chi.Router, store supportRequests) {
	r.Post("/v1/workspaces/{wsID}/requests", func(w http.ResponseWriter, req *http.Request) {
		by, owner, ok := requestFiler(req.Context())
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "only a person signed in to the workspace may file a request")
			return
		}
		var in struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"kind", "message"}`)
			return
		}
		kind, err := support.ParseKind(in.Kind)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if kind.OwnerOnly() && !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner may ask to close the account or for its data")
			return
		}
		out, err := store.File(req.Context(), chi.URLParam(req, "wsID"), kind, by, in.Message)
		var held *support.BalancesRefusal
		switch {
		case errors.As(err, &held):
			writeJSONOK(w, http.StatusConflict, map[string]any{"error": api.Refusal(held.Error()), "code": "balances_not_empty",
				"field": "balances", "balances": held.Balances})
		case errors.Is(err, support.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not file the request")
		default:
			writeJSONOK(w, http.StatusCreated, out)
		}
	})
	r.Get("/v1/workspaces/{wsID}/requests", func(w http.ResponseWriter, req *http.Request) {
		by, owner, ok := requestFiler(req.Context())
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "only a person signed in to the workspace may read its requests")
			return
		}
		if owner {
			by = ""
		}
		out, err := store.List(req.Context(), chi.URLParam(req, "wsID"), by)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the requests")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"requests": out})
	})
	r.Get("/v1/workspaces/{wsID}/requests/{id}/export", func(w http.ResponseWriter, req *http.Request) {
		if _, owner, _ := requestFiler(req.Context()); !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner may download its data")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
		if err != nil {
			writeJSONErr(w, http.StatusNotFound, support.ErrNotFound.Error())
			return
		}
		bw := &headerOnWrite{w: w}
		err = store.Export(req.Context(), bw, chi.URLParam(req, "wsID"), id)
		switch {
		case bw.started && err != nil:
			// The download is already under way: a status can no longer be sent, so the body ends short of its
			// closing brace and does not parse.
			return
		case errors.Is(err, support.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, support.ErrNotReady):
			writeJSONOK(w, http.StatusConflict, map[string]any{"error": api.Refusal(err.Error()), "code": "not_ready"})
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not build the export")
		}
	})
}

// headerOnWrite sends the download's headers with its first byte, so a refusal before it still answers with a status.
type headerOnWrite struct {
	w       http.ResponseWriter
	started bool
}

func (h *headerOnWrite) Write(p []byte) (int, error) {
	if !h.started {
		h.started = true
		h.w.Header().Set("Content-Type", "application/json")
		h.w.Header().Set("Content-Disposition", `attachment; filename="talyvor-data.json"`)
		h.w.WriteHeader(http.StatusOK)
	}
	return h.w.Write(p)
}

// newRequestInboxHandler — GET /v1/admin/requests: every workspace's requests, the oldest first, behind
// requireAdminOrOperatorRead.
func newRequestInboxHandler(store *support.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		var f support.InboxFilter
		if s := q.Get("state"); s != "" {
			st, err := support.ParseState(s)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			f.State = st
		}
		if k := q.Get("kind"); k != "" {
			kind, err := support.ParseKind(k)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			f.Kind = kind
		}
		f.WorkspaceID = q.Get("workspace")
		out, err := store.Inbox(req.Context(), f)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the requests")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"requests": out})
	})
}

// newRequestAnswerHandler — PUT /v1/admin/requests/{id} {"state", "answer", "actor"}: behind requireAdmin, so no
// workspace key, session or role reaches it, whatever its scopes.
func newRequestAnswerHandler(store *support.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			State  string `json:"state"`
			Answer string `json:"answer"`
			Actor  string `json:"actor"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"state", "answer", "actor"}`)
			return
		}
		state, err := support.ParseState(in.State)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
		if err != nil {
			writeJSONErr(w, http.StatusNotFound, support.ErrNotFound.Error())
			return
		}
		out, err := store.Answer(req.Context(), id, state, in.Answer, actor)
		switch {
		case errors.Is(err, support.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, support.ErrInvalid), errors.Is(err, operatoraudit.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not answer the request")
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	})
}
