package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/modelwatch"
)

// newCatalogDiscoveredHandler serves GET /v1/catalog/discovered (B10.5): every model a provider lists
// that the catalog cannot price yet — "needs a price", never offered and never billed as free. An
// empty list, not an error, where discovery is off.
func newCatalogDiscoveredHandler(w *modelwatch.Watcher) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if w == nil {
			writeJSONOK(rw, http.StatusOK, []modelwatch.Discovered{})
			return
		}
		list, err := w.Discovered(req.Context())
		if err != nil {
			writeJSONErr(rw, http.StatusInternalServerError, "could not read discovered models")
			return
		}
		writeJSONOK(rw, http.StatusOK, list)
	}
}

// newCatalogPriceHandler serves PUT /v1/admin/catalog/models/{id}/price (B10.5, ADMIN-ONLY — wrapped
// in requireAdmin at wiring): a person confirms a discovered model's price, read from the provider's
// own pricing page, and the model enters the catalog — and the chat picker — at once.
// Body: {"input_per_1m": 2, "output_per_1m": 10, "source": "https://…"} in USD per 1M tokens.
func newCatalogPriceHandler(w *modelwatch.Watcher) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if w == nil {
			writeJSONErr(rw, http.StatusServiceUnavailable, "model discovery is off on this deployment (LENS_MODEL_WATCH_ENABLED)")
			return
		}
		var in struct {
			InputPer1M  *float64 `json:"input_per_1m"`
			OutputPer1M *float64 `json:"output_per_1m"`
			Source      string   `json:"source"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(rw, req.Body, 4096)).Decode(&in); err != nil || in.InputPer1M == nil || in.OutputPer1M == nil {
			writeJSONErr(rw, http.StatusBadRequest, `body must be {"input_per_1m": …, "output_per_1m": …, "source": "https://…"}`)
			return
		}
		m, err := w.ConfirmPrice(req.Context(), chi.URLParam(req, "id"), *in.InputPer1M, *in.OutputPer1M, in.Source)
		switch {
		case errors.Is(err, modelwatch.ErrNotListed):
			writeJSONErr(rw, http.StatusNotFound, err.Error())
		case errors.Is(err, modelwatch.ErrAlreadyPriced):
			writeJSONErr(rw, http.StatusConflict, err.Error()+". A seeded price changes in internal/catalog/seed.go")
		case err != nil:
			writeJSONErr(rw, http.StatusBadRequest, err.Error())
		default:
			writeJSONOK(rw, http.StatusOK, m)
		}
	}
}
