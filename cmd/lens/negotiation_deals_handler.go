package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B52.13 — Counter closes inside the mandate; everything else waits for the owner's signature
// (economy/negotiation_deals.go).
//
//	POST /v1/negotiations/{id}/close          {counterparty, payee_id?, agent_id?} — accepts their latest offer: 200
//	                                          accepted (and paid inside the agent's limits), 202 waiting for the owner
//	POST /v1/negotiations/{id}/deal/approval  — the statement of their exact terms against the mandate, its challenge
//	                                          and the passkeys that may sign it
//	POST /v1/negotiations/{id}/deal/approve   {"statement", "assertion"} — the owner accepts the waiting deal; it is paid
//	POST /v1/negotiations/{id}/deal/decline   — the waiting deal is declined; nothing is paid
//
// All of it is the workspace owner's or an admin's. Mounted in the authed group.

type negotiationDealBank interface {
	CloseNegotiation(ctx context.Context, workspaceID, id, by string, in economy.NegotiationCloseIn) (economy.NegotiationDeal, error)
	PrepareDealApproval(ctx context.Context, workspaceID, id string) (statement, challenge string, credentialIDs []string, err error)
	ApproveNegotiationDeal(ctx context.Context, workspaceID, id, by, statement string, a *passkey.Assertion) (economy.NegotiationDeal, error)
	DeclineNegotiationDeal(ctx context.Context, workspaceID, id, by string) (economy.NegotiationDeal, error)
}

func mountNegotiationDealRoutes(r chi.Router, bank negotiationDealBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (ws, by string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "negotiations are a workspace's: use a workspace's key")
			return "", "", false
		}
		who, isOwner := storedanswers.OwnerOrAdmin(req.Context())
		if !isOwner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may close or approve its deals")
			return "", "", false
		}
		return ws, who, true
	}
	reply := func(w http.ResponseWriter, status int, out any, err error) {
		switch {
		case errors.Is(err, economy.ErrNegotiationDealDecided), errors.Is(err, economy.ErrPaymentRequestConflict):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrNegotiationMandate), errors.Is(err, economy.ErrPaymentRequest), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrNegotiationNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, status, out)
		}
	}
	r.Post("/v1/negotiations/{id}/close", func(w http.ResponseWriter, req *http.Request) {
		ws, by, ok := owner(w, req)
		if !ok {
			return
		}
		var in economy.NegotiationCloseIn
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"counterparty", "payee_id"?, "agent_id"?}`)
			return
		}
		d, err := bank.CloseNegotiation(req.Context(), ws, chi.URLParam(req, "id"), by, in)
		status := http.StatusOK
		if d.Status == "awaiting_approval" {
			status = http.StatusAccepted
		}
		reply(w, status, d, err)
	})
	r.Post("/v1/negotiations/{id}/deal/approval", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		statement, challenge, creds, err := bank.PrepareDealApproval(req.Context(), ws, chi.URLParam(req, "id"))
		reply(w, http.StatusOK, map[string]any{"statement": statement, "challenge": challenge, "allow_credentials": creds,
			"user_verification": "required"}, err)
	})
	r.Post("/v1/negotiations/{id}/deal/approve", func(w http.ResponseWriter, req *http.Request) {
		ws, by, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Statement string             `json:"statement"`
			Assertion *passkey.Assertion `json:"assertion"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Statement == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"statement": "<the statement approval returned>", "assertion": {…}}`)
			return
		}
		d, err := bank.ApproveNegotiationDeal(req.Context(), ws, chi.URLParam(req, "id"), by, in.Statement, in.Assertion)
		reply(w, http.StatusOK, d, err)
	})
	r.Post("/v1/negotiations/{id}/deal/decline", func(w http.ResponseWriter, req *http.Request) {
		ws, by, ok := owner(w, req)
		if !ok {
			return
		}
		d, err := bank.DeclineNegotiationDeal(req.Context(), ws, chi.URLParam(req, "id"), by)
		reply(w, http.StatusOK, d, err)
	})
}
