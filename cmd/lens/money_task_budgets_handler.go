package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B28.310 — a budget for one task that ends at a time (economy/money_task_budgets.go).
//
//	POST /v1/money/task-budgets {agent_id, task, currency, amount_minor, expires_at, funding, idempotency_key} — pay the
//	                            agent the budget from the company's account now; what is left goes back at expires_at
//	GET  /v1/money/task-budgets ?agent_id= — the budgets, the latest first, each with what was spent and returned
//	GET  /v1/money/task-budgets/{id} — one of them
//	POST /v1/money/task-budgets/{id}/end — end it now and return what is left; tried again on one whose return was refused
//
// The workspace is the key's, and only its owner or an admin may use them. Mounted in the authed group; the scheduler
// in main.go returns the ones whose time has come.

type moneyTaskBudgetBank interface {
	CreateMoneyTaskBudget(ctx context.Context, b economy.MoneyTaskBudget, now time.Time) (economy.MoneyTaskBudget, error)
	MoneyTaskBudgets(ctx context.Context, workspaceID, agentID string) ([]economy.MoneyTaskBudget, error)
	MoneyTaskBudget(ctx context.Context, workspaceID, id string) (economy.MoneyTaskBudget, error)
	EndMoneyTaskBudget(ctx context.Context, workspaceID, id string, now time.Time) (economy.MoneyTaskBudget, error)
}

func mountMoneyTaskBudgetRoutes(r chi.Router, bank moneyTaskBudgetBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); ws == "" || !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may give agents task budgets")
			return "", false
		}
		return ws, true
	}
	answer := func(w http.ResponseWriter, b economy.MoneyTaskBudget, err error) {
		switch {
		case errors.Is(err, economy.ErrMoneyTaskBudgetNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrMoneyTaskBudget):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrMoneyTaskBudgetFunds), errors.Is(err, economy.ErrMoneyAccountNotFound):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrCapabilityNotCleared):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, b)
		}
	}
	r.Post("/v1/money/task-budgets", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in economy.MoneyTaskBudget
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"agent_id": "<id>", "task": "<what it is for>", "currency": "GBP|EUR|USD", "amount_minor": <positive>, "expires_at": "<RFC 3339 time>", "funding": "test|live", "idempotency_key": "<optional>"}`)
			return
		}
		b, err := bank.CreateMoneyTaskBudget(req.Context(), economy.MoneyTaskBudget{WorkspaceID: ws, AgentID: in.AgentID, Task: in.Task,
			Currency: in.Currency, AmountMinor: in.AmountMinor, ExpiresAt: in.ExpiresAt, Funding: in.Funding, IdempotencyKey: in.IdempotencyKey}, time.Now())
		answer(w, b, err)
	})
	r.Get("/v1/money/task-budgets", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		list, err := bank.MoneyTaskBudgets(req.Context(), ws, req.URL.Query().Get("agent_id"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"task_budgets": list})
	})
	r.Get("/v1/money/task-budgets/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		b, err := bank.MoneyTaskBudget(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, b, err)
	})
	r.Post("/v1/money/task-budgets/{id}/end", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		b, err := bank.EndMoneyTaskBudget(req.Context(), ws, chi.URLParam(req, "id"), time.Now())
		answer(w, b, err)
	})
}
