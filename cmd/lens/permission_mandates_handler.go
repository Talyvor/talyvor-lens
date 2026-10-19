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

// B40.2 — permission mandates: what a person allowed an assistant or an agent to prepare and pay, signed with their
// passkey (economy/permission_mandates.go).
//
//	POST /v1/money/permission-mandates/prepare  {principal, agent_id, clients, scope, limits, requires_approval_above,
//	                                             new_payee, valid_from, expires_at} — the canonical JSON to sign, its
//	                                             challenge (SHA-256, base64url) and the passkeys that may sign it
//	POST /v1/money/permission-mandates          {"canonical", "assertion"} — keeps the signed mandate (201)
//	GET  /v1/money/permission-mandates          — the workspace's mandates; an agent's key sees its own and general ones
//	POST /v1/money/permission-mandates/{id}/revoke   — revoked at once
//	POST /v1/money/permission-mandates/{id}/evaluate {client, payee_id, currency, amount_minor, …} — allow,
//	                                             needs_approval or block, with the reason and the rule; an agent's key
//	                                             is judged as itself ("key:<its id>"), whatever it names
//
// Preparing, signing and revoking are the owner's or an admin's. Mounted in the authed group.

type permissionMandateBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	PreparePermissionMandate(ctx context.Context, workspaceID string, t economy.PermissionMandateTerms) (canonical, challenge string, credentialIDs []string, err error)
	CreatePermissionMandate(ctx context.Context, workspaceID, by, canonical string, a *passkey.Assertion) (economy.PermissionMandate, error)
	PermissionMandates(ctx context.Context, workspaceID, agentID string) ([]economy.PermissionMandate, error)
	RevokePermissionMandate(ctx context.Context, workspaceID, id, by string) (economy.PermissionMandate, error)
	EvaluateMandatePayment(ctx context.Context, workspaceID, id string, p economy.PreparedPayment) (economy.MandateDecision, error)
}

func mountPermissionMandateRoutes(r chi.Router, bank permissionMandateBank) {
	// caller is the key's workspace and either the owner (agentID "") or the agent whose key it is.
	caller := func(w http.ResponseWriter, req *http.Request, ownerOnly bool) (ws, agentID, keyID, by string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "permission mandates are a workspace's: use a workspace's key")
			return "", "", "", "", false
		}
		if who, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
			return ws, "", "", who, true
		}
		if actx := auth.GetAuthContext(req.Context()); !ownerOnly && actx != nil && actx.APIKeyID != "" {
			agentID, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
			keyID = actx.APIKeyID
		}
		if agentID == "" {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may prepare, sign or revoke its permission mandates")
			return "", "", "", "", false
		}
		return ws, agentID, keyID, "agent:" + agentID, true
	}
	refuse := func(w http.ResponseWriter, err error) bool {
		switch {
		case errors.Is(err, economy.ErrPermissionMandate), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPermissionMandateNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			return false
		}
		return true
	}
	r.Post("/v1/money/permission-mandates/prepare", func(w http.ResponseWriter, req *http.Request) {
		ws, _, _, _, ok := caller(w, req, true)
		if !ok {
			return
		}
		var t economy.PermissionMandateTerms
		if err := json.NewDecoder(req.Body).Decode(&t); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be the mandate's terms: {"principal", "clients", "scope", "limits", "new_payee", "expires_at", …}`)
			return
		}
		if t.Principal.ID == "" {
			if t.Principal.Type == "company" {
				t.Principal.ID = ws
			} else if actx := auth.GetAuthContext(req.Context()); actx != nil {
				t.Principal.ID = actx.UserID
			}
		}
		canonical, challenge, creds, err := bank.PreparePermissionMandate(req.Context(), ws, t)
		if refuse(w, err) {
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"canonical": canonical, "challenge": challenge, "allow_credentials": creds,
			"user_verification": "required"})
	})
	r.Post("/v1/money/permission-mandates", func(w http.ResponseWriter, req *http.Request) {
		ws, _, _, by, ok := caller(w, req, true)
		if !ok {
			return
		}
		var in struct {
			Canonical string             `json:"canonical"`
			Assertion *passkey.Assertion `json:"assertion"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Canonical == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"canonical": "<the JSON prepare returned>", "assertion": {credential_id, client_data_json, authenticator_data, signature}}`)
			return
		}
		m, err := bank.CreatePermissionMandate(req.Context(), ws, by, in.Canonical, in.Assertion)
		if refuse(w, err) {
			return
		}
		writeJSONOK(w, http.StatusCreated, m)
	})
	r.Get("/v1/money/permission-mandates", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, _, _, ok := caller(w, req, false)
		if !ok {
			return
		}
		list, err := bank.PermissionMandates(req.Context(), ws, agentID)
		if refuse(w, err) {
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"mandates": list})
	})
	r.Post("/v1/money/permission-mandates/{id}/revoke", func(w http.ResponseWriter, req *http.Request) {
		ws, _, _, by, ok := caller(w, req, true)
		if !ok {
			return
		}
		m, err := bank.RevokePermissionMandate(req.Context(), ws, chi.URLParam(req, "id"), by)
		if refuse(w, err) {
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	})
	r.Post("/v1/money/permission-mandates/{id}/evaluate", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, keyID, _, ok := caller(w, req, false)
		if !ok {
			return
		}
		var p economy.PreparedPayment
		if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be the prepared payment: {"client", "payee_id", "currency", "amount_minor", …}`)
			return
		}
		if agentID != "" { // an agent's key is judged as itself, never as the client it names
			p.Client, p.AgentID = "key:"+keyID, agentID
		}
		d, err := bank.EvaluateMandatePayment(req.Context(), ws, chi.URLParam(req, "id"), p)
		if refuse(w, err) {
			return
		}
		writeJSONOK(w, http.StatusOK, d)
	})
}
