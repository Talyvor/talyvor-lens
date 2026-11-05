package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/tenant"
)

// apiKeyCreateHandler serves POST /v1/workspaces/{wsID}/api-keys; main.go mounts it behind ownerOnly (B50.33).
// The owner's session (keys) and the operator may grant any scope; any other caller only scopes it holds itself,
// so a key can never mint one stronger than it is.
func apiKeyCreateHandler(tenantStore *tenant.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		var in struct {
			Name      string     `json:"name"`
			Scopes    []string   `json:"scopes"`
			ExpiresAt *time.Time `json:"expires_at,omitempty"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if actx := auth.GetAuthContext(req.Context()); !actx.HasScope(auth.ScopeKeys) {
			for _, s := range in.Scopes {
				if !actx.HasScope(s) {
					writeJSONErr(w, http.StatusForbidden, "this credential may not grant the scope it does not hold: "+s)
					return
				}
			}
		}
		raw, key, err := tenantStore.CreateAPIKey(req.Context(), wsID, in.Name, in.Scopes, in.ExpiresAt)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{
			"key":     raw,
			"id":      key.ID,
			"prefix":  key.KeyPrefix,
			"name":    key.Name,
			"scopes":  key.Scopes,
			"warning": "Store this key securely. It will not be shown again.",
		})
	}
}
