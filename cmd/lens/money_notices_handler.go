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

// B53.7 — notices that arrive at the right time (economy/spend_notices.go).
//
//	GET /v1/money/notices          — the notice settings and the latest notices, newest first, each sent, waiting
//	                                 (with why: quiet_hours or daily_tip) or dropped
//	PUT /v1/money/notices/settings {timezone, quiet_from, quiet_until, off} — quiet hours as HH:MM in timezone (both
//	                                 empty: none), and the kinds switched off: payment, budget, unusual, tip, digest
//
// The workspace is the key's; only its owner or an admin may use these (spendingOwner). Mounted in the authed group.

type moneyNoticesBank interface {
	SpendNoticeSettings(ctx context.Context, workspaceID string) (economy.SpendNoticeSettings, error)
	SetSpendNoticeSettings(ctx context.Context, workspaceID string, in economy.SpendNoticeSettings) (economy.SpendNoticeSettings, error)
	SpendNotices(ctx context.Context, workspaceID string) ([]economy.SpendNotice, error)
}

func mountMoneyNoticeRoutes(r chi.Router, bank moneyNoticesBank) {
	r.Get("/v1/money/notices", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		set, err := bank.SpendNoticeSettings(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		notices, err := bank.SpendNotices(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"settings": set, "notices": notices})
	})
	r.Put("/v1/money/notices/settings", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in economy.SpendNoticeSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"timezone", "quiet_from", "quiet_until", "off"}`)
			return
		}
		out, err := bank.SetSpendNoticeSettings(req.Context(), ws, in)
		switch {
		case errors.Is(err, economy.ErrSpending):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	})
}

// sweepSpendNotices makes and sends, every minute until ctx ends, the notices of the workspaces' spending.
func sweepSpendNotices(ctx context.Context, store *economy.DualTokenStore, ai economy.CoachInferer, model string) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n, err := store.SweepSpendNotices(ctx, ai, model, now); err != nil {
				slog.Warn("spend notices: sweep failed", slog.String("err", err.Error()))
			} else if n > 0 {
				slog.Info("spend notices: sent", slog.Int("notices", n))
			}
		}
	}
}
