package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/scan"
)

// B52.27 — a Scan agent's watches: the owner picks the series and what counts as unusual, and the agent's wallet pays
// for each alert its watch sends.
func mountScanWatchRoutes(r chi.Router, watches *scan.Watcher, bank agentBank, keys agentKeyIssuer) {
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/scan-watches", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Series []string `json:"series"`
			Rule   string   `json:"rule"`
			Z      *float64 `json:"z"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"series": ["<series>", …], "rule": "range|z", "z": <standard deviations, for rule z>}`)
			return
		}
		ws, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		if err := bank.AgentMayRun(req.Context(), ws, agentID); errors.Is(err, economy.ErrAgentNotFound) {
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		} // a paused agent may still be given a watch; it runs once the agent is resumed
		wt, err := watches.Create(req.Context(), ws, agentID, in.Series, in.Rule, in.Z)
		switch {
		case errors.Is(err, scan.ErrBadWatch):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// The agent pays through its own key; one made on Agent Wallets has none until Lens issues it one.
		if err := ensurePromptKey(req.Context(), bank, keys, ws, agentID); err != nil {
			_ = watches.Stop(req.Context(), ws, wt.ID)
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, wt)
	}))
	r.Get("/v1/workspaces/{wsID}/scan-watches", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		list, err := watches.List(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"watches": list})
	}))
	r.Delete("/v1/workspaces/{wsID}/scan-watches/{watchID}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		err := watches.Stop(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "watchID"))
		switch {
		case errors.Is(err, scan.ErrWatchNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"id": chi.URLParam(req, "watchID"), "active": false})
		}
	}))
}
