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
	"github.com/talyvor/lens/internal/workspace"
)

// B22.3 — send and request money between any agents on Talyvor (economy/agent_transfers.go).
//
//	PUT  /v1/workspaces/{wsID}/agents/{agentID}/handle      {handle} — the agent's address besides its wallet ID
//	GET  /v1/wallets/{address}                              who a wallet ID or @handle is, before sending to it
//	POST /v1/workspaces/{wsID}/agents/{agentID}/send        {to, amount_ulxc, memo} — to any agent on Talyvor
//	POST /v1/workspaces/{wsID}/agents/{agentID}/requests    {from, amount_ulxc, memo} — ask an agent for credits
//	GET  /v1/workspaces/{wsID}/money-requests               the requests its agents made and were made
//	POST /v1/workspaces/{wsID}/money-requests/{id}/accept   pay a request made of one of its agents
//	POST /v1/workspaces/{wsID}/money-requests/{id}/decline
//	POST /v1/workspaces/{wsID}/transfers/{id}/refund        give a transfer one of its agents received back
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/transfers   what the agent sent and received
//
// Sending and asking take the agent's own key, the workspace's owner or an admin; answering, giving back and
// the handle take the owner or an admin. A recurring transfer is a schedule (POST …/agents/{id}/schedules)
// whose payee is any agent. Mounted in the authed group.

type agentTransferBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	SetAgentHandle(ctx context.Context, workspaceID, agentID, handle string) (economy.WalletAddress, error)
	ResolveWallet(ctx context.Context, address string) (economy.WalletAddress, error)
	SendCredits(ctx context.Context, workspaceID, fromAgentID, address string, amount int64, memo string) (economy.AgentTransfer, error)
	RequestCredits(ctx context.Context, workspaceID, agentID, address string, amount int64, memo string) (economy.MoneyRequest, error)
	AnswerMoneyRequest(ctx context.Context, workspaceID, requestID string, accept bool) (economy.MoneyRequest, error)
	ListMoneyRequests(ctx context.Context, workspaceID string) ([]economy.MoneyRequest, error)
	RefundTransfer(ctx context.Context, workspaceID, transferID string) (economy.AgentTransfer, error)
	ListAgentTransfers(ctx context.Context, workspaceID, agentID string) ([]economy.AgentTransfer, error)
}

func mountAgentTransferRoutes(r chi.Router, bank agentTransferBank) {
	// agentOrOwner lets the agent's own key, the workspace's owner or an admin move the agent's money.
	agentOrOwner := func(w http.ResponseWriter, req *http.Request, agentID string) bool {
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
			return true
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			if keyAgent, _, _ := bank.AgentOfKey(req.Context(), actx.APIKeyID); keyAgent == agentID {
				return true
			}
		}
		writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may move its money")
		return false
	}
	type move struct {
		To         string `json:"to"`
		From       string `json:"from"`
		AmountULXC int64  `json:"amount_ulxc"`
		Memo       string `json:"memo"`
	}

	r.Put("/v1/workspaces/{wsID}/agents/{agentID}/handle", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Handle string `json:"handle"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"handle": "<3–32 of a–z, 0–9, . _ ->"}`)
			return
		}
		a, err := bank.SetAgentHandle(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.Handle)
		if err != nil {
			writeTransferErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, a)
	}))
	r.Get("/v1/wallets/{address}", func(w http.ResponseWriter, req *http.Request) {
		a, err := bank.ResolveWallet(req.Context(), chi.URLParam(req, "address"))
		if err != nil {
			writeTransferErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, a)
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/send", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if !agentOrOwner(w, req, agentID) {
			return
		}
		var in move
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.To == "" || in.AmountULXC <= 0 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to": "<wallet id or @handle>", "amount_ulxc": <positive µLXC>, "memo": "<optional>"}`)
			return
		}
		// B50.1: a send retried with the same Idempotency-Key lands once.
		key := req.Header.Get("Idempotency-Key")
		if len(key) > 128 {
			writeJSONErr(w, http.StatusBadRequest, "the Idempotency-Key must be at most 128 characters")
			return
		}
		t, err := bank.SendCredits(economy.WithMoveKey(req.Context(), key), chi.URLParam(req, "wsID"), agentID, in.To, in.AmountULXC, in.Memo)
		if err != nil {
			writeTransferErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/requests", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if !agentOrOwner(w, req, agentID) {
			return
		}
		var in move
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.From == "" || in.AmountULXC <= 0 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"from": "<wallet id or @handle>", "amount_ulxc": <positive µLXC>, "memo": "<optional>"}`)
			return
		}
		m, err := bank.RequestCredits(req.Context(), chi.URLParam(req, "wsID"), agentID, in.From, in.AmountULXC, in.Memo)
		if err != nil {
			writeTransferErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, m)
	})
	r.Get("/v1/workspaces/{wsID}/money-requests", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListMoneyRequests(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"requests": list})
	})
	for _, answer := range []string{"accept", "decline"} {
		accept := answer == "accept"
		r.Post("/v1/workspaces/{wsID}/money-requests/{requestID}/"+answer, ownerOnly(func(w http.ResponseWriter, req *http.Request) {
			m, err := bank.AnswerMoneyRequest(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "requestID"), accept)
			if err != nil {
				writeTransferErr(w, err)
				return
			}
			writeJSONOK(w, http.StatusOK, m)
		}))
	}
	r.Post("/v1/workspaces/{wsID}/transfers/{transferID}/refund", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		t, err := bank.RefundTransfer(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "transferID"))
		if err != nil {
			writeTransferErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/transfers", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListAgentTransfers(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"transfers": list})
	})
}

func writeTransferErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrRequestNotFound), errors.Is(err, economy.ErrTransferNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrSameAgent), errors.Is(err, economy.ErrHandle):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrIdempotencyKeyReused):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrAgentFunds), errors.Is(err, economy.ErrAgentOwnerless), errors.Is(err, economy.ErrOwnerUnverified),
		errors.Is(err, economy.ErrAlreadyRefunded):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrAgentRule), errors.Is(err, economy.ErrApprovalRequired), errors.Is(err, economy.ErrCapabilityNotCleared),
		errors.Is(err, workspace.ErrMoneyWall):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
