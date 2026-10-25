package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/a2acard"
	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B45.28 — agents approve agents within their own limits, and the owner's approvals are AP2 v0.2 mandates
// (internal/economy/agent_approvers.go, docs/ap2-mandates.md). The owner, signing on the Talyvor app:
//
//	POST   /v1/workspaces/{wsID}/agents/{id}/approver/challenge {"approver_agent_id"}   the challenge to sign, and the
//	                                                     limit the approver would approve up to (its own)
//	PUT    /v1/workspaces/{wsID}/agents/{id}/approver {"approver_agent_id", "assertion"}   the approver of {id}'s requests,
//	                                                     kept as an AP2 open Payment Mandate
//	GET    /v1/workspaces/{wsID}/agents/{id}/approver    the approver in force and its mandate
//	DELETE /v1/workspaces/{wsID}/agents/{id}/approver    take it away, at once
//	GET    /v1/workspaces/{wsID}/agents/approvals/{approvalID}/evidence   the owner's closed mandate, or the agent's
//	                                                     evidence and the open mandate it ran under
//
// the approver agent, by its own key only:
//
//	POST   /v1/workspaces/{wsID}/agents/{id}/approvals/{approvalID}/approve   approve, as agent {id}; 409 above its limits,
//	                                                     and the request waits for the owner
//
// and anyone, to check a mandate or evidence:
//
//	GET    /.well-known/talyvor-ap2/jwks.json            the key they are signed with

type approverBank interface {
	ApproverChallenge(ctx context.Context, workspaceID, agentID, approverID string) (string, int64, error)
	SetAgentApprover(ctx context.Context, workspaceID, agentID, approverID string, a *passkey.Assertion) (economy.AgentApprover, error)
	GetAgentApprover(ctx context.Context, workspaceID, agentID string) (economy.AgentApprover, error)
	RemoveAgentApprover(ctx context.Context, workspaceID, agentID string) error
	ApproveAsAgent(ctx context.Context, workspaceID, approverID, approvalID string) (economy.AgentApproval, error)
	ApprovalEvidence(ctx context.Context, workspaceID, approvalID string) (economy.ApprovalEvidence, error)
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
}

type approverCards interface {
	Card(ctx context.Context, agentID string) (a2acard.Card, error)
}

func writeApproverErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrApprovalNotFound), errors.Is(err, a2acard.ErrArchived):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrPasskeyRequired), errors.Is(err, passkey.ErrInvalid), errors.Is(err, economy.ErrNotApprover),
		errors.Is(err, economy.ErrAgentRule):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, economy.ErrAboveApproverLimits):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrApprover), errors.Is(err, economy.ErrAgentArchived):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

func mountAgentApproverRoutes(r chi.Router, bank approverBank, cards approverCards) {
	type body = struct {
		ApproverAgentID string             `json:"approver_agent_id"`
		Assertion       *passkey.Assertion `json:"assertion"`
	}
	read := func(w http.ResponseWriter, req *http.Request) (body, bool) {
		var b body
		if err := json.NewDecoder(req.Body).Decode(&b); err != nil || b.ApproverAgentID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"approver_agent_id": "<agent>", "assertion": <the owner's signature, on PUT>}`)
			return b, false
		}
		return b, true
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/approver/challenge", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		b, ok := read(w, req)
		if !ok {
			return
		}
		c, limit, err := bank.ApproverChallenge(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), b.ApproverAgentID)
		if err != nil {
			writeApproverErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "limit_ulxc": limit, "user_verification": "required"})
	}))
	r.Put("/v1/workspaces/{wsID}/agents/{agentID}/approver", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		b, ok := read(w, req)
		if !ok {
			return
		}
		if b.Assertion != nil { // the open mandate is bound to the approver's Agent Card key: make sure it has one
			if _, err := cards.Card(req.Context(), b.ApproverAgentID); err != nil {
				writeApproverErr(w, err)
				return
			}
		}
		a, err := bank.SetAgentApprover(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), b.ApproverAgentID, b.Assertion)
		if err != nil {
			writeApproverErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, a)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/approver", func(w http.ResponseWriter, req *http.Request) {
		a, err := bank.GetAgentApprover(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		if errors.Is(err, economy.ErrNotApprover) {
			writeJSONErr(w, http.StatusNotFound, "this agent has no approver: its requests go to the owner")
			return
		}
		if err != nil {
			writeApproverErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, a)
	})
	r.Delete("/v1/workspaces/{wsID}/agents/{agentID}/approver", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		err := bank.RemoveAgentApprover(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		if errors.Is(err, economy.ErrNotApprover) {
			writeJSONErr(w, http.StatusNotFound, "this agent has no approver")
			return
		}
		if err != nil {
			writeApproverErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/approvals/{approvalID}/evidence", func(w http.ResponseWriter, req *http.Request) {
		e, err := bank.ApprovalEvidence(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "approvalID"))
		if err != nil {
			writeApproverErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"approval_id": e.ApprovalID, "owner_mandate": e.OwnerMandate,
			"approved_by_agent_id": e.ApprovedByAgentID, "agent_evidence": e.AgentEvidence, "approver_mandate": e.ApproverMandate,
			"jwks_url": "/.well-known/talyvor-ap2/jwks.json"})
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/approvals/{approvalID}/approve", func(w http.ResponseWriter, req *http.Request) {
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		// Only the approver's own key: the owner approves as the owner, on the approvals route, signed.
		keyAgent := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
		}
		if keyAgent == "" || keyAgent != agentID {
			writeJSONErr(w, http.StatusForbidden, "only the approver agent's own key may approve as it")
			return
		}
		a, err := bank.ApproveAsAgent(req.Context(), wsID, agentID, chi.URLParam(req, "approvalID"))
		if err != nil {
			writeApproverErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, a)
	})
}

func mountAP2PublicRoutes(r chi.Router, signer *ap2.Signer) {
	r.Get("/.well-known/talyvor-ap2/jwks.json", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		writeJSONOK(w, http.StatusOK, signer.JWKS())
	})
}
