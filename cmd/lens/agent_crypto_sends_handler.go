package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/smartaccount"
)

// B48.9 — an agent sends USDC from its smart account to another Talyvor agent or to any address
// (economy/agent_crypto_sends.go).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/quote       {to, network, token, amount_minor} — before sending
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends             the same, with an Idempotency-Key: sends it once
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends             what the agent sent and was sent
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{id}        one send, its receipt read again
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{id}/approve  {test_amount_minor?} — a send to a new address
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{id}/decline
//
// Quoting, sending and reading take the agent's own key or the workspace's owner; only the owner, signed in, says yes
// or no to a send to a new address, and only their own send there goes without asking. Mounted in the authed group.

type agentCryptoSendBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	QuoteAgentCryptoSend(ctx context.Context, workspaceID, agentID string, in economy.CryptoSendRequest) (economy.CryptoSendQuote, error)
	SendAgentCrypto(ctx context.Context, workspaceID, agentID, by string, byOwner bool, key string, in economy.CryptoSendRequest) (economy.CryptoSend, error)
	AgentCryptoSends(ctx context.Context, workspaceID, agentID string) ([]economy.CryptoSend, error)
	AgentCryptoSend(ctx context.Context, workspaceID, agentID, id string) (economy.CryptoSend, error)
	AnswerAgentCryptoSend(ctx context.Context, workspaceID, agentID, id, by string, approve bool, testMinor int64) (economy.CryptoSend, error)
}

// signedInOwner is the workspace owner's signed-in session, as keyOwnerOnly admits it: never a key, admin-scoped or not.
func signedInOwner(req *http.Request) (string, bool) {
	actx := auth.GetAuthContext(req.Context())
	if actx == nil || actx.AuthMethod == auth.MethodWorkspaceKey || actx.AuthMethod == auth.MethodSessionKey || !actx.HasScope(auth.ScopeKeys) {
		return "", false
	}
	return "user:" + actx.UserID, true
}

func mountAgentCryptoSendRoutes(r chi.Router, bank agentCryptoSendBank) {
	// caller is who may move the agent's money: its owner, signed in, or the agent's own key.
	caller := func(w http.ResponseWriter, req *http.Request, agentID string) (by string, owner, ok bool) {
		if by, owner := signedInOwner(req); owner {
			return by, true, true
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			if keyAgent, _, _ := bank.AgentOfKey(req.Context(), actx.APIKeyID); keyAgent == agentID {
				return "agent:" + agentID, false, true
			}
		}
		writeJSONErr(w, http.StatusForbidden, "only the agent's own key or the workspace's owner may send its money")
		return "", false, false
	}
	body := func(w http.ResponseWriter, req *http.Request) (economy.CryptoSendRequest, bool) {
		var in economy.CryptoSendRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to": "0x…", "network": "<network>", "token": "USDC", "amount_minor": <positive>}`)
			return in, false
		}
		return in, true
	}

	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/quote", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if _, _, ok := caller(w, req, agentID); !ok {
			return
		}
		in, ok := body(w, req)
		if !ok {
			return
		}
		q, err := bank.QuoteAgentCryptoSend(req.Context(), chi.URLParam(req, "wsID"), agentID, in)
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, q)
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		by, owner, ok := caller(w, req, agentID)
		if !ok {
			return
		}
		in, ok := body(w, req)
		if !ok {
			return
		}
		c, err := bank.SendAgentCrypto(req.Context(), chi.URLParam(req, "wsID"), agentID, by, owner, req.Header.Get("Idempotency-Key"), in)
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeCryptoSend(w, c)
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if _, _, ok := caller(w, req, agentID); !ok {
			return
		}
		list, err := bank.AgentCryptoSends(req.Context(), chi.URLParam(req, "wsID"), agentID)
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"sends": list})
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{sendID}", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if _, _, ok := caller(w, req, agentID); !ok {
			return
		}
		c, err := bank.AgentCryptoSend(req.Context(), chi.URLParam(req, "wsID"), agentID, chi.URLParam(req, "sendID"))
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeCryptoSend(w, c)
	})
	for _, answer := range []string{"approve", "decline"} {
		approve := answer == "approve"
		r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{sendID}/"+answer, func(w http.ResponseWriter, req *http.Request) {
			by, ok := signedInOwner(req)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, signed in, answers a send to a new address")
				return
			}
			var in struct {
				TestAmountMinor int64 `json:"test_amount_minor"`
			}
			if req.ContentLength != 0 {
				if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<10)).Decode(&in); err != nil || in.TestAmountMinor < 0 {
					writeJSONErr(w, http.StatusBadRequest, `body is empty, or {"test_amount_minor": <positive, less than the send>} to send that much first`)
					return
				}
			}
			c, err := bank.AnswerAgentCryptoSend(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"),
				chi.URLParam(req, "sendID"), by, approve, in.TestAmountMinor)
			if err != nil {
				writeCryptoSendErr(w, err)
				return
			}
			writeCryptoSend(w, c)
		})
	}
}

// writeCryptoSend answers a send with what its status says: done, on its way, refused or failed.
func writeCryptoSend(w http.ResponseWriter, c economy.CryptoSend) {
	code := http.StatusAccepted // waiting for the owner, or on its way
	switch c.Status {
	case "landed":
		code = http.StatusOK
	case "refused":
		code = http.StatusForbidden
	case "failed":
		code = http.StatusBadGateway
	}
	writeJSONOK(w, code, c)
}

func writeCryptoSendErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrCryptoSendsOff), errors.Is(err, economy.ErrSmartAccountsOff):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, economy.ErrCryptoSendNotFound), errors.Is(err, economy.ErrSmartAccountNotFound), errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrCryptoSend), errors.Is(err, smartaccount.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrIdempotencyKeyReused):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, smartaccount.ErrRefused):
		writeJSONErr(w, http.StatusBadGateway, "the network did not take it: try again under the same Idempotency-Key")
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
