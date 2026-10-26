package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/byok"
	"github.com/talyvor/lens/internal/plans"
)

// B27.26 — "Your provider keys", for a workspace whose plan lets it use its own (B32.12). A key goes in and never comes back out:
// every answer carries the provider and the last four characters only. Registered only while custody is
// armed (LENS_PROVIDER_SECRET_KEK); {wsID} is bound to the caller's credential like its billing siblings.

// GET /v1/workspaces/{wsID}/provider-keys → {"byok": on the plan, "providers": [...], "keys": [...]}
func newProviderKeysListHandler(s *byok.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		on, err := s.Subscribed(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		keys, err := s.List(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		providers := byok.Providers
		if !on {
			providers = []string{} // B17.278: off the plan a key can be added for no provider
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"byok": on, "providers": providers, "keys": keys})
	}
}

// PUT /v1/workspaces/{wsID}/provider-keys/{provider} {"key": "..."} → {"provider", "last4", "updated_at"}
func newProviderKeyPutHandler(s *byok.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"key": "<your provider API key>"}`)
			return
		}
		k, err := s.Put(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "provider"), body.Key)
		switch {
		case errors.Is(err, byok.ErrUnsupportedProvider), errors.Is(err, byok.ErrInvalidKey):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, plans.ErrRefused):
			writeJSONErr(w, http.StatusPaymentRequired, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the key could not be stored")
		default:
			writeJSONOK(w, http.StatusOK, k)
		}
	}
}

// DELETE /v1/workspaces/{wsID}/provider-keys/{provider} → 204, or 404 when there was no key.
func newProviderKeyDeleteHandler(s *byok.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		gone, err := s.Delete(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "provider"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !gone {
			writeJSONErr(w, http.StatusNotFound, "no key stored for that provider")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
