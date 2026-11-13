package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// crypto_requests_handler.go — B48.10: asking to be paid into an agent's crypto wallet (economy.CryptoRequest).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-requests                         {token, network, amount_minor, memo, expires_at, payer_address?}
//	                                                                                     with an Idempotency-Key; the agent's own key, the owner or an admin
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/crypto-requests                         its requests, newest first, each with where it stands
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/crypto-requests/{requestID}             one
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-requests/{requestID}/overpayment {decision: refund | keep}; the owner or an admin
//	GET  /v1/crypto-requests/{requestID}                                                PUBLIC: what the shared link shows
//
// Creating one needs the chain indexer (LENS_CHAIN_INDEX), which matches the deposits that pay it: 503 without. A
// payer that is another Talyvor agent is asked in its owner's approvals with the trust Talyvor has in the requester's
// workspace; a workspace names at most payer_requests_per_day payers a day (429 beyond).

func writeCryptoRequest(w http.ResponseWriter, status int, out any, err error) {
	switch {
	case errors.Is(err, economy.ErrCryptoRequest):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrCryptoRequestNotFound), errors.Is(err, economy.ErrSmartAccountNotFound), errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrCryptoRequestConflict):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrCryptoRequestLimit):
		writeJSONErr(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, economy.ErrCryptoRequestChain):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, "the request to be paid could not be read or kept")
	default:
		writeJSONOK(w, status, out)
	}
}

func mountCryptoRequestRoutes(r chi.Router, bank *economy.DualTokenStore, c *chainIndex) {
	agentOrOwner := func(w http.ResponseWriter, req *http.Request) bool {
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
			return true
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			agent, ws, _ := bank.AgentOfKey(req.Context(), actx.APIKeyID)
			if agent == chi.URLParam(req, "agentID") && ws == chi.URLParam(req, "wsID") {
				return true
			}
		}
		writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may ask for or read its payments")
		return false
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-requests", func(w http.ResponseWriter, req *http.Request) {
		if !agentOrOwner(w, req) {
			return
		}
		if c == nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "the chain indexer is not switched on")
			return
		}
		key := req.Header.Get("Idempotency-Key")
		if len(key) > 128 {
			writeJSONErr(w, http.StatusBadRequest, "the Idempotency-Key must be at most 128 characters")
			return
		}
		var in economy.CryptoRequestIn
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"token": "USDC", "network", "amount_minor", "memo", "expires_at", "payer_address"?}`)
			return
		}
		in.WorkspaceID, in.AgentID, in.IdempotencyKey = chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), key
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			in.CreatedBy = actx.UserID
			if in.CreatedBy == "" {
				in.CreatedBy = "key:" + actx.APIKeyID
			}
		}
		out, err := c.x.CreatePaymentRequest(req.Context(), in)
		writeCryptoRequest(w, http.StatusCreated, out, err)
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/crypto-requests", func(w http.ResponseWriter, req *http.Request) {
		if !agentOrOwner(w, req) {
			return
		}
		out, err := bank.AgentCryptoPaymentRequests(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		writeCryptoRequest(w, http.StatusOK, map[string]any{"requests": out}, err)
	})
	one := func(req *http.Request) (economy.CryptoRequest, error) {
		out, err := bank.CryptoPaymentRequest(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "requestID"))
		if err == nil && out.AgentID != chi.URLParam(req, "agentID") {
			err = economy.ErrCryptoRequestNotFound
		}
		return out, err
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/crypto-requests/{requestID}", func(w http.ResponseWriter, req *http.Request) {
		if !agentOrOwner(w, req) {
			return
		}
		out, err := one(req)
		writeCryptoRequest(w, http.StatusOK, out, err)
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-requests/{requestID}/overpayment", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Decision string `json:"decision"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"decision": "refund" | "keep"}`)
			return
		}
		if _, err := one(req); err != nil {
			writeCryptoRequest(w, 0, nil, err)
			return
		}
		by := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			by = actx.UserID
		}
		out, err := bank.DecideCryptoOverpayment(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "requestID"), in.Decision, by)
		writeCryptoRequest(w, http.StatusOK, out, err)
	}))
}

// serveCryptoRequest is GET /v1/crypto-requests/{requestID}: what the shared link shows, to anyone holding it.
func serveCryptoRequest(bank *economy.DualTokenStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		out, err := bank.PublicCryptoPaymentRequest(req.Context(), chi.URLParam(req, "requestID"))
		writeCryptoRequest(w, http.StatusOK, out, err)
	}
}
