package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.17 — send money to people and companies outside Talyvor (economy/money_payments.go).
//
//	POST /v1/money/payments   {payee_id, amount_minor, currency, reference, account_id, funding, idempotency_key}, or the
//	                          key as the Idempotency-Key header — pays a saved payee from the company's account in the
//	                          currency, or account_id; an agent's key pays from its own account, judged by its rules. An
//	                          approval it needs is filed and the payment refused (403) until the owner approves it.
//	GET  /v1/money/payments   ?agent_id= — the payments out, newest first
//	GET  /v1/money/payments/{id} — one payment, after asking the partner where it stands now
//
// The workspace is the key's. Its owner or an admin pays from and reads any of its accounts; an agent's own key only
// its own. Mounted in the authed group.

type moneyPaymentBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	PayOutside(ctx context.Context, workspaceID string, in economy.OutsidePayment) (economy.OutsidePayment, error)
	OutsidePayments(ctx context.Context, workspaceID, agentID string) ([]economy.OutsidePayment, error)
	RefreshOutsidePayment(ctx context.Context, workspaceID, agentID, id string) (economy.OutsidePayment, error)
}

// moneyCaller is the key's workspace and the agent it may act for: "" for the owner or an admin, who may act for any.
// agentOf is bank.AgentOfKey. It writes the refusal and answers ok false when the key may not make payments.
func moneyCaller(w http.ResponseWriter, req *http.Request, agentOf func(context.Context, string) (string, string, error)) (ws, agentID string, ok bool) {
	ws, _ = auth.WorkspaceIdentity(req.Context())
	if ws == "" {
		writeJSONErr(w, http.StatusBadRequest, "payments are a workspace's: use a workspace's key")
		return "", "", false
	}
	if _, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
		return ws, "", true
	}
	if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
		agentID, _, _ = agentOf(req.Context(), actx.APIKeyID)
	}
	if agentID == "" {
		writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or an agent's own key may make payments")
		return "", "", false
	}
	return ws, agentID, true
}

func mountMoneyPaymentRoutes(r chi.Router, bank moneyPaymentBank) {
	caller := func(w http.ResponseWriter, req *http.Request) (ws, agentID string, ok bool) {
		return moneyCaller(w, req, bank.AgentOfKey)
	}
	r.Post("/v1/money/payments", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		var in economy.OutsidePayment
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"payee_id", "amount_minor", "currency", "reference", "idempotency_key"}`)
			return
		}
		if k := req.Header.Get("Idempotency-Key"); k != "" {
			in.IdempotencyKey = k
		}
		p, err := bank.PayOutside(req.Context(), ws, economy.OutsidePayment{AccountID: in.AccountID, AgentID: agentID, PayeeID: in.PayeeID,
			AmountMinor: in.AmountMinor, Currency: in.Currency, Funding: in.Funding, Reference: in.Reference, IdempotencyKey: in.IdempotencyKey})
		if err != nil {
			writePaymentErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, p)
	})
	r.Get("/v1/money/payments", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		if agentID == "" {
			agentID = req.URL.Query().Get("agent_id")
		}
		list, err := bank.OutsidePayments(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"payments": list})
	})
	r.Get("/v1/money/payments/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		p, err := bank.RefreshOutsidePayment(req.Context(), ws, agentID, chi.URLParam(req, "id"))
		if err != nil {
			writePaymentErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, p)
	})
}

func writePaymentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrOutsidePayment):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrOutsidePaymentNotFound), errors.Is(err, economy.ErrPayeeNotFound),
		errors.Is(err, economy.ErrMoneyAccountNotFound), errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrPayeeUnconfirmed), errors.Is(err, economy.ErrAgentFunds), errors.Is(err, economy.ErrIdempotencyKeyReused),
		errors.Is(err, economy.ErrMoneyAccountNotOpen):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrAgentRule), errors.Is(err, economy.ErrApprovalRequired), errors.Is(err, economy.ErrCapabilityNotCleared),
		errors.As(err, new(*screening.Refusal)):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, partners.ErrScreeningUnavailable), errors.Is(err, economy.ErrAccountNotOpened):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
