package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.19 — mandates: a payee may pull money within limits the owner set (economy/money_mandates.go).
//
//	POST /v1/money/mandates              {grant: {account_id, payee_workspace_id | payee_business_id + payee_id,
//	                                     payee_name, max_per_pull_minor, max_per_month_minor, expires_at}, assertion}, or
//	                                     the grant on its own — an agent's account grants a payee a mandate; an outside
//	                                     business's pull_key is answered once, and what it pulls is paid out to its saved
//	                                     payee (B30.129). B55.24: an agent's own key only asks: the mandate is requested
//	                                     and pulls nothing until the owner approves it. The owner's or an admin's grant,
//	                                     once the workspace has a passkey or device key, carries assertion, that key's
//	                                     signature over the challenge below for exactly this grant; without it, 403.
//	POST /v1/money/mandates/challenge    {grant} or {mandate_id} — B55.24, the owner or an admin: {challenge,
//	                                     allow_credentials}, the challenge a grant or an approval is signed over
//	GET  /v1/money/mandates              ?status= — the mandates granted and, for the owner, those received from other
//	                                     companies; with status (requested, active, …) only the granted ones in it
//	POST /v1/money/mandates/{id}/approve {assertion} — B55.24, the owner or an admin approves an agent's request, signed
//	POST /v1/money/mandates/{id}/decline the owner or an admin declines it: it never pulls
//	POST /v1/money/mandates/{id}/revoke  the owner or the agent revokes it, for good
//	POST /v1/money/mandates/{id}/pull    {amount_minor, idempotency_key, funding, memo} — the payee pulls within it: a
//	                                     Talyvor company with its own key, an outside business with X-Talyvor-Mandate-Key
//
// The workspace is the key's. Its owner or an admin grants, reads and revokes any of its agents' mandates; an agent's
// own key only its own. The first three are mounted in the authed group; the pull route by mountMandatePullRoute.

type moneyMandateBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	GrantMandate(ctx context.Context, workspaceID, agentID string, g economy.MandateGrant) (economy.Mandate, error)
	GrantMandateSigned(ctx context.Context, workspaceID, by string, g economy.MandateGrant, a *passkey.Assertion) (economy.Mandate, error)
	MandateGrantChallenge(ctx context.Context, workspaceID string, g economy.MandateGrant) (string, []string, error)
	MandateApprovalChallenge(ctx context.Context, workspaceID, mandateID string) (string, []string, error)
	ApproveMandate(ctx context.Context, workspaceID, mandateID, by string, a *passkey.Assertion) (economy.Mandate, error)
	DeclineMandate(ctx context.Context, workspaceID, mandateID, by string) (economy.Mandate, error)
	Mandates(ctx context.Context, workspaceID, agentID string) (granted, received []economy.Mandate, err error)
	RevokeMandate(ctx context.Context, workspaceID, agentID, mandateID, by string) (economy.Mandate, error)
	PullMandate(ctx context.Context, mandateID string, payee economy.MandatePayee, amountMinor int64, funding, idempotencyKey, memo string) (economy.MandatePull, error)
}

func mountMandateRoutes(r chi.Router, bank moneyMandateBank) {
	// caller is the key's workspace, and the agent its key is, or "" for the owner or an admin.
	caller := func(w http.ResponseWriter, req *http.Request) (ws, agentID, by string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "mandates are a workspace's: use a workspace's key")
			return "", "", "", false
		}
		if who, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
			return ws, "", who, true
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			agentID, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
		}
		if agentID == "" {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or the agent's own key may use its mandates")
			return "", "", "", false
		}
		return ws, agentID, "agent:" + agentID, true
	}
	// ownerOnly is caller for what only the owner or an admin does: decide, and sign.
	ownerOnly := func(w http.ResponseWriter, req *http.Request) (ws, by string, ok bool) {
		ws, agentID, by, ok := caller(w, req)
		if ok && agentID != "" {
			writeJSONErr(w, http.StatusForbidden, "an agent's own key asks for a mandate: only the workspace's owner or an admin approves, declines or signs one")
			return "", "", false
		}
		return ws, by, ok
	}
	// The grant comes as {"grant": …} beside its assertion, or on its own as it did before B55.24.
	type grantBody struct {
		economy.MandateGrant
		Grant     *economy.MandateGrant `json:"grant"`
		MandateID string                `json:"mandate_id"`
		Assertion *passkey.Assertion    `json:"assertion"`
	}
	grantOf := func(in grantBody) economy.MandateGrant {
		if in.Grant != nil {
			return *in.Grant
		}
		return in.MandateGrant
	}
	const grantShape = `body must be {"grant": {"account_id", "payee_workspace_id" or "payee_business_id" with "payee_id", "payee_name", "max_per_pull_minor", "max_per_month_minor", "expires_at": RFC 3339}, "assertion"}`
	r.Post("/v1/money/mandates", func(w http.ResponseWriter, req *http.Request) {
		var in grantBody
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, grantShape)
			return
		}
		ws, agentID, by, ok := caller(w, req)
		if !ok {
			return
		}
		var m economy.Mandate
		var err error
		if agentID == "" { // a person grants it, signed (B55.24); an agent's own key only asks
			m, err = bank.GrantMandateSigned(req.Context(), ws, by, grantOf(in), in.Assertion)
		} else {
			m, err = bank.GrantMandate(req.Context(), ws, agentID, grantOf(in))
		}
		if err != nil {
			writeMandateErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, m)
	})
	r.Get("/v1/money/mandates", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, _, ok := caller(w, req)
		if !ok {
			return
		}
		granted, received, err := bank.Mandates(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if status := req.URL.Query().Get("status"); status != "" {
			granted = slices.DeleteFunc(granted, func(m economy.Mandate) bool { return m.Status != status })
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"granted": granted, "received": received})
	})
	r.Post("/v1/money/mandates/challenge", func(w http.ResponseWriter, req *http.Request) {
		var in grantBody
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"grant": the grant, as it will be sent} or {"mandate_id": the request to approve}`)
			return
		}
		ws, _, ok := ownerOnly(w, req)
		if !ok {
			return
		}
		var c string
		var creds []string
		var err error
		if in.MandateID != "" {
			c, creds, err = bank.MandateApprovalChallenge(req.Context(), ws, in.MandateID)
		} else {
			c, creds, err = bank.MandateGrantChallenge(req.Context(), ws, grantOf(in))
		}
		if err != nil {
			writeMandateErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	})
	r.Post("/v1/money/mandates/{id}/approve", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Assertion *passkey.Assertion `json:"assertion"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"assertion"}: the signature over POST /v1/money/mandates/challenge {"mandate_id"}`)
			return
		}
		ws, by, ok := ownerOnly(w, req)
		if !ok {
			return
		}
		m, err := bank.ApproveMandate(req.Context(), ws, chi.URLParam(req, "id"), by, in.Assertion)
		if err != nil {
			writeMandateErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	})
	r.Post("/v1/money/mandates/{id}/decline", func(w http.ResponseWriter, req *http.Request) {
		ws, by, ok := ownerOnly(w, req)
		if !ok {
			return
		}
		m, err := bank.DeclineMandate(req.Context(), ws, chi.URLParam(req, "id"), by)
		if err != nil {
			writeMandateErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	})
	r.Post("/v1/money/mandates/{id}/revoke", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, by, ok := caller(w, req)
		if !ok {
			return
		}
		m, err := bank.RevokeMandate(req.Context(), ws, agentID, chi.URLParam(req, "id"), by)
		if err != nil {
			writeMandateErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	})
}

// mountMandatePullRoute mounts POST /v1/money/mandates/{id}/pull on r, outside the authed group: an outside business
// has no Talyvor key and pulls with its mandate's key, while a Talyvor company's key goes through authed — the authed
// group's own middleware — first.
func mountMandatePullRoute(r chi.Router, bank moneyMandateBank, authed func(http.Handler) http.Handler) {
	pull := func(w http.ResponseWriter, req *http.Request, payee economy.MandatePayee) {
		var in struct {
			AmountMinor    int64  `json:"amount_minor"`
			IdempotencyKey string `json:"idempotency_key"`
			Funding        string `json:"funding"`
			Memo           string `json:"memo"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_minor", "idempotency_key", "funding": "test|live", "memo"}`)
			return
		}
		p, err := bank.PullMandate(req.Context(), chi.URLParam(req, "id"), payee, in.AmountMinor, in.Funding, in.IdempotencyKey, in.Memo)
		if err != nil {
			writeMandateErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, p)
	}
	byCompany := authed(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "a Talyvor company pulls with its workspace's key")
			return
		}
		pull(w, req, economy.MandatePayee{WorkspaceID: ws})
	}))
	r.Post("/v1/money/mandates/{id}/pull", func(w http.ResponseWriter, req *http.Request) {
		if key := req.Header.Get("X-Talyvor-Mandate-Key"); key != "" {
			pull(w, req, economy.MandatePayee{Key: key})
			return
		}
		byCompany.ServeHTTP(w, req)
	})
}

func writeMandateErr(w http.ResponseWriter, err error) {
	var refusal *economy.CapabilityRefusal
	switch {
	case errors.Is(err, economy.ErrMandate):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrMandateNotFound), errors.Is(err, economy.ErrMoneyAccountNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrMandateOverLimit), errors.As(err, &refusal), errors.Is(err, economy.ErrAgentRule),
		errors.Is(err, economy.ErrMandateUnsigned), errors.Is(err, passkey.ErrInvalid):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, economy.ErrMandateRevoked), errors.Is(err, economy.ErrMandateExpired), errors.Is(err, economy.ErrMandateShort),
		errors.Is(err, economy.ErrMandatePayerPaused), errors.Is(err, economy.ErrMandateWorkspacePaused), errors.Is(err, economy.ErrMandatePayeeAccount),
		errors.Is(err, economy.ErrMoneyAccountNotOpen), errors.Is(err, economy.ErrIdempotencyKeyReused),
		errors.Is(err, economy.ErrMandateRequested), errors.Is(err, economy.ErrMandateDeclined):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default: // an outside business's payee and its payment out answer as a payment out does
		writePaymentErr(w, err)
	}
}
