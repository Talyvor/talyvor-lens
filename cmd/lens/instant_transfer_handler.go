package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B59.14 — the Instant transfer quote, locked for 30 seconds (economy/instant_transfer_quote.go).
//
//	POST /v1/workspaces/{wsID}/money/instant-transfers/quote {send_minor, send_currency, receive_currency, to, country}
//	                          — you send, the fee, the rate, they get and when it arrives, good until expires_at
//
// The workspace's owner or an admin asks it; a workspace that is not hybrid is refused 409 wrong_money_kind. Mounted in
// the authed group.

type instantTransferBank interface {
	QuoteInstantTransfer(ctx context.Context, workspaceID string, in economy.InstantQuoteRequest) (economy.InstantQuote, error)
}

func mountInstantTransferRoutes(r chi.Router, bank instantTransferBank) {
	r.Post("/v1/workspaces/{wsID}/money/instant-transfers/quote", func(w http.ResponseWriter, req *http.Request) {
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may quote an Instant transfer")
			return
		}
		var in economy.InstantQuoteRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"send_minor", "send_currency", "receive_currency", "to", "country"}`)
			return
		}
		q, err := bank.QuoteInstantTransfer(req.Context(), chi.URLParam(req, "wsID"), in)
		switch {
		case err == nil:
			writeJSONOK(w, http.StatusCreated, q)
		case writeMoneyKindRefusal(w, err):
		case errors.Is(err, economy.ErrInstantTransfer), errors.Is(err, money.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		default:
			writePaymentErr(w, err)
		}
	})
}
