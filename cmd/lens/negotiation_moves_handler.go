package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B52.11 — Counter's deal engine: code sets every number, the model writes the words (economy/negotiation_engine.go).
//
//	POST /v1/negotiations/{id}/plan     {"benchmarks_minor"?} — the opening offer, the concession schedule, who is asked
//	                                    for a quote and when Counter walks away
//	POST /v1/negotiations/{id}/move     {counterparty, channel, benchmarks_minor?, mention_best_quote?} — the engine's
//	                                    next move with one counterparty, worded; a counter is added to its term sheet
//	POST /v1/negotiations/{id}/replies  {counterparty, channel, message} — a counterparty's reply read into terms and
//	                                    added to its term sheet, or answered with what the owner has to read first
//
// All of it is the workspace owner's or an admin's, under a signed, live mandate. Mounted in the authed group.

type negotiationEngine interface {
	PlanNegotiation(ctx context.Context, workspaceID, id string, benchmarks []int64) (economy.NegotiationPlan, error)
	NextNegotiationMove(ctx context.Context, workspaceID, id, counterparty, channel string, benchmarks []int64, mentionRival bool,
		ai economy.NegotiationInferer, model string) (economy.NegotiationMove, error)
	ReceiveNegotiationReply(ctx context.Context, workspaceID, id, counterparty, channel, text string, ai economy.NegotiationInferer,
		model string) (economy.NegotiationReply, error)
}

func mountNegotiationMoveRoutes(r chi.Router, bank negotiationEngine, ai economy.NegotiationInferer, model string) {
	owner := func(w http.ResponseWriter, req *http.Request, in any) (ws string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "negotiations are a workspace's: use a workspace's key")
			return "", false
		}
		if _, isOwner := storedanswers.OwnerOrAdmin(req.Context()); !isOwner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may use its negotiation mandates")
			return "", false
		}
		if err := json.NewDecoder(req.Body).Decode(in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be JSON: see the route's fields")
			return "", false
		}
		return ws, true
	}
	reply := func(w http.ResponseWriter, out any, err error) {
		switch {
		case errors.Is(err, economy.ErrNegotiationMandate):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrNegotiationNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	}
	r.Post("/v1/negotiations/{id}/plan", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Benchmarks []int64 `json:"benchmarks_minor"`
		}
		if ws, ok := owner(w, req, &in); ok {
			p, err := bank.PlanNegotiation(req.Context(), ws, chi.URLParam(req, "id"), in.Benchmarks)
			reply(w, p, err)
		}
	})
	r.Post("/v1/negotiations/{id}/move", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Counterparty string  `json:"counterparty"`
			Channel      string  `json:"channel"`
			Benchmarks   []int64 `json:"benchmarks_minor"`
			MentionBest  bool    `json:"mention_best_quote"`
		}
		if ws, ok := owner(w, req, &in); ok {
			m, err := bank.NextNegotiationMove(req.Context(), ws, chi.URLParam(req, "id"), in.Counterparty, in.Channel, in.Benchmarks,
				in.MentionBest, ai, model)
			reply(w, m, err)
		}
	})
	r.Post("/v1/negotiations/{id}/replies", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Counterparty string `json:"counterparty"`
			Channel      string `json:"channel"`
			Message      string `json:"message"`
		}
		if ws, ok := owner(w, req, &in); ok {
			rr, err := bank.ReceiveNegotiationReply(req.Context(), ws, chi.URLParam(req, "id"), in.Counterparty, in.Channel, in.Message, ai, model)
			reply(w, rr, err)
		}
	})
}
