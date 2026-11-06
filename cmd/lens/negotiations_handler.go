package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B52.10 — Counter's negotiation mandate, signed with the owner's passkey, and its term sheets
// (economy/negotiation_mandates.go).
//
//	POST /v1/negotiations/prepare   {task, side, deal, currency, target_minor, walk_away_minor, must_haves, trade_offs,
//	                                deadline, counterparties, channels, web_forms, authority} — the canonical JSON to
//	                                sign, its challenge and the passkeys that may sign it
//	POST /v1/negotiations           {"canonical", "assertion"} — keeps the signed mandate (201); every refusal, a
//	                                deal a counterparty confirmed to another party included, is the same 400
//	                                "Counter can't take this negotiation" (B54.7)
//	GET  /v1/negotiations/{id}      — the mandate and its term sheet per counterparty
//	POST /v1/negotiations/{id}/revoke  — ended at once; its deal is free again
//	POST /v1/negotiations/{id}/terms   {counterparty, party, channel, terms, message} — an offer or counter-offer
//	                                added to the counterparty's term sheet, ours only inside the signed mandate
//	POST /v1/negotiations/domains   {"domain"} — claims a domain the workspace trades as; verified once that domain's
//	                                agent says so (below). A deal is confirmed only to a workspace with one (B54.7)
//
// All of it is the workspace owner's or an admin's. Mounted in the authed group. And, from the counterparty's side
// (B54.7), mounted in the public, rate-limited group:
//
//	POST /v1/a2a/negotiations/{id}/deal  {"deal"} signed under Talyvor's money profile (docs/agent-signatures.md) by the
//	                                     counterparty's agent — confirms that the RFQ, auction or quote is this
//	                                     mandate's, which only then holds it; 409 when it already confirmed another
//	POST /v1/a2a/negotiations/domains    {"workspace_id"} signed the same way — verifies that workspace's claim to
//	                                     the agent's own (registrable) domain; 404 when it made none

type negotiationBank interface {
	PrepareNegotiationMandate(ctx context.Context, workspaceID string, t economy.NegotiationMandateTerms) (canonical, challenge string, credentialIDs []string, err error)
	CreateNegotiationMandate(ctx context.Context, workspaceID, by, canonical string, a *passkey.Assertion) (economy.NegotiationMandate, error)
	NegotiationMandate(ctx context.Context, workspaceID, id string) (economy.NegotiationMandate, error)
	RevokeNegotiationMandate(ctx context.Context, workspaceID, id, by string) (economy.NegotiationMandate, error)
	RecordNegotiationTerms(ctx context.Context, workspaceID, id string, o economy.NegotiationOffer) (economy.NegotiationOffer, error)
	ConfirmNegotiationDeal(ctx context.Context, id, domain, deal string) error
	ClaimNegotiationDomain(ctx context.Context, workspaceID, domain, by string) (economy.NegotiationDomain, error)
	VerifyNegotiationDomain(ctx context.Context, workspaceID, domain string) error
}

func mountNegotiationRoutes(r chi.Router, bank negotiationBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (ws, by string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "negotiations are a workspace's: use a workspace's key")
			return "", "", false
		}
		who, isOwner := storedanswers.OwnerOrAdmin(req.Context())
		if !isOwner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may sign, read or use its negotiation mandates")
			return "", "", false
		}
		return ws, who, true
	}
	reply := func(w http.ResponseWriter, status int, out any, err error) {
		switch {
		case errors.Is(err, economy.ErrNegotiationOneSide):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrNegotiationMandate), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrNegotiationNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, status, out)
		}
	}
	r.Post("/v1/negotiations/prepare", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		var t economy.NegotiationMandateTerms
		if err := json.NewDecoder(req.Body).Decode(&t); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be the mandate's terms: {"task", "side", "currency", "target_minor", "walk_away_minor", "deadline", "counterparties", "channels", "authority", …}`)
			return
		}
		canonical, challenge, creds, err := bank.PrepareNegotiationMandate(req.Context(), ws, t)
		reply(w, http.StatusOK, map[string]any{"canonical": canonical, "challenge": challenge, "allow_credentials": creds,
			"user_verification": "required"}, err)
	})
	r.Post("/v1/negotiations", func(w http.ResponseWriter, req *http.Request) {
		ws, by, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Canonical string             `json:"canonical"`
			Assertion *passkey.Assertion `json:"assertion"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Canonical == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"canonical": "<the JSON prepare returned>", "assertion": {…}}`)
			return
		}
		m, err := bank.CreateNegotiationMandate(req.Context(), ws, by, in.Canonical, in.Assertion)
		if errors.Is(err, economy.ErrNegotiationMandate) || errors.Is(err, passkey.ErrInvalid) {
			// One answer for every refusal, so a refusal in a deal confirmed to another party tells nobody it is one.
			writeJSONErr(w, http.StatusBadRequest, "Counter can't take this negotiation")
			return
		}
		reply(w, http.StatusCreated, m, err)
	})
	r.Post("/v1/negotiations/domains", func(w http.ResponseWriter, req *http.Request) {
		ws, by, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Domain) == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"domain": "<a domain the workspace trades as>"}`)
			return
		}
		d, err := bank.ClaimNegotiationDomain(req.Context(), ws, in.Domain, by)
		reply(w, http.StatusOK, d, err)
	})
	r.Get("/v1/negotiations/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		m, err := bank.NegotiationMandate(req.Context(), ws, chi.URLParam(req, "id"))
		reply(w, http.StatusOK, m, err)
	})
	r.Post("/v1/negotiations/{id}/revoke", func(w http.ResponseWriter, req *http.Request) {
		ws, by, ok := owner(w, req)
		if !ok {
			return
		}
		m, err := bank.RevokeNegotiationMandate(req.Context(), ws, chi.URLParam(req, "id"), by)
		reply(w, http.StatusOK, m, err)
	})
	r.Post("/v1/negotiations/{id}/terms", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		var o economy.NegotiationOffer
		if err := json.NewDecoder(req.Body).Decode(&o); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"counterparty", "party": "ours"|"theirs", "channel", "terms": {"price_minor", …}, "message"?}`)
			return
		}
		out, err := bank.RecordNegotiationTerms(req.Context(), ws, chi.URLParam(req, "id"), o)
		reply(w, http.StatusCreated, out, err)
	})
}

func mountNegotiationAgentRoutes(r chi.Router, v *agentsig.Verifier, bank negotiationBank) {
	r.With(v.Middleware(agentsig.Money)).Post("/v1/a2a/negotiations/{id}/deal", func(w http.ResponseWriter, req *http.Request) {
		trust, _ := agentsig.FromContext(req.Context())
		if trust.Domain == "" {
			writeJSONErr(w, http.StatusUnauthorized, "a counterparty confirms a deal with its agent's signature (docs/agent-signatures.md)")
			return
		}
		var in struct {
			Deal string `json:"deal"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Deal) == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"deal": "<the RFQ, auction or quote reference>"}`)
			return
		}
		switch err := bank.ConfirmNegotiationDeal(req.Context(), chi.URLParam(req, "id"), trust.Domain, in.Deal); {
		case errors.Is(err, economy.ErrNegotiationNotFound):
			writeJSONErr(w, http.StatusNotFound, "no such negotiation")
		case errors.Is(err, economy.ErrNegotiationOneSide):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrNegotiationMandate):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not confirm the deal")
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"deal": strings.ToLower(strings.TrimSpace(in.Deal)), "confirmed_by": trust.Domain})
		}
	})
	r.With(v.Middleware(agentsig.Money)).Post("/v1/a2a/negotiations/domains", func(w http.ResponseWriter, req *http.Request) {
		trust, _ := agentsig.FromContext(req.Context())
		if trust.Domain == "" {
			writeJSONErr(w, http.StatusUnauthorized, "a domain's agent verifies it with its signature (docs/agent-signatures.md)")
			return
		}
		var in struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&in); err != nil || in.WorkspaceID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"workspace_id": "<the workspace that claimed this domain>"}`)
			return
		}
		switch err := bank.VerifyNegotiationDomain(req.Context(), in.WorkspaceID, trust.Domain); {
		case errors.Is(err, economy.ErrNegotiationNotFound):
			writeJSONErr(w, http.StatusNotFound, "no such claim")
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not verify the domain")
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"workspace_id": in.WorkspaceID, "verified": true})
		}
	})
}
