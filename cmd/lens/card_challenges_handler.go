package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
)

// B59.22 — 3-D Secure: an online card challenge on an agent's card waits for the owner (economy/card_challenges.go).
// Each challenge is an approval of kind card_challenge in the approvals inbox, decided there with the owner's signed
// answer (POST /v1/workspaces/{wsID}/agents/approvals/{id}/approve or /deny, with {"assertion"}).
//
//	GET  /v1/money/card-challenges   the workspace's 3-D Secure challenges, newest first, and where each stands
//	POST /v1/money/card-challenges   {card_id, merchant, amount_minor, currency} — the Sandbox challenges a payment on one
//	                                 of the workspace's agents' Sandbox cards, as an issuer would: 201, the challenge and
//	                                 the approval it waits on
//
// The owner or an admin only. Mounted in the authed group.

type cardChallengeBank interface {
	CardChallenges(ctx context.Context, workspaceID string) ([]economy.CardChallenge, error)
	SandboxCardChallenge(ctx context.Context, workspaceID, cardID, merchant string, amount money.Amount) (economy.CardChallenge, error)
}

func mountCardChallengeRoutes(r chi.Router, bank cardChallengeBank) {
	r.Get("/v1/money/card-challenges", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		list, err := bank.CardChallenges(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"card_challenges": list})
	}))
	r.Post("/v1/money/card-challenges", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		var in struct {
			CardID      string `json:"card_id"`
			Merchant    string `json:"merchant"`
			AmountMinor int64  `json:"amount_minor"`
			Currency    string `json:"currency"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || ws == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"card_id", "merchant", "amount_minor", "currency"}`)
			return
		}
		c, err := bank.SandboxCardChallenge(req.Context(), ws, in.CardID, in.Merchant, money.Amount{Minor: in.AmountMinor, Asset: in.Currency})
		switch {
		case errors.Is(err, economy.ErrCardChallenge), errors.Is(err, money.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, c)
		}
	}))
}
