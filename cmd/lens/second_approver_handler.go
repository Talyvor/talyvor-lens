package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B28.315 — a Business workspace's second approver (internal/economy/second_approver.go):
//
//	GET  /v1/workspaces/{wsID}/second-approver            {user_id, above_minor, currency, set_by, set_at}; empty when none
//	POST /v1/workspaces/{wsID}/second-approver/challenge  {user_id, above_minor, currency} → {challenge, allow_credentials}
//	PUT  /v1/workspaces/{wsID}/second-approver            {user_id, above_minor, currency, assertion}; {} with
//	                                                      user_id "" and above_minor 0 clears it
//
// Reading is the owner's or an admin's, as the approvals are; setting it is the owner's alone, on their own signed-in
// session, signed with a passkey once the workspace has one. A Personal workspace has none: 404 with code not_business.

type secondApproverStore interface {
	SecondApprover(ctx context.Context, workspaceID string) (economy.SecondApprover, error)
	SecondApproverChallenge(ctx context.Context, workspaceID string, in economy.SecondApprover) (string, []string, error)
	SetSecondApprover(ctx context.Context, workspaceID, by string, in economy.SecondApprover, a *passkey.Assertion) (economy.SecondApprover, error)
}

func mountSecondApproverRoutes(r chi.Router, store secondApproverStore) {
	write := func(w http.ResponseWriter, out any, err error) {
		switch {
		case errors.Is(err, economy.ErrNotBusiness):
			writeJSONOK(w, http.StatusNotFound, map[string]any{"error": "a Personal account has no second approver", "code": "not_business"})
		case errors.Is(err, economy.ErrWorkspaceNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrSecondApproverInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPasskeyRequired), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the second approver could not be read or saved")
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	}
	type body struct {
		economy.SecondApprover
		Assertion *passkey.Assertion `json:"assertion"`
	}
	read := func(w http.ResponseWriter, req *http.Request) (body, bool) {
		var in body
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"user_id", "above_minor", "currency": "GBP" | "EUR" | "USD" | "USDC", "assertion"}`)
			return in, false
		}
		return in, true
	}
	r.Get("/v1/workspaces/{wsID}/second-approver", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		out, err := store.SecondApprover(req.Context(), chi.URLParam(req, "wsID"))
		write(w, out, err)
	}))
	r.Post("/v1/workspaces/{wsID}/second-approver/challenge", businessOwnerOnly(func(w http.ResponseWriter, req *http.Request, _ string) {
		in, ok := read(w, req)
		if !ok {
			return
		}
		c, creds, err := store.SecondApproverChallenge(req.Context(), chi.URLParam(req, "wsID"), in.SecondApprover)
		write(w, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"}, err)
	}))
	r.Put("/v1/workspaces/{wsID}/second-approver", businessOwnerOnly(func(w http.ResponseWriter, req *http.Request, by string) {
		in, ok := read(w, req)
		if !ok {
			return
		}
		out, err := store.SetSecondApprover(req.Context(), chi.URLParam(req, "wsID"), by, in.SecondApprover, in.Assertion)
		write(w, out, err)
	}))
}
