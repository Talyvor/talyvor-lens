package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B53.4 — weekly and monthly budgets, with alerts at 50, 80 and 100 percent (economy/spend_budgets.go).
//
//	GET    /v1/money/budgets        — the workspace's budgets, each with this period's start and end, what has been
//	                                  spent in it, what is left and the percent spent
//	PUT    /v1/money/budgets        {agent_id, category, period, amount_minor, currency, timezone} — set the budget for
//	                                  that wallet ("" your own, "all" every wallet, or an agent's), category ("" all
//	                                  spending), period (week from Monday, or month, in timezone) and currency
//	DELETE /v1/money/budgets/{id}   — remove a budget
//	GET    /v1/money/budgets/alerts — every time spending reached 50, 80 or 100% of a budget, a card's cap or an agent's
//	                                  limit, newest first; each once per period
//
// The workspace is the key's; only its owner or an admin may use these (mountMoneySpendingRoutes' rule). Mounted in
// the authed group.

type moneyBudgetsBank interface {
	SpendBudgets(ctx context.Context, workspaceID string, now time.Time) ([]economy.SpendBudget, error)
	SetSpendBudget(ctx context.Context, workspaceID string, b economy.SpendBudget, by string, now time.Time) (economy.SpendBudget, error)
	DeleteSpendBudget(ctx context.Context, workspaceID, id string) error
	CheckSpendAlerts(ctx context.Context, workspaceID string, now time.Time) ([]economy.SpendAlert, error)
	SpendAlerts(ctx context.Context, workspaceID string) ([]economy.SpendAlert, error)
}

func mountMoneyBudgetRoutes(r chi.Router, bank moneyBudgetsBank) {
	answer := func(w http.ResponseWriter, err error, ok func()) {
		switch {
		case errors.Is(err, economy.ErrSpending):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrBudgetNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			ok()
		}
	}
	r.Get("/v1/money/budgets", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		out, err := bank.SpendBudgets(req.Context(), ws, time.Now())
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"budgets": out}) })
	})
	r.Put("/v1/money/budgets", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in economy.SpendBudget
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"agent_id", "category", "period", "amount_minor", "currency", "timezone"}`)
			return
		}
		out, err := bank.SetSpendBudget(req.Context(), ws, in, who, time.Now())
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, out) })
	})
	r.Delete("/v1/money/budgets/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		err := bank.DeleteSpendBudget(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]string{"deleted": chi.URLParam(req, "id")}) })
	})
	r.Get("/v1/money/budgets/alerts", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		if _, err := bank.CheckSpendAlerts(req.Context(), ws, time.Now()); err != nil { // the sweep's minute need not be waited for
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err := bank.SpendAlerts(req.Context(), ws)
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"alerts": out}) })
	})
}

// sweepSpendAlerts raises, every minute until ctx ends, the alerts spending has reached: within a minute of the payment.
func sweepSpendAlerts(ctx context.Context, store *economy.DualTokenStore) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n, err := store.SweepSpendAlerts(ctx, now); err != nil {
				slog.Warn("spend alerts: sweep failed", slog.String("err", err.Error()))
			} else if n > 0 {
				slog.Info("spend alerts: raised", slog.Int("alerts", n))
			}
		}
	}
}
