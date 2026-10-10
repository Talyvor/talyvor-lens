package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// payments_in_handler.go — B30.15: RECEIVE MONEY FROM OUTSIDE (economy/payments_in.go).
//
//	POST /v1/money/payments-in/webhook   the account partner's notice of a payment that arrived at a company account's
//	                                     details, signed X-Partner-Signature: hex HMAC-SHA256 of the raw body under
//	                                     LENS_ACCOUNT_PARTNER_WEBHOOK_SECRET. Public, on the bare router; unset ⇒
//	                                     unregistered. The money posts to the account the details or the reference
//	                                     name, or to suspense
//	GET  /v1/admin/money/suspense        money in that matched no account, and when each goes back to its payer
//	POST /v1/admin/money/suspense/{entryID}/assign {account_id} — the operator puts it in the account it was for

// paymentsInStore is what the webhook and the operator's suspense routes use: *economy.DualTokenStore.
type paymentsInStore interface {
	ReceivePayment(ctx context.Context, in economy.InboundPayment) (economy.MoneyEntry, error)
	SuspenseItems(ctx context.Context, days int) ([]economy.SuspenseItem, error)
	AssignSuspense(ctx context.Context, entryID, accountID string) (economy.MoneyEntry, error)
}

// newPaymentsInWebhook posts each signed payment in. Money the Test partner reports is test money; any other
// partner's is live, and is refused by the ledger until payments_in is cleared.
func newPaymentsInWebhook(secret string, store paymentsInStore, registry *partners.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 64<<10))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "unreadable body")
			return
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(raw)
		got, _ := hex.DecodeString(strings.TrimSpace(req.Header.Get("X-Partner-Signature")))
		if !hmac.Equal(got, mac.Sum(nil)) {
			writeJSONErr(w, http.StatusUnauthorized, "bad or missing X-Partner-Signature")
			return
		}
		var in economy.InboundPayment
		if err := json.Unmarshal(raw, &in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		p, err := registry.Account(req.Context(), economy.CapabilityPaymentsIn)
		if err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		in.Funding = economy.FundingLive
		if p.Name() == "test" {
			in.Funding = economy.FundingTest
		}
		e, err := store.ReceivePayment(req.Context(), in)
		var refusal *screening.Refusal
		switch {
		case errors.Is(err, economy.ErrMoneyAccountNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.As(err, &refusal), errors.Is(err, economy.ErrCapabilityNotCleared):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error()) // the partner retries: a payment posts once
		default:
			writeJSONOK(w, http.StatusOK, e)
		}
	})
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
		e, err := store.AssignSuspense(req.Context(), chi.URLParam(req, "entryID"), in.AccountID)
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
