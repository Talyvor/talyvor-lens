package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/members"
)

// B52.35 — a person joins another workspace by invite, lists their workspaces, and gets a token for one.
//
// The owner manages the workspace's invite links and members on its own session token (ownerOnly); the suite BFF
// accepts an invite and lists a person's workspaces on LENS_PROVISION_SECRET, for the identity of the session it
// holds; and POST /v1/provision with a workspace_id mints a member's read-only token (provision_handler.go).

// writeMembersErr answers a members.Store error.
func writeMembersErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, members.ErrNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, members.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, members.ErrConflict):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

// mountWorkspaceMemberRoutes registers the owner's routes inside the authed group, behind workspaceIsolationMiddleware.
func mountWorkspaceMemberRoutes(r chi.Router, store *members.Store) {
	r.Post("/v1/workspaces/{wsID}/member-invites", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			MaxUses   int        `json:"max_uses"`
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {max_uses, expires_at}: "+err.Error())
			return
		}
		inv, err := store.CreateInvite(req.Context(), chi.URLParam(req, "wsID"), in.MaxUses, in.ExpiresAt)
		if err != nil {
			writeMembersErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, inv)
	}))
	r.Get("/v1/workspaces/{wsID}/member-invites", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		invites, err := store.Invites(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeMembersErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"invites": invites})
	}))
	r.Delete("/v1/workspaces/{wsID}/member-invites/{inviteID}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		inv, err := store.RevokeInvite(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "inviteID"))
		if err != nil {
			writeMembersErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, inv)
	}))
	r.Get("/v1/workspaces/{wsID}/members", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		list, err := store.Members(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeMembersErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"members": list})
	}))
	r.Delete("/v1/workspaces/{wsID}/members/{memberID}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		if err := store.RemoveMember(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "memberID")); err != nil {
			writeMembersErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// mountMemberProvisionRoutes registers the BFF's routes on the bare router, beside /v1/provision and on its secret.
// Unset secret ⇒ not registered.
func mountMemberProvisionRoutes(r chi.Router, secret string, store *members.Store) {
	if secret == "" {
		return
	}
	// decode checks the provisioning secret and reads a body naming the identity; ok is false once it has answered.
	decode := func(w http.ResponseWriter, req *http.Request, into any, identity *string) bool {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(provisionSecretHeader)), []byte(secret)) != 1 {
			writeJSONErr(w, http.StatusUnauthorized, "provisioning credentials required")
			return false
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(into); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return false
		}
		if *identity == "" {
			writeJSONErr(w, http.StatusBadRequest, "identity required")
			return false
		}
		return true
	}
	r.Post("/v1/provision/invites/accept", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Identity string `json:"identity"`
			Token    string `json:"token"`
		}
		if !decode(w, req, &in, &in.Identity) {
			return
		}
		m, created, err := store.Accept(req.Context(), in.Identity, deriveWorkspaceID(in.Identity), in.Token)
		if err != nil {
			writeMembersErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"member": m, "created": created})
	})
	r.Post("/v1/provision/workspaces", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Identity string `json:"identity"`
		}
		if !decode(w, req, &in, &in.Identity) {
			return
		}
		list, err := store.Workspaces(req.Context(), in.Identity, deriveWorkspaceID(in.Identity))
		if err != nil {
			writeMembersErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"workspaces": list})
	})
}
