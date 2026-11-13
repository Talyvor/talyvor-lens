package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/approvalchat"
	"github.com/talyvor/lens/internal/plans"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B56.16 — Slack and Teams approvals (internal/approvalchat). The owner or an admin saves the workspace's incoming
// webhook; it goes in and never comes back out, every answer naming its kind and host only. Registered only while
// custody is armed (LENS_PROVIDER_SECRET_KEK); {wsID} is bound to the caller's credential like its siblings. None of
// these decides an approval: that is done in Talyvor, with the passkey.
func mountApprovalChatRoutes(r chi.Router, s *approvalchat.Store) {
	r.Get("/v1/workspaces/{wsID}/approval-chat", newApprovalChatGetHandler(s))
	r.Put("/v1/workspaces/{wsID}/approval-chat", newApprovalChatPutHandler(s))
	r.Delete("/v1/workspaces/{wsID}/approval-chat", newApprovalChatDeleteHandler(s))
}

// GET /v1/workspaces/{wsID}/approval-chat → {"on": the plan has it, "hook": {"kind", "host", "updated_at"} or null}
func newApprovalChatGetHandler(s *approvalchat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		on, err := s.On(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		hook, err := s.Get(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"on": on, "hook": hook})
	}
}

// PUT /v1/workspaces/{wsID}/approval-chat {"kind": "slack" or "teams", "url": "..."} → {"kind", "host", "updated_at"}
func newApprovalChatPutHandler(s *approvalchat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin connects Slack or Teams")
			return
		}
		var body struct {
			Kind string `json:"kind"`
			URL  string `json:"url"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"kind": "slack" or "teams", "url": "<the incoming webhook>"}`)
			return
		}
		h, err := s.Put(req.Context(), chi.URLParam(req, "wsID"), body.Kind, body.URL)
		switch {
		case errors.Is(err, approvalchat.ErrInvalidHook):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, plans.ErrRefused):
			writeJSONErr(w, http.StatusPaymentRequired, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the webhook could not be saved")
		default:
			writeJSONOK(w, http.StatusOK, h)
		}
	}
}

// DELETE /v1/workspaces/{wsID}/approval-chat → 204, or 404 when none was saved.
func newApprovalChatDeleteHandler(s *approvalchat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin disconnects Slack or Teams")
			return
		}
		gone, err := s.Delete(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !gone {
			writeJSONErr(w, http.StatusNotFound, "no Slack or Teams webhook is saved")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
