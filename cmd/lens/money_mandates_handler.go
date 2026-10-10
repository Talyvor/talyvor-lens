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

// B30.19 — mandates: a payee may pull money within limits the owner set (economy/money_mandates.go).
//
//	POST /v1/money/mandates              {account_id, payee_workspace_id | payee_business_id, payee_name,
//	                                     max_per_pull_minor, max_per_month_minor, expires_at} — an agent's account
//	                                     grants a payee a mandate; an outside business's pull_key is answered once
//	GET  /v1/money/mandates              the mandates granted and, for the owner, those received from other companies
//	POST /v1/money/mandates/{id}/revoke  the owner or the agent revokes it, for good
//	POST /v1/money/mandates/{id}/pull    {amount_minor, idempotency_key, funding, memo} — the payee pulls within it: a
//	                                     Talyvor company with its own key, an outside business with X-Talyvor-Mandate-Key
//
// The workspace is the key's. Its owner or an admin grants, reads and revokes any of its agents' mandates; an agent's
// own key only its own. The first three are mounted in the authed group; the pull route by mountMandatePullRoute.

type moneyMandateBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	GrantMandate(ctx context.Context, workspaceID, agentID string, g economy.MandateGrant) (economy.Mandate, error)
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
	r.Post("/v1/money/mandates", func(w http.ResponseWriter, req *http.Request) {
		var g economy.MandateGrant
		if err := json.NewDecoder(req.Body).Decode(&g); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"account_id", "payee_workspace_id" or "payee_business_id", "payee_name", "max_per_pull_minor", "max_per_month_minor", "expires_at": RFC 3339}`)
			return
		}
		ws, agentID, _, ok := caller(w, req)
		if !ok {
			return
		}
		m, err := bank.GrantMandate(req.Context(), ws, agentID, g)
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
		writeJSONOK(w, http.StatusOK, map[string]any{"granted": granted, "received": received})
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
	case errors.Is(err, economy.ErrMandateOverLimit), errors.As(err, &refusal):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, economy.ErrMandateRevoked), errors.Is(err, economy.ErrMandateExpired), errors.Is(err, economy.ErrMandateShort),
		errors.Is(err, economy.ErrMandatePayerPaused), errors.Is(err, economy.ErrMandatePayeeAccount),
		errors.Is(err, economy.ErrMoneyAccountNotOpen), errors.Is(err, economy.ErrIdempotencyKeyReused):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
