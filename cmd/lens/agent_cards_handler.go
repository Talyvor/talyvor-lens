package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/economy"
)

// B19.12 — agent cards in test mode, through Stripe Issuing (internal/agentcard, economy/agent_cards.go).
//
//	POST /v1/workspaces/{wsID}/agents/{id}/card   {first_name, last_name, email, phone_number, line1, line2, city, postal_code, country}
//	     issue the agent a virtual card, its owner the cardholder — test mode only (class RED): with a live
//	     Stripe key it is 409 and nothing is issued
//	GET  /v1/workspaces/{wsID}/agents/{id}/card   the card and its authorisations, newest first (approved and declined)
//	POST /v1/workspaces/{wsID}/agents/{id}/card/freeze, …/card/unfreeze   (B17.111) the card, frozen or not: every
//	     purchase on a frozen card is declined and nothing leaves the agent
//
// Each purchase is decided by Stripe's call to POST /v1/agent-cards/authorizations (agentcard.Handler), by
// the agent's rules and balance; the decision and the ECB rate it was priced at are on each authorisation.
// Mounted in the authed group; issuing takes the workspace's owner or an admin, reading any credential.

type agentCardBank interface {
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
	GetAgentCard(ctx context.Context, workspaceID, agentID string) (economy.AgentCard, error)
	SaveAgentCard(ctx context.Context, workspaceID string, c economy.AgentCard) (economy.AgentCard, error)
	SetAgentCardFrozen(ctx context.Context, workspaceID, agentID string, frozen bool) (economy.AgentCard, error)
	ListAgentCardAuthorizations(ctx context.Context, workspaceID, agentID string, limit int) ([]economy.CardAuthorizationRecord, error)
}

func mountAgentCardRoutes(r chi.Router, bank agentCardBank, issuer agentcard.Issuer) {
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/card", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		var holder agentcard.Cardholder
		if err := json.NewDecoder(req.Body).Decode(&holder); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the cardholder: {first_name, last_name, email, phone_number, line1, line2, city, postal_code, country}")
			return
		}
		if missing := holder.Validate(); missing != "" {
			writeJSONErr(w, http.StatusBadRequest, missing)
			return
		}
		// The agent must be here and hold no card before Stripe is asked for one.
		_, err := bank.GetAgentCard(req.Context(), wsID, agentID)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		case err == nil:
			writeJSONErr(w, http.StatusConflict, economy.ErrAgentHasCard.Error())
			return
		case !errors.Is(err, economy.ErrNoAgentCard):
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		name := agentID
		if book, err := bank.AgentBook(req.Context(), wsID); err == nil {
			for _, a := range book.Agents {
				if a.ID == agentID {
					name = a.Name
				}
			}
		}
		card, err := issuer.IssueCard(req.Context(), wsID, agentID, name, holder)
		switch {
		case errors.Is(err, agentcard.ErrLiveKey):
			writeJSONErr(w, http.StatusConflict, err.Error())
			return
		case errors.Is(err, errNoTestMode): // B25.6
			writeJSONErr(w, http.StatusForbidden, err.Error())
			return
		case errors.Is(err, agentcard.ErrNotConfigured):
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
			return
		case stripeRefusal(err) != "": // B26.12: what Stripe will not take in the cardholder
			writeJSONErr(w, http.StatusBadRequest, stripeRefusal(err))
			return
		case err != nil:
			writeJSONErr(w, http.StatusBadGateway, err.Error())
			return
		}
		card, err = bank.SaveAgentCard(req.Context(), wsID, card)
		switch {
		case errors.Is(err, economy.ErrAgentHasCard):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, card)
		}
	}))
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/card", func(w http.ResponseWriter, req *http.Request) {
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		card, err := bank.GetAgentCard(req.Context(), wsID, agentID)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrNoAgentCard):
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		auths, err := bank.ListAgentCardAuthorizations(req.Context(), wsID, agentID, 200)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"card": card, "authorizations": auths})
	})
	freeze := func(frozen bool) http.HandlerFunc {
		return ownerOnly(func(w http.ResponseWriter, req *http.Request) {
			card, err := bank.SetAgentCardFrozen(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), frozen)
			switch {
			case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrNoAgentCard):
				writeJSONErr(w, http.StatusNotFound, err.Error())
			case errors.Is(err, economy.ErrLockedDown):
				writeJSONErr(w, http.StatusForbidden, err.Error())
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			default:
				writeJSONOK(w, http.StatusOK, card)
			}
		})
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/card/freeze", freeze(true))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/card/unfreeze", freeze(false))
}
