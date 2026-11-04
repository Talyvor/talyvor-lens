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

// B53.13 — accounts at other banks, read through open banking (economy/bank_connections.go).
//
//	GET    /v1/money/banks              — the connected accounts: bank, account, balance, when each was last read, when
//	                                      its consent runs out and whether its renewal is due
//	POST   /v1/money/banks              {bank, account} — consent to Lens reading one account there, then read it
//	POST   /v1/money/banks/{id}/refresh — read it again now
//	POST   /v1/money/banks/{id}/renew   — extend its consent
//	DELETE /v1/money/banks/{id}         — end its consent and delete everything read from it
//
// What is read shows in GET /v1/money/spending as kind bank. Only the workspace's owner or an admin may use these.
// Mounted in the authed group.

type moneyBanks interface {
	BankConnections(ctx context.Context, workspaceID string, now time.Time) ([]economy.BankConnection, error)
	ConnectBank(ctx context.Context, workspaceID, bank, account, by string) (economy.BankConnection, error)
	RefreshBank(ctx context.Context, workspaceID, id string) (economy.BankConnection, error)
	RenewBankConsent(ctx context.Context, workspaceID, id string) (economy.BankConnection, error)
	DisconnectBank(ctx context.Context, workspaceID, id string) error
}

func mountMoneyBankRoutes(r chi.Router, banks moneyBanks) {
	answer := func(w http.ResponseWriter, err error, ok func()) {
		switch {
		case errors.Is(err, economy.ErrBankInfoUnavailable):
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, economy.ErrBankConnectionNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrBankConnection):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			ok()
		}
	}
	r.Get("/v1/money/banks", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		out, err := banks.BankConnections(req.Context(), ws, time.Now())
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"connections": out}) })
	})
	r.Post("/v1/money/banks", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in struct{ Bank, Account string }
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"bank", "account"}`)
			return
		}
		c, err := banks.ConnectBank(req.Context(), ws, in.Bank, in.Account, who)
		answer(w, err, func() { writeJSONOK(w, http.StatusCreated, c) })
	})
	r.Post("/v1/money/banks/{id}/refresh", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		c, err := banks.RefreshBank(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, c) })
	})
	r.Post("/v1/money/banks/{id}/renew", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		c, err := banks.RenewBankConsent(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, c) })
	})
	r.Delete("/v1/money/banks/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		err := banks.DisconnectBank(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, err, func() { w.WriteHeader(http.StatusNoContent) })
	})
}

// sweepBankConnections reads, every quarter of an hour until ctx ends, each connected account due a background read.
func sweepBankConnections(ctx context.Context, store *economy.DualTokenStore) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n, err := store.SweepBankConnections(ctx, now); err != nil {
				slog.Warn("bank connections: sweep failed", slog.String("err", err.Error()))
			} else if n > 0 {
				slog.Info("bank connections: read", slog.Int("connections", n))
			}
		}
	}
}
