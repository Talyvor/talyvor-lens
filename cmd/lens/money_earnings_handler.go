package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B46.11 — earnings paid out on the money layer (economy/money_earnings_payouts.go, rooms/payouts.go).
//
//	PUT  /v1/money/payout-details    {asset, holder, iban, bic} or {asset: "USDC", holder, network: "base", address},
//	                                 country: where the workspace is paid in that asset; its holder is verified by the
//	                                 money layer's partner, and payouts held for want of verified details are paid
//	GET  /v1/money/payout-details    the workspace's payout details, each asset's, and whether they are verified
//	GET  /v1/money/earnings-payouts  the payouts the workspace made or was paid, newest first: a sale, a Room's split
//	                                 or a royalty, its agreement, and held, pending, completed, failed or returned
//	POST /v1/rooms/{roomID}/payouts  {amount_minor, currency, shares}, with an Idempotency-Key: the room's owner pays
//	                                 out of the room's wallet to its members as the room's terms split it — equal, by
//	                                 votes, or shares the owner decides ({member workspace: basis points}, summing to
//	                                 10000). Each member is paid once; one without verified details is held, not paid.
//
// Only the workspace's owner or an admin saves payout details or pays out a room. Mounted in the authed group.

type moneyEarningsBank interface {
	SavePayoutDetails(ctx context.Context, workspaceID string, d economy.PayoutDetails) (economy.PayoutDetails, error)
	PayoutDetailsOf(ctx context.Context, workspaceID string) ([]economy.PayoutDetails, error)
	EarningsPayouts(ctx context.Context, workspaceID string) ([]economy.EarningsPayout, error)
	PayEarnings(ctx context.Context, workspaceID string, e economy.Earnings) ([]economy.EarningsPayout, error)
}

type roomSplitter interface {
	Split(ctx context.Context, roomID, callerWS, key string, in rooms.SplitPayout) (economy.Earnings, error)
}

func writeEarningsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrPayoutDetails), errors.Is(err, economy.ErrEarningsPayout), errors.Is(err, rooms.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, rooms.ErrNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, rooms.ErrForbidden):
		writeJSONErr(w, http.StatusForbidden, "only the room's owner pays out its earnings")
	case errors.Is(err, rooms.ErrConflict):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrAgentWalletsOff):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		writePaymentErr(w, err)
	}
}

func mountMoneyEarningsRoutes(r chi.Router, bank moneyEarningsBank, roomsStore roomSplitter) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); ws == "" || !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin does this")
			return "", false
		}
		return ws, true
	}
	r.Put("/v1/money/payout-details", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var d economy.PayoutDetails
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<10)).Decode(&d); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"asset", "holder", "iban", "bic"} or {"asset": "USDC", "holder", "network": "base", "address"}`)
			return
		}
		d, err := bank.SavePayoutDetails(req.Context(), ws, d)
		if err != nil {
			writeEarningsErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, d)
	})
	r.Get("/v1/money/payout-details", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		list, err := bank.PayoutDetailsOf(req.Context(), ws)
		if err != nil {
			writeEarningsErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"payout_details": list})
	})
	r.Get("/v1/money/earnings-payouts", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		list, err := bank.EarningsPayouts(req.Context(), ws)
		if err != nil {
			writeEarningsErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"payouts": list})
	})
	r.Post("/v1/rooms/{roomID}/payouts", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in rooms.SplitPayout
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_minor", "currency", "shares"}`)
			return
		}
		e, err := roomsStore.Split(req.Context(), chi.URLParam(req, "roomID"), ws, req.Header.Get("Idempotency-Key"), in)
		if err != nil {
			writeEarningsErr(w, err)
			return
		}
		paid, err := bank.PayEarnings(req.Context(), ws, e)
		if err != nil {
			writeEarningsErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"agreement": e.Agreement, "payouts": paid})
	})
}
