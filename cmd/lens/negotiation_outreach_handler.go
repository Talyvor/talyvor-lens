package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B52.12 — Counter reaching the other side, honestly (economy/negotiation_outreach.go).
//
//	POST /v1/negotiations/{id}/outreach        {counterparty, channel, to, benchmarks_minor?, mention_best_quote?} —
//	                                           the engine's next move sent to a verified agent ("agent", to = its id)
//	                                           or answered for a web form ("web_form", to = its https URL); the first
//	                                           message to a counterparty says it is from an AI agent acting for the
//	                                           owner. 422 for a refused site, an unverified agent or email.
//	GET  /v1/negotiations/inbox                — the messages Counter sent to this workspace's agents
//	POST /v1/negotiations/inbox/{message}/reply {message} — a reply, threaded into that message's deal for its owner
//
// All of it is the workspace owner's or an admin's: the sender's for outreach, the agent's workspace's for the inbox.
// Mounted in the authed group.

type negotiationOutreachBank interface {
	SendNegotiationOutreach(ctx context.Context, workspaceID, id, counterparty, channel, to string, benchmarks []int64,
		mentionRival bool, ai economy.NegotiationInferer, model string) (economy.NegotiationOutreach, error)
	NegotiationInbox(ctx context.Context, workspaceID string) ([]economy.NegotiationInboxMessage, error)
	ReplyToNegotiationMessage(ctx context.Context, workspaceID string, id int64, body string) (economy.NegotiationMessage, error)
}

func mountNegotiationOutreachRoutes(r chi.Router, bank negotiationOutreachBank, ai economy.NegotiationInferer, model string) {
	owner := func(w http.ResponseWriter, req *http.Request, in any) (ws string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "negotiations are a workspace's: use a workspace's key")
			return "", false
		}
		if _, isOwner := storedanswers.OwnerOrAdmin(req.Context()); !isOwner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may use its negotiations")
			return "", false
		}
		if in != nil {
			if err := json.NewDecoder(req.Body).Decode(in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "body must be JSON: see the route's fields")
				return "", false
			}
		}
		return ws, true
	}
	reply := func(w http.ResponseWriter, status int, out any, err error) {
		switch {
		case errors.Is(err, economy.ErrNegotiationChannel):
			writeJSONErr(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, economy.ErrNegotiationMandate):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrNegotiationNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, status, out)
		}
	}
	r.Post("/v1/negotiations/{id}/outreach", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Counterparty string  `json:"counterparty"`
			Channel      string  `json:"channel"`
			To           string  `json:"to"`
			Benchmarks   []int64 `json:"benchmarks_minor"`
			MentionBest  bool    `json:"mention_best_quote"`
		}
		if ws, ok := owner(w, req, &in); ok {
			o, err := bank.SendNegotiationOutreach(req.Context(), ws, chi.URLParam(req, "id"), in.Counterparty, in.Channel, in.To,
				in.Benchmarks, in.MentionBest, ai, model)
			reply(w, http.StatusOK, o, err)
		}
	})
	r.Get("/v1/negotiations/inbox", func(w http.ResponseWriter, req *http.Request) {
		if ws, ok := owner(w, req, nil); ok {
			msgs, err := bank.NegotiationInbox(req.Context(), ws)
			reply(w, http.StatusOK, map[string]any{"messages": msgs}, err)
		}
	})
	r.Post("/v1/negotiations/inbox/{message}/reply", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Message string `json:"message"`
		}
		if ws, ok := owner(w, req, &in); ok {
			id, err := strconv.ParseInt(chi.URLParam(req, "message"), 10, 64)
			if err != nil {
				writeJSONErr(w, http.StatusNotFound, economy.ErrNegotiationNotFound.Error())
				return
			}
			m, err := bank.ReplyToNegotiationMessage(req.Context(), ws, id, in.Message)
			reply(w, http.StatusCreated, m, err)
		}
	})
}
