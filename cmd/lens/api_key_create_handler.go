package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/tenant"
)

// apiKeyCreateHandler serves POST /v1/workspaces/{wsID}/api-keys; main.go mounts it behind keyOwnerOnly (B50.33), so
// only the owner's signed-in session or the operator reaches it and no key ever mints another.
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
