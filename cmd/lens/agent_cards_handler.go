package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
//	POST /v1/workspaces/{wsID}/agents/{id}/card/draws-on   {draws_on: "credits" | "account"}   (B30.30) what the card
//	     spends: the agent's credits, or its currency account in the card's currency, which must be open
//	     old one did; the old one is cancelled: every purchase on it is declined and nothing leaves the agent. 201, the new card
//	POST /v1/workspaces/{wsID}/agents/{id}/card/cancel    (B28.321) the card, cancelled for good; the agent holds no card
//	POST /v1/workspaces/{wsID}/agents/{id}/card/cancel    (B28.321) the card, cancelled for good; the agent holds no card
//	     until it is issued a new one. 200, the cancelled card
//	POST /v1/workspaces/{wsID}/agents/{id}/card/merchant-lock   {lock: "off" | "first" | "merchant", merchant_id}
//	     (B30.31) lock the card to one merchant: the merchant of its next approved purchase, or the merchant id the
//	     owner sets; a purchase from any other merchant is declined, the lock named in its reason
//
// Each purchase is decided by Stripe's call to POST /v1/agent-cards/authorizations (agentcard.Handler), by
// the agent's rules and balance; the decision and the ECB rate it was priced at are on each authorisation.
// Mounted in the authed group; issuing takes the workspace's owner or an admin, reading any credential.

type agentCardBank interface {
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
	GetAgentCard(ctx context.Context, workspaceID, agentID string) (economy.AgentCard, error)
	SaveAgentCard(ctx context.Context, workspaceID string, c economy.AgentCard) (economy.AgentCard, error)
	SetAgentCardFrozen(ctx context.Context, workspaceID, agentID string, frozen bool) (economy.AgentCard, error)
	SetAgentCardDrawsOn(ctx context.Context, workspaceID, agentID, drawsOn string) (economy.AgentCard, error)
	CancelAgentCard(ctx context.Context, workspaceID, agentID string) (economy.AgentCard, error)
	ReplaceAgentCard(ctx context.Context, workspaceID, agentID, oldCardID string, next economy.AgentCard) (economy.AgentCard, error)
	SetAgentCardMerchantLock(ctx context.Context, workspaceID, agentID, lock, merchantID string) (economy.AgentCard, error)
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
		card, err := issuer.IssueCard(req.Context(), wsID, agentID, agentCardName(req.Context(), bank, wsID, agentID), holder)
		if writeIssuerErr(w, err) {
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
	// B28.321: the new card is issued at Stripe first and recorded with the old one's cancel in one transaction, so the agent
	// never holds two working cards; the old card is cancelled at Stripe after, though Lens declines it from that commit.
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/card/replace", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ctx, wsID, agentID := req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		old, err := bank.GetAgentCard(ctx, wsID, agentID)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrNoAgentCard):
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		next, err := issuer.ReplaceCard(ctx, wsID, agentCardName(ctx, bank, wsID, agentID), old)
		if writeIssuerErr(w, err) {
			return
		}
		card, err := bank.ReplaceAgentCard(ctx, wsID, agentID, old.ID, next)
		if err != nil {
			cancelAtStripe(ctx, issuer, wsID, next.ID) // no agent holds it
			switch {
			case errors.Is(err, economy.ErrNoAgentCard): // cancelled or replaced while this one was issued
				writeJSONErr(w, http.StatusConflict, "the card was cancelled or replaced meanwhile: nothing was changed")
			case errors.Is(err, economy.ErrLockedDown):
				writeJSONErr(w, http.StatusForbidden, err.Error())
			default:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		cancelAtStripe(ctx, issuer, wsID, old.ID)
		writeJSONOK(w, http.StatusCreated, card)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/card/cancel", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ctx, wsID := req.Context(), chi.URLParam(req, "wsID")
		card, err := bank.CancelAgentCard(ctx, wsID, chi.URLParam(req, "agentID"))
		switch {
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrNoAgentCard):
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		cancelAtStripe(ctx, issuer, wsID, card.ID)
		writeJSONOK(w, http.StatusOK, card)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/card/draws-on", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			DrawsOn string `json:"draws_on"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"draws_on": "credits" | "account"}`)
			return
		}
		card, err := bank.SetAgentCardDrawsOn(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), body.DrawsOn)
		switch {
		case errors.Is(err, economy.ErrCardDrawsOn):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrNoAgentCard):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrMoneyAccountNotFound), errors.Is(err, economy.ErrMoneyAccountNotOpen):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, card)
		}
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/card/merchant-lock", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Lock       string `json:"lock"`
			MerchantID string `json:"merchant_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"lock": "off" | "first" | "merchant", "merchant_id": "…"}`)
			return
		}
		card, err := bank.SetAgentCardMerchantLock(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), body.Lock, body.MerchantID)
		switch {
		case errors.Is(err, economy.ErrCardMerchantLock):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrNoAgentCard):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrLockedDown):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, card)
		}
	}))
}

// agentCardName is the agent's name for its card at Stripe, or its id.
func agentCardName(ctx context.Context, bank agentCardBank, wsID, agentID string) string {
	if book, err := bank.AgentBook(ctx, wsID); err == nil {
		for _, a := range book.Agents {
			if a.ID == agentID {
				return a.Name
			}
		}
	}
	return agentID
}

// writeIssuerErr answers what Stripe's issuing returned, if it failed, and says whether it did.
func writeIssuerErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, agentcard.ErrLiveKey):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, errNoTestMode): // B25.6
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, agentcard.ErrNotConfigured):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	case stripeRefusal(err) != "": // B26.12: what Stripe will not take in the cardholder
		writeJSONErr(w, http.StatusBadRequest, stripeRefusal(err))
	default:
		writeJSONErr(w, http.StatusBadGateway, err.Error())
	}
	return true
}

// cancelAtStripe cancels cardID at Stripe (B28.321). Lens declines every purchase on a card it cancelled whatever Stripe
// says, so a failure here is logged, not answered.
func cancelAtStripe(ctx context.Context, issuer agentcard.Issuer, wsID, cardID string) {
	if issuer == nil {
		return
	}
	if err := issuer.CancelCard(ctx, wsID, cardID); err != nil {
		slog.Warn("agent card: not cancelled at Stripe; Lens declines it anyway", "workspace", wsID, "card", cardID, "err", err.Error())
	}
}
