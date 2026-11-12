package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B59.30 — a Business workspace's details, ready for the partner's identity flow (internal/economy/business_details.go):
//
//	GET /v1/workspaces/{wsID}/business   {company_name, registration_number, country, people: [{name, role, share_bps}]}
//	PUT /v1/workspaces/{wsID}/business   the same, replacing what was saved; role is director, shareholder or other, and
//	                                     share_bps the person's share in hundredths of a percent (2500 is 25%)
//
// Both are the owner's alone, on their own signed-in session: never a member, the operator, an API key or an agent's
// key, as they name people. A Personal workspace has no business details: 404 with code not_business. Each change is
// in the audit trail. The company check (POST .../verification/company) takes from these whatever it is not given.

type businessDetailsStore interface {
	BusinessDetails(ctx context.Context, workspaceID string) (economy.BusinessDetails, error)
	SetBusinessDetails(ctx context.Context, workspaceID, by string, in economy.BusinessDetails) (economy.BusinessDetails, error)
}

// businessOwnerOnly admits only the workspace's owner on their own session (a person's token carrying auth.ScopeKeys for
// this workspace), by recoveryOwner's rule without its sign-in age, and hands on who they are for the audit trail.
func businessOwnerOnly(next func(w http.ResponseWriter, req *http.Request, by string)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actx := auth.GetAuthContext(req.Context())
		if actx == nil || actx.AuthMethod != auth.MethodJWT || actx.UserID == "" || !actx.HasScope(auth.ScopeKeys) ||
			actx.WorkspaceID != chi.URLParam(req, "wsID") {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, signed in, may read or change its business details")
			return
		}
		next(w, req, auth.MethodJWT+":user:"+actx.UserID)
	}
}

func mountBusinessDetailsRoutes(r chi.Router, store businessDetailsStore) {
	write := func(w http.ResponseWriter, out economy.BusinessDetails, err error) {
		switch {
		case errors.Is(err, economy.ErrNotBusiness):
			writeJSONOK(w, http.StatusNotFound, map[string]any{"error": err.Error(), "code": "not_business"})
		case errors.Is(err, economy.ErrWorkspaceNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrBusinessDetailsInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the business details could not be read or saved")
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	}
	r.Get("/v1/workspaces/{wsID}/business", businessOwnerOnly(func(w http.ResponseWriter, req *http.Request, _ string) {
		out, err := store.BusinessDetails(req.Context(), chi.URLParam(req, "wsID"))
		write(w, out, err)
	}))
	r.Put("/v1/workspaces/{wsID}/business", businessOwnerOnly(func(w http.ResponseWriter, req *http.Request, by string) {
		var in economy.BusinessDetails
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"company_name", "registration_number", "country", "people": [{"name", "role", "share_bps"}]}`)
			return
		}
		out, err := store.SetBusinessDetails(req.Context(), chi.URLParam(req, "wsID"), by, in)
		write(w, out, err)
	}))
}
