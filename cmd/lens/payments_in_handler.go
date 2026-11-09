package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// payments_in_handler.go — B30.15: RECEIVE MONEY FROM OUTSIDE (economy/payments_in.go). A partner's webhook posts each
// payment in (B46.3, money_layer.go); money that names no open account goes to suspense, and here the operator sees it
// and assigns it, or the hourly job pays it back.
//
//	GET  /v1/admin/money/suspense        money in that matched no account, and when each goes back to its payer
//	POST /v1/admin/money/suspense/{entryID}/assign {account_id} — the operator puts it in the account it was for

// paymentsInStore is what the operator's suspense routes use: *economy.DualTokenStore.
type paymentsInStore interface {
	SuspenseItems(ctx context.Context, days int) ([]economy.SuspenseItem, error)
	AssignSuspense(ctx context.Context, entryID, accountID, by string) (economy.MoneyEntry, error)
}

func newSuspenseListHandler(store paymentsInStore, days int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		items, err := store.SuspenseItems(req.Context(), days)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"suspense": items, "return_days": days})
	})
}

func newSuspenseAssignHandler(store paymentsInStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			AccountID string `json:"account_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil || in.AccountID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"account_id": "<the company or agent account it was for>"}`)
			return
		}
		// requireStepUp has checked the operator X-Talyvor-Operator names: the audit row's who.
		by := strings.TrimSpace(req.Header.Get(moderatorOperatorHeader))
		e, err := store.AssignSuspense(req.Context(), chi.URLParam(req, "entryID"), in.AccountID, by)
		switch {
		case errors.Is(err, economy.ErrSuspenseNotHeld):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrMoneyAccountNotFound):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, e)
		}
	})
}

// returnSuspenseHourly is the job that pays money in back to its payer once it has sat in suspense days business days.
func returnSuspenseHourly(ctx context.Context, store *economy.DualTokenStore, days int) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := store.ReturnDueSuspense(ctx, time.Now(), days); err != nil {
			slog.Warn("suspense: the return run did not complete", "err", err)
		} else if n > 0 {
			slog.Info("suspense: money in that matched no account went back to its payers", "returned", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
