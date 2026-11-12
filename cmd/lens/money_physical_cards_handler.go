package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B53.19 — the owner's physical card, on the money layer's card issuer (economy/money_physical_cards.go).
//
//	GET  /v1/money/physical-cards                 {"cards": [card]}, newest first; never a card number
//	POST /v1/money/physical-cards                 {name_on_card, address} — 201 the card, inactive and ordered; 422
//	                                              when the address fails its check
//	POST /v1/money/physical-cards/{id}/activate   {last4} — 200 active; 422 not its last four; 409 before it shipped
//	POST /v1/money/physical-cards/{id}/replace    {reason: lost|stolen|damaged} — 201 {"old", "card"}: the old one
//	                                              frozen at the issuer at once, a new one ordered to the same address
//
// The workspace is the key's; only its owner or an admin (spendingOwner). Mounted only while LENS_PHYSICAL_CARDS is
// on: off, every route is 404 and nothing is written.

type physicalCardBank interface {
	PhysicalCards(ctx context.Context, workspaceID string) ([]economy.PhysicalCard, error)
	OrderPhysicalCard(ctx context.Context, workspaceID, nameOnCard string, address economy.CardAddress) (economy.PhysicalCard, error)
	ActivatePhysicalCard(ctx context.Context, workspaceID, id, last4 string) (economy.PhysicalCard, error)
	ReplacePhysicalCard(ctx context.Context, workspaceID, id, reason string) (old, card economy.PhysicalCard, err error)
}

func mountPhysicalCardRoutes(r chi.Router, bank physicalCardBank, on bool) {
	if !on {
		return
	}
	body := func(w http.ResponseWriter, req *http.Request, in any, shape string) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be "+shape)
			return false
		}
		return true
	}
	r.Get("/v1/money/physical-cards", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		cards, err := bank.PhysicalCards(req.Context(), ws)
		if err != nil {
			writePhysicalCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"cards": cards})
	})
	r.Post("/v1/money/physical-cards", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		var in struct {
			NameOnCard string              `json:"name_on_card"`
			Address    economy.CardAddress `json:"address"`
		}
		if !ok || !body(w, req, &in, `{"name_on_card", "address": {"line1", "line2", "city", "postcode", "country"}}`) {
			return
		}
		card, err := bank.OrderPhysicalCard(req.Context(), ws, in.NameOnCard, in.Address)
		if err != nil {
			writePhysicalCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, card)
	})
	r.Post("/v1/money/physical-cards/{id}/activate", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		var in struct {
			Last4 string `json:"last4"`
		}
		if !ok || !body(w, req, &in, `{"last4"}`) {
			return
		}
		card, err := bank.ActivatePhysicalCard(req.Context(), ws, chi.URLParam(req, "id"), in.Last4)
		if err != nil {
			writePhysicalCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, card)
	})
	r.Post("/v1/money/physical-cards/{id}/replace", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		var in struct {
			Reason string `json:"reason"`
		}
		if !ok || !body(w, req, &in, `{"reason": "lost" | "stolen" | "damaged"}`) {
			return
		}
		old, card, err := bank.ReplacePhysicalCard(req.Context(), ws, chi.URLParam(req, "id"), in.Reason)
		if err != nil {
			writePhysicalCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{"old": old, "card": card})
	})
}

func writePhysicalCardErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrPhysicalCard):
		writeJSONErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, economy.ErrPhysicalCardNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrPhysicalCardState):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeCardErr(w, err)
	}
}
