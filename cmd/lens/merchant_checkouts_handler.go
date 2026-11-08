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

// B30.38 — Talyvor Pay: a business accepts payments from any Talyvor agent (economy/merchant_checkouts.go).
//
//	POST /v1/merchant/checkouts             {amount_minor, currency, order_id, return_url} — the company opens a checkout
//	GET  /v1/merchant/checkouts             the company's checkouts: open, paid or refunded
//	POST /v1/merchant/checkouts/{id}/refund the company refunds a paid one in full
//	POST /v1/merchant/checkouts/{id}/pay    {agent_id, funding} — a Talyvor agent pays it from its account in the currency
//
// The workspace is the key's. A merchant's routes are its owner's or an admin's; paying is the owner's for any of its
// agents, or an agent's own key for itself (MCP: wallet_pay_merchant). Mounted in the authed group.

type merchantCheckoutBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	CreateCheckout(ctx context.Context, workspaceID string, r economy.CheckoutRequest) (economy.Checkout, error)
	Checkouts(ctx context.Context, workspaceID string) ([]economy.Checkout, error)
	RefundCheckout(ctx context.Context, workspaceID, id string) (economy.PaidCheckout, error)
	PayCheckout(ctx context.Context, id, workspaceID, agentID, funding string) (economy.PaidCheckout, error)
}

func mountMerchantCheckoutRoutes(r chi.Router, bank merchantCheckoutBank) {
	merchant := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "checkouts are a workspace's: use a workspace's key")
			return "", false
		}
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may use its checkouts")
			return "", false
		}
		return ws, true
	}
	r.Post("/v1/merchant/checkouts", func(w http.ResponseWriter, req *http.Request) {
		var in economy.CheckoutRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_minor", "currency", "order_id", "return_url"}`)
			return
		}
		ws, ok := merchant(w, req)
		if !ok {
			return
		}
		c, err := bank.CreateCheckout(req.Context(), ws, in)
		if err != nil {
			writeCheckoutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, c)
	})
	r.Get("/v1/merchant/checkouts", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := merchant(w, req)
		if !ok {
			return
		}
		list, err := bank.Checkouts(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"checkouts": list})
	})
	r.Post("/v1/merchant/checkouts/{id}/refund", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := merchant(w, req)
		if !ok {
			return
		}
		out, err := bank.RefundCheckout(req.Context(), ws, chi.URLParam(req, "id"))
		if err != nil {
			writeCheckoutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, out)
	})
	r.Post("/v1/merchant/checkouts/{id}/pay", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			AgentID string `json:"agent_id"`
			Funding string `json:"funding"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"agent_id": "<the paying agent; an agent's own key names none>", "funding": "test|live"}`)
			return
		}
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "an agent pays from its workspace: use a workspace's key")
			return
		}
		agentID := in.AgentID
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			keyAgent := ""
			if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
				keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
			}
			if keyAgent == "" || (agentID != "" && agentID != keyAgent) {
				writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or the agent's own key may pay for the agent")
				return
			}
			agentID = keyAgent
		}
		if agentID == "" {
			writeJSONErr(w, http.StatusBadRequest, "agent_id names the agent that pays")
			return
		}
		out, err := bank.PayCheckout(req.Context(), chi.URLParam(req, "id"), ws, agentID, in.Funding)
		if err != nil {
			writeCheckoutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, out)
	})
}

func writeCheckoutErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrCheckout):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrCheckoutNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrCheckoutState), errors.Is(err, economy.ErrCheckoutShort):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeInvoiceErr(w, err)
	}
}
