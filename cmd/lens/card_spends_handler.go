package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B59.21 — approval before the card: on a preapproved card programme the agent asks before it pays
// (economy/card_spends.go).
//
//	POST /v1/money/card-spends      {merchant, amount_minor, currency, standing_card_id, agent_id, funding,
//	                                idempotency_key}, or the key as the Idempotency-Key header — the agent asks to pay
//	                                merchant amount_minor by card: on a one-time card, or with standing_card_id by
//	                                raising that card's limit. Within the agent's rules: 201, the card issued or the
//	                                limit raised. Outside them: 202 {status: pending_approval, approval_id}, decided in
//	                                the approvals inbox; asked again with the same key once approved, 201. On a
//	                                realtime programme: 409, and the agent pays with its card as it does today.
//	GET  /v1/money/card-spends      ?agent_id= — the card spends, newest first
//	GET  /v1/money/card-spends/{id} one card spend, with its one-time card once issued
//
// An agent's own key asks for its own; the owner or an admin names the agent. Mounted in the authed group.

type cardSpendBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	RequestCardSpend(ctx context.Context, workspaceID string, in economy.CardSpend) (economy.CardSpend, error)
	CardSpend(ctx context.Context, workspaceID, agentID, id string) (economy.CardSpend, error)
	CardSpends(ctx context.Context, workspaceID, agentID string) ([]economy.CardSpend, error)
}

func mountCardSpendRoutes(r chi.Router, bank cardSpendBank) {
	caller := func(w http.ResponseWriter, req *http.Request) (ws, agentID string, ok bool) {
		return moneyCaller(w, req, bank.AgentOfKey)
	}
	r.Post("/v1/money/card-spends", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		var in economy.CardSpend
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"merchant", "amount_minor", "currency", "standing_card_id", "idempotency_key"}`)
			return
		}
		if k := req.Header.Get("Idempotency-Key"); k != "" {
			in.IdempotencyKey = k
		}
		if agentID == "" {
			agentID = in.AgentID
		}
		sp, err := bank.RequestCardSpend(req.Context(), ws, economy.CardSpend{AgentID: agentID, Merchant: in.Merchant, AmountMinor: in.AmountMinor,
			Currency: in.Currency, Funding: in.Funding, StandingCardID: in.StandingCardID, IdempotencyKey: in.IdempotencyKey})
		if err != nil {
			if !writePendingApproval(w, err) {
				writeCardSpendErr(w, err)
			}
			return
		}
		writeJSONOK(w, http.StatusCreated, sp)
	})
	r.Get("/v1/money/card-spends", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		if agentID == "" {
			agentID = req.URL.Query().Get("agent_id")
		}
		list, err := bank.CardSpends(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"card_spends": list})
	})
	r.Get("/v1/money/card-spends/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		sp, err := bank.CardSpend(req.Context(), ws, agentID, chi.URLParam(req, "id"))
		if err != nil {
			writeCardSpendErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, sp)
	})
}

func writeCardSpendErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrCardSpend), errors.Is(err, economy.ErrReusableCard):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrCardSpendNotFound), errors.Is(err, economy.ErrReusableCardNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrCardProgrammeRealtime):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeCardErr(w, err)
	}
}
