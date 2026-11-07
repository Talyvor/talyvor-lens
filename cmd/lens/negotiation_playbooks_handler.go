package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B52.16 — Counter playbooks on the marketplace (market/playbooks.go). A creator publishes one as a listing of kind
// counter_playbook through the marketplace's own publish route; it is kept only when it passes Counter's gym.
//
//	GET  /v1/negotiations/playbooks?deal_type=  — the listed playbooks, best gym score first
//	POST /v1/negotiations/{id}/playbook         {listing_id, max_price_usd_micros?} — runs a playbook on the
//	                                            negotiation: one billed use, the first time only, never above the
//	                                            most given; Counter plans and words the negotiation by it
//
// Mounted in the authed group; running one is the workspace owner's or an admin's.
func mountNegotiationPlaybookRoutes(r chi.Router, store *market.Store, meter market.Meter, agents marketAgents) {
	r.Get("/v1/negotiations/playbooks", func(w http.ResponseWriter, req *http.Request) {
		out, err := store.CounterPlaybooks(req.Context(), req.URL.Query().Get("deal_type"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"playbooks": out})
	})
	r.Post("/v1/negotiations/{id}/playbook", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "negotiations are a workspace's: use a workspace's key")
			return
		}
		if _, isOwner := storedanswers.OwnerOrAdmin(req.Context()); !isOwner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may choose a negotiation's playbook")
			return
		}
		var in struct {
			ListingID string `json:"listing_id"`
			MaxPrice  *int64 `json:"max_price_usd_micros"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil || in.ListingID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"listing_id", "max_price_usd_micros"?}`)
			return
		}
		m, byKind := meter, stripeByKind{}
		if k, ok := meter.(stripeByKind); ok {
			m, byKind = k.meterFor(ws), k
		}
		run, err := store.RunCounterPlaybook(req.Context(), market.UseDeps{Meter: m, Agents: agents}, ws, chi.URLParam(req, "id"), in.ListingID, in.MaxPrice)
		switch {
		case errors.Is(err, market.ErrPlaybookBound):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeMarketUseErr(w, err, byKind, ws)
		default:
			writeJSONOK(w, http.StatusOK, run)
		}
	})
}
