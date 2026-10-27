package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B46.10 — an agent's single-use cards, locked to one merchant and capped (economy/money_cards.go).
//
//	POST /v1/money/cards              {purpose, currency, cards: [{merchant, cap_minor}], agent_id, funding,
//	                                  idempotency_key}, or the key as the Idempotency-Key header — asks for up to 10
//	                                  cards for one job, one per merchant, held for one approval. An agent's own key asks
//	                                  for its own; the owner or an admin names the agent.
//	POST /v1/money/cards/{id}/approve the owner or an admin approves the batch: every card's cap is held and every card
//	                                  issued. Each pays its merchant once, up to its cap, within 24 hours.
//	GET  /v1/money/cards              ?agent_id= — the batches with their cards, newest first
//	GET  /v1/money/cards/{id}         one batch with its cards and where each stands
//
// The workspace is the key's. Mounted in the authed group.

type moneyCardBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	RequestCardBatch(ctx context.Context, workspaceID string, in economy.CardBatch) (economy.CardBatch, error)
	ApproveCardBatch(ctx context.Context, workspaceID, id, by string) (economy.CardBatch, error)
	CardBatch(ctx context.Context, workspaceID, agentID, id string) (economy.CardBatch, error)
	CardBatches(ctx context.Context, workspaceID, agentID string) ([]economy.CardBatch, error)
}

func mountMoneyCardRoutes(r chi.Router, bank moneyCardBank) {
	caller := func(w http.ResponseWriter, req *http.Request) (ws, agentID string, ok bool) {
		return moneyCaller(w, req, bank.AgentOfKey)
	}
	r.Post("/v1/money/cards", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		var in economy.CardBatch
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"purpose", "currency", "cards": [{"merchant", "cap_minor"}], "idempotency_key"}`)
			return
		}
		if k := req.Header.Get("Idempotency-Key"); k != "" {
			in.IdempotencyKey = k
		}
		if agentID == "" {
			agentID = in.AgentID
		}
		cards := make([]economy.MoneyCard, len(in.Cards))
		for i, c := range in.Cards {
			cards[i] = economy.MoneyCard{Merchant: c.Merchant, CapMinor: c.CapMinor}
		}
		b, err := bank.RequestCardBatch(req.Context(), ws, economy.CardBatch{AgentID: agentID, Purpose: in.Purpose, Currency: in.Currency,
			Funding: in.Funding, IdempotencyKey: in.IdempotencyKey, Cards: cards})
		if err != nil {
			writeCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, b)
	})
	r.Post("/v1/money/cards/{id}/approve", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := caller(w, req)
		if !ok {
			return
		}
		who, owner := storedanswers.OwnerOrAdmin(req.Context())
		if !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin approves an agent's cards")
			return
		}
		b, err := bank.ApproveCardBatch(req.Context(), ws, chi.URLParam(req, "id"), who)
		if err != nil {
			writeCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, b)
	})
	r.Get("/v1/money/cards", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		if agentID == "" {
			agentID = req.URL.Query().Get("agent_id")
		}
		list, err := bank.CardBatches(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"batches": list})
	})
	r.Get("/v1/money/cards/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		b, err := bank.CardBatch(req.Context(), ws, agentID, chi.URLParam(req, "id"))
		if err != nil {
			writeCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, b)
	})
}

func writeCardErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrCardBatch):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrCardBatchNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrAgentWalletsOff):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		writePaymentErr(w, err)
	}
}
