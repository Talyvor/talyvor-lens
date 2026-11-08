package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.59 — a company account's idle money, swept each day into a money-market fund (economy/treasury_sweeps.go).
//
//	PUT  /v1/money/accounts/{id}/treasury-sweep {target_minor, funding} — what the account keeps; above it, money moves
//	                          into the fund each day. The first set opens the fund at the broker partner
//	GET  /v1/money/accounts/{id}/treasury-sweep — the target, what the fund holds, and at the Test broker its simulated return
//	POST /v1/money/accounts/{id}/treasury-sweep/withdraw {amount_minor, idempotency_key} — back into the account at once
//
// The company's money: only the workspace's owner or an admin. Mounted in the authed group.
type treasuryBank interface {
	SetTreasurySweep(ctx context.Context, workspaceID, accountID string, targetMinor int64, funding string) (economy.TreasurySweep, error)
	TreasurySweep(ctx context.Context, workspaceID, accountID string) (economy.TreasurySweep, error)
	WithdrawTreasury(ctx context.Context, workspaceID, accountID string, amountMinor int64, key string) (economy.MoneyEntry, error)
}

func mountTreasurySweepRoutes(r chi.Router, bank treasuryBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); ws == "" || !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may use the company's money-market fund")
			return "", false
		}
		return ws, true
	}
	writeErr := func(w http.ResponseWriter, err error) {
		switch {
		case errors.Is(err, economy.ErrTreasurySweep):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrTreasurySweepNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrTreasuryFunds):
			writeJSONErr(w, http.StatusConflict, err.Error())
		default:
			writePaymentErr(w, err)
		}
	}
	r.Put("/v1/money/accounts/{id}/treasury-sweep", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			TargetMinor int64  `json:"target_minor"`
			Funding     string `json:"funding"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"target_minor": <0 or more>, "funding": "test|live"}`)
			return
		}
		sw, err := bank.SetTreasurySweep(req.Context(), ws, chi.URLParam(req, "id"), in.TargetMinor, in.Funding)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, sw)
	})
	r.Get("/v1/money/accounts/{id}/treasury-sweep", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		sw, err := bank.TreasurySweep(req.Context(), ws, chi.URLParam(req, "id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, sw)
	})
	r.Post("/v1/money/accounts/{id}/treasury-sweep/withdraw", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			AmountMinor    int64  `json:"amount_minor"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_minor": <positive>, "idempotency_key": "<key>"}`)
			return
		}
		e, err := bank.WithdrawTreasury(req.Context(), ws, chi.URLParam(req, "id"), in.AmountMinor, in.IdempotencyKey)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, e)
	})
}

// sweepTreasuryDaily runs the day's sweeps every hour; each runs once a day, so the hour only catches a missed one.
func sweepTreasuryDaily(ctx context.Context, store *economy.DualTokenStore) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := store.SweepTreasuries(ctx, time.Now()); err != nil {
			slog.Warn("treasury sweep: not every sweep ran", "moved", n, "err", err)
		} else if n > 0 {
			slog.Info("treasury sweep: idle money moved into the money-market fund", "accounts", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
