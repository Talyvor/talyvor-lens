package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.27 — a monthly allowance in money for each agent (economy/money_allowances.go).
//
//	PUT    /v1/money/allowances {agent_id, currency, amount_minor, day_of_month, sweep_back, funding} — set or change
//	                            the agent's allowance in that currency, paid from the company's account on that day
//	GET    /v1/money/allowances ?agent_id= — the allowances and when each is next paid
//	GET    /v1/money/allowances/{id}/runs — each month: what was swept back, what was paid in, and the entries
//	DELETE /v1/money/allowances/{id} — stop it; its months stay
//
// The workspace is the key's, and only its owner or an admin may use them. Mounted in the authed group.

type moneyAllowanceBank interface {
	SetMoneyAllowance(ctx context.Context, a economy.MoneyAllowance) (economy.MoneyAllowance, error)
	MoneyAllowances(ctx context.Context, workspaceID, agentID string) ([]economy.MoneyAllowance, error)
	MoneyAllowanceRuns(ctx context.Context, workspaceID, id string) ([]economy.MoneyAllowanceRun, error)
	StopMoneyAllowance(ctx context.Context, workspaceID, id string) error
}

func mountMoneyAllowanceRoutes(r chi.Router, bank moneyAllowanceBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); ws == "" || !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may set agents' allowances")
			return "", false
		}
		return ws, true
	}
	notFound := func(w http.ResponseWriter, err error) {
		if errors.Is(err, economy.ErrMoneyAllowanceNotFound) {
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
	r.Put("/v1/money/allowances", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in economy.MoneyAllowance
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"agent_id": "<id>", "currency": "GBP|EUR|USD", "amount_minor": <positive>, "day_of_month": 1-28, "sweep_back": true|false, "funding": "test|live"}`)
			return
		}
		a, err := bank.SetMoneyAllowance(req.Context(), economy.MoneyAllowance{WorkspaceID: ws, AgentID: in.AgentID, Currency: in.Currency,
			AmountMinor: in.AmountMinor, DayOfMonth: in.DayOfMonth, SweepBack: in.SweepBack, Funding: in.Funding})
		switch {
		case errors.Is(err, economy.ErrMoneyAllowance):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrMoneyAccountNotFound):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrCapabilityNotCleared):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, a)
		}
	})
	r.Get("/v1/money/allowances", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		list, err := bank.MoneyAllowances(req.Context(), ws, req.URL.Query().Get("agent_id"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"allowances": list})
	})
	r.Get("/v1/money/allowances/{id}/runs", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		runs, err := bank.MoneyAllowanceRuns(req.Context(), ws, chi.URLParam(req, "id"))
		if err != nil {
			notFound(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"runs": runs})
	})
	r.Delete("/v1/money/allowances/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		if err := bank.StopMoneyAllowance(req.Context(), ws, chi.URLParam(req, "id")); err != nil {
			notFound(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
