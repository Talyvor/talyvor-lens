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

// B55.1 — a new key joins from another device (economy/key_enrolments.go). A passkey (POST …/agents/passkeys) or phone
// key (POST /v1/money/devices) registered with no assertion in a workspace that has a key answers 202
// {"enrolment": {"id", "code", "expires_at"}} and waits; 409 once three wait, or one with the same code does; 409 for a
// passkey already registered, or when every key of the workspace is revoked (B54.28).
//
//	GET  /v1/workspaces/{wsID}/agents/keys/enrolments                — the keys waiting
//	GET  /v1/workspaces/{wsID}/agents/keys/enrolments/{id}           — one, whatever its status
//	POST /v1/workspaces/{wsID}/agents/keys/enrolments/{id}/challenge — {"challenge", "allow_credentials"}
//	POST /v1/workspaces/{wsID}/agents/keys/enrolments/{id}/approve   {"assertion"} — by an existing passkey or phone key
//	                                                                  over that challenge; the key is then the workspace's
//	POST /v1/workspaces/{wsID}/agents/keys/enrolments/{id}/decline   — it can never be let in after
//
// The owner's or an admin's; approving adds a key, so it is the owner's signed-in session's alone (B50.33). Mounted in
// the authed group.

type keyEnrolmentBank interface {
	ListKeyEnrolments(ctx context.Context, workspaceID string) ([]economy.KeyEnrolment, error)
	GetKeyEnrolment(ctx context.Context, workspaceID, id string) (economy.KeyEnrolment, error)
	KeyEnrolmentChallenge(ctx context.Context, workspaceID, id string) (challenge string, credentialIDs []string, err error)
	ApproveKeyEnrolment(ctx context.Context, workspaceID, id string, a *passkey.Assertion) (economy.KeyEnrolment, error)
	DeclineKeyEnrolment(ctx context.Context, workspaceID, id string) (economy.KeyEnrolment, error)
}

// writeEnrolmentWaiting answers a registration that was kept waiting (202) or refused beside the ones waiting (409); false
// when err is neither.
func writeEnrolmentWaiting(w http.ResponseWriter, err error) bool {
	var waiting *economy.EnrolmentWaiting
	switch {
	case errors.As(err, &waiting):
		e := waiting.Enrolment
		writeJSONOK(w, http.StatusAccepted, map[string]any{"enrolment": map[string]any{"id": e.ID, "code": e.Code, "expires_at": e.ExpiresAt}})
	case errors.Is(err, economy.ErrTooManyEnrolments), errors.Is(err, economy.ErrEnrolmentCodeTaken),
		errors.Is(err, economy.ErrPasskeyRegistered), errors.Is(err, economy.ErrNoLiveKey): // B54.28
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		return false
	}
	return true
}

func mountKeyEnrolmentRoutes(r chi.Router, bank keyEnrolmentBank) {
	answer := func(w http.ResponseWriter, v any, err error) {
		switch {
		case errors.Is(err, economy.ErrEnrolmentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrPasskeyRequired):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPasskeyRegistered):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, v)
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/keys/enrolments", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListKeyEnrolments(req.Context(), chi.URLParam(req, "wsID"))
		answer(w, map[string]any{"enrolments": list}, err)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/keys/enrolments/{id}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		e, err := bank.GetKeyEnrolment(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "id"))
		answer(w, e, err)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/keys/enrolments/{id}/challenge", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		c, creds, err := bank.KeyEnrolmentChallenge(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "id"))
		answer(w, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"}, err)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/keys/enrolments/{id}/approve", keyOwnerOnly(ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Assertion *passkey.Assertion `json:"assertion"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		e, err := bank.ApproveKeyEnrolment(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "id"), in.Assertion)
		answer(w, e, err)
	})))
	r.Post("/v1/workspaces/{wsID}/agents/keys/enrolments/{id}/decline", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		e, err := bank.DeclineKeyEnrolment(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "id"))
		answer(w, e, err)
	}))
}
