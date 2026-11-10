package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/sso"
)

// B56.18 — single sign-on: a workspace's own identity provider (internal/sso). The owner, signed in, saves, reads
// back, checks a domain of and removes the connection; the secret never comes back to them. The suite BFF asks, on
// LENS_PROVISION_SECRET, for the connection that covers an email, and it alone receives the secret. Registered only
// while custody is armed (LENS_PROVIDER_SECRET_KEK), like provider keys.

// ssoOwnerOnly admits only the workspace owner's signed-in session or the operator, by keyOwnerOnly's rule: who may
// sign in to a workspace is decided no more loosely than who holds its keys.
func ssoOwnerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actx := auth.GetAuthContext(req.Context())
		if actx == nil || actx.AuthMethod == auth.MethodWorkspaceKey || actx.AuthMethod == auth.MethodSessionKey ||
			!actx.HasScope(auth.ScopeKeys) {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, signed in, may manage its single sign-on")
			return
		}
		next(w, req)
	}
}

func writeSSOErr(w http.ResponseWriter, err error) {
	switch {
	case writePlanRefusal(w, err):
	case errors.Is(err, sso.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sso.ErrNoConnection), errors.Is(err, sso.ErrNoDomain):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, sso.ErrDomainTaken):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, "single sign-on could not be read or stored")
	}
}

// GET /v1/workspaces/{wsID}/sso → the connection without its secret, each domain with the TXT record that proves it.
func newSSOGetHandler(s *sso.Store) http.HandlerFunc {
	return ssoOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		c, err := s.Get(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeSSOErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
}

// PUT /v1/workspaces/{wsID}/sso {"issuer", "client_id", "client_secret", "domains": [...]} → the connection.
func newSSOPutHandler(s *sso.Store) http.HandlerFunc {
	return ssoOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Issuer       string   `json:"issuer"`
			ClientID     string   `json:"client_id"`
			ClientSecret string   `json:"client_secret"`
			Domains      []string `json:"domains"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 16<<10)).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"issuer", "client_id", "client_secret", "domains": [...]}`)
			return
		}
		c, err := s.Save(req.Context(), chi.URLParam(req, "wsID"), body.Issuer, body.ClientID, body.ClientSecret, body.Domains)
		if err != nil {
			writeSSOErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
}

// POST /v1/workspaces/{wsID}/sso/domains/{domain}/check → the domain, proven; 409 with the record to publish when its
// TXT record does not answer yet.
func newSSOCheckHandler(s *sso.Store) http.HandlerFunc {
	return ssoOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		d, err := s.Check(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "domain"))
		if errors.Is(err, sso.ErrUnproven) {
			writeJSONOK(w, http.StatusConflict, map[string]any{"error": err.Error(), "domain": d})
			return
		}
		if err != nil {
			writeSSOErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, d)
	})
}

// DELETE /v1/workspaces/{wsID}/sso → 204, or 404 when there was no connection.
func newSSODeleteHandler(s *sso.Store) http.HandlerFunc {
	return ssoOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		gone, err := s.Remove(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeSSOErr(w, err)
			return
		}
		if !gone {
			writeSSOErr(w, sso.ErrNoConnection)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// mountSSOSignInRoute registers POST /v1/sso/sign-in on the bare router, beside /v1/provision and for the same reason:
// its caller carries LENS_PROVISION_SECRET, not an API key. Unset secret or no store ⇒ not registered.
// POST {"email"} → {"workspace_id", "issuer", "client_id", "client_secret"}, or 404 when no proven domain covers it.
func mountSSOSignInRoute(r chi.Router, secret string, s *sso.Store) {
	if secret == "" || s == nil {
		return
	}
	r.Post("/v1/sso/sign-in", func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(provisionSecretHeader)), []byte(secret)) != 1 {
			writeJSONErr(w, http.StatusUnauthorized, "provisioning credentials required")
			return
		}
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"email": "..."}`)
			return
		}
		c, err := s.For(req.Context(), body.Email)
		if err != nil {
			writeSSOErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSONOK(w, http.StatusOK, c)
	})
}
