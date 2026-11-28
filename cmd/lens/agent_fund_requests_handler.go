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

// agent_fund_requests_handler.go — B28.73: the owner's SDK asks to fund an agent; a person funds it from the link.
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/fund-requests   {"amount_ulxc"}, Idempotency-Key required: files the
//	                                                            request and answers its approval_url (201; the same
//	                                                            key and body again: 200). Nothing moves.
//	GET  /v1/workspaces/{wsID}/agents/fund-requests/{id}        the request, as it stands
//	POST /v1/workspaces/{wsID}/agents/fund-requests/{id}/approve  a person signed in — never a key — funds the agent:
//	                                                            one posting, its ref the request's key
//	POST /v1/workspaces/{wsID}/agents/fund-requests/{id}/decline  a person signed in declines it: nothing moves
//
// Filing and reading are the owner's, an admin's or Finance's, as /fund is; deciding is theirs signed in, so a key that
// files a request cannot also approve it.

type fundRequestBank interface {
	RequestAgentFunding(ctx context.Context, workspaceID, agentID string, amount int64, idempotencyKey, by string) (economy.AgentFundRequest, bool, error)
	AgentFundRequestByID(ctx context.Context, workspaceID, id string) (economy.AgentFundRequest, error)
	ApproveAgentFundRequest(ctx context.Context, workspaceID, id, by string) (economy.AgentFundRequest, error)
	DeclineAgentFundRequest(ctx context.Context, workspaceID, id, by string) (economy.AgentFundRequest, error)
}

// signedInOrRole is ownerOrRole for a person signed in only: no workspace key, admin-scoped or not, and no operator key.
func signedInOrRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return ownerOrRole(func(w http.ResponseWriter, req *http.Request) {
		if actx := auth.GetAuthContext(req.Context()); actx == nil || actx.AuthMethod != auth.MethodJWT || actx.UserID == "" {
			writeJSONErr(w, http.StatusForbidden, "a person signed in to the workspace decides a fund request, from its approval link: a key cannot")
			return
		}
		next(w, req)
	}, roles...)
}

func writeFundRequest(w http.ResponseWriter, code int, r economy.AgentFundRequest, err error) {
	switch {
	case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrFundRequestNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrFundRequest):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrFundRequestConflict), errors.Is(err, economy.ErrFundRequestDecided), errors.Is(err, economy.ErrAgentFunds),
		errors.Is(err, economy.ErrAgentOwnerless), errors.Is(err, economy.ErrAgentArchived), errors.Is(err, economy.ErrIdempotencyKeyReused):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSONOK(w, code, r)
	}
}

func mountAgentFundRequestRoutes(r chi.Router, bank fundRequestBank) {
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/fund-requests", ownerOrRole(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			AmountULXC int64 `json:"amount_ulxc"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.AmountULXC <= 0 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_ulxc": <positive µLXC>}`)
			return
		}
		by, _ := storedanswers.OwnerOrRole(req.Context(), moneyRoles...)
		out, again, err := bank.RequestAgentFunding(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.AmountULXC,
			req.Header.Get("Idempotency-Key"), by)
		code := http.StatusCreated
		if again {
			code = http.StatusOK
		}
		writeFundRequest(w, code, out, err)
	}, moneyRoles...))
	r.Get("/v1/workspaces/{wsID}/agents/fund-requests/{requestID}", ownerOrRole(func(w http.ResponseWriter, req *http.Request) {
		out, err := bank.AgentFundRequestByID(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "requestID"))
		writeFundRequest(w, http.StatusOK, out, err)
	}, moneyRoles...))
	decide := func(fn func(ctx context.Context, workspaceID, id, by string) (economy.AgentFundRequest, error)) http.HandlerFunc {
		return signedInOrRole(func(w http.ResponseWriter, req *http.Request) {
			by, _ := storedanswers.OwnerOrRole(req.Context(), moneyRoles...)
			out, err := fn(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "requestID"), by)
			writeFundRequest(w, http.StatusOK, out, err)
		}, moneyRoles...)
	}
	r.Post("/v1/workspaces/{wsID}/agents/fund-requests/{requestID}/approve", decide(bank.ApproveAgentFundRequest))
	r.Post("/v1/workspaces/{wsID}/agents/fund-requests/{requestID}/decline", decide(bank.DeclineAgentFundRequest))
}
