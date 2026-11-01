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

// B53.2 — the owner's own reusable card, and an agent's, with limits and categories (economy/money_reusable_cards.go).
//
//	POST  /v1/money/reusable-cards      {agent_id, currency, funding, per_payment_limit_minor, daily_limit_minor,
//	                                    allowed_mccs, blocked_mccs, blocks_gambling, idempotency_key}, or the key as the
//	                                    Idempotency-Key header — the owner or an admin issues a reusable card on the
//	                                    owner's own wallet, or with agent_id on that agent's
//	GET   /v1/money/reusable-cards      ?agent_id= — the cards and their controls, newest first; an agent's key reads its own
//	PATCH /v1/money/reusable-cards/{id} {frozen, per_payment_limit_minor, daily_limit_minor, allowed_mccs, blocked_mccs,
//	                                    blocks_gambling} — the owner or an admin changes what is given, and nothing else
//
// The workspace is the key's. Mounted in the authed group, and only when reusableCardsOn.

// reusableCardsOn mounts the routes. It stays false until a live card issuer serves cards (B49, B51): on the fake
// issuer nothing a person could pay with exists, so no screen offers one.
const reusableCardsOn = false

type reusableCardBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	IssueReusableCard(ctx context.Context, workspaceID string, in economy.ReusableCard) (economy.ReusableCard, error)
	SetReusableCardControls(ctx context.Context, workspaceID, id string, c economy.CardControls) (economy.ReusableCard, error)
	ReusableCards(ctx context.Context, workspaceID, agentID string) ([]economy.ReusableCard, error)
}

func mountReusableCardRoutes(r chi.Router, bank reusableCardBank, enabled bool) {
	if !enabled { // stays off until a live issuer serves cards
		return
	}
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, agentID, ok := moneyCaller(w, req, bank.AgentOfKey)
		if ok && agentID != "" {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin issues a card or changes its controls")
			return "", false
		}
		return ws, ok
	}
	r.Post("/v1/money/reusable-cards", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in economy.ReusableCard
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"currency", "per_payment_limit_minor", "daily_limit_minor", "idempotency_key"}`)
			return
		}
		if k := req.Header.Get("Idempotency-Key"); k != "" {
			in.IdempotencyKey = k
		}
		c, err := bank.IssueReusableCard(req.Context(), ws, economy.ReusableCard{AgentID: in.AgentID, Currency: in.Currency, Funding: in.Funding,
			IdempotencyKey: in.IdempotencyKey, PerPaymentLimitMinor: in.PerPaymentLimitMinor, DailyLimitMinor: in.DailyLimitMinor,
			AllowedMCCs: in.AllowedMCCs, BlockedMCCs: in.BlockedMCCs, BlocksGambling: in.BlocksGambling})
		if err != nil {
			writeReusableCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, c)
	})
	r.Get("/v1/money/reusable-cards", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := moneyCaller(w, req, bank.AgentOfKey)
		if !ok {
			return
		}
		if _, isOwner := storedanswers.OwnerOrAdmin(req.Context()); isOwner {
			agentID = req.URL.Query().Get("agent_id")
		}
		list, err := bank.ReusableCards(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"cards": list})
	})
	r.Patch("/v1/money/reusable-cards/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in economy.CardControls
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"frozen", "per_payment_limit_minor", "daily_limit_minor", "allowed_mccs", "blocked_mccs", "blocks_gambling"}`)
			return
		}
		c, err := bank.SetReusableCardControls(req.Context(), ws, chi.URLParam(req, "id"), in)
		if err != nil {
			writeReusableCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
}

func writeReusableCardErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrReusableCard):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrReusableCardNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	default:
		writeCardErr(w, err)
	}
}
