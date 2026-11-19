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

// B28.334 — alert when an agent's balance falls below a threshold (economy/low_balance_alerts.go).
//
//	GET    /v1/money/low-balance        — the workspace's thresholds, each with what its agent holds now and whether
//	                                      that is below
//	PUT    /v1/money/low-balance        {agent_id, currency, threshold_minor} — set the threshold for that agent's
//	                                      balance in that currency
//	DELETE /v1/money/low-balance/{id}   — remove a threshold; its alerts are kept
//	GET    /v1/money/low-balance/alerts — every hour an agent's balance was found below its threshold, newest first;
//	                                      one an hour, however often it is checked
//
// The workspace is the key's; only its owner or an admin may use these (spendingOwner). Mounted in the authed group.

type moneyLowBalanceBank interface {
	BalanceThresholds(ctx context.Context, workspaceID string) ([]economy.BalanceThreshold, error)
	SetBalanceThreshold(ctx context.Context, workspaceID string, t economy.BalanceThreshold, by string) (economy.BalanceThreshold, error)
	DeleteBalanceThreshold(ctx context.Context, workspaceID, id string) error
	CheckLowBalanceAlerts(ctx context.Context, workspaceID string, now time.Time) ([]economy.LowBalanceAlert, error)
	LowBalanceAlerts(ctx context.Context, workspaceID string) ([]economy.LowBalanceAlert, error)
}

func mountMoneyLowBalanceRoutes(r chi.Router, bank moneyLowBalanceBank) {
	answer := func(w http.ResponseWriter, err error, ok func()) {
		switch {
		case errors.Is(err, economy.ErrSpending):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrBalanceThresholdNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			ok()
		}
	}
	r.Get("/v1/money/low-balance", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		out, err := bank.BalanceThresholds(req.Context(), ws)
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"thresholds": out}) })
	})
	r.Put("/v1/money/low-balance", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in economy.BalanceThreshold
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"agent_id", "currency", "threshold_minor"}`)
			return
		}
		out, err := bank.SetBalanceThreshold(req.Context(), ws, in, who)
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, out) })
	})
	r.Delete("/v1/money/low-balance/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		err := bank.DeleteBalanceThreshold(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]string{"deleted": chi.URLParam(req, "id")}) })
	})
	r.Get("/v1/money/low-balance/alerts", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		if _, err := bank.CheckLowBalanceAlerts(req.Context(), ws, time.Now()); err != nil { // the sweep's minute need not be waited for
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err := bank.LowBalanceAlerts(req.Context(), ws)
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"alerts": out}) })
	})
}

// sweepLowBalanceAlerts raises, every minute until ctx ends, an alert for each agent below its balance threshold.
func sweepLowBalanceAlerts(ctx context.Context, store *economy.DualTokenStore) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n, err := store.SweepLowBalanceAlerts(ctx, now); err != nil {
				slog.Warn("low balance alerts: sweep failed", slog.String("err", err.Error()))
			} else if n > 0 {
				slog.Info("low balance alerts: raised", slog.Int("alerts", n))
			}
		}
	}
}
