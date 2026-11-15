package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/kyatap"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.5 — Know Your Agent: a signed credential every agent can show (internal/kya, docs/kya.md). Any platform checks
// one without an account:
//
//	GET  /.well-known/talyvor-kya/jwks.json      the Ed25519 keys credentials are signed with (RFC 8037 OKP JWKs)
//	GET  /.well-known/talyvor-kya/revoked.json   every credential revoked that has not yet expired, and why
//	POST /v1/kya/verify   {"credential"}         Talyvor's answer: valid, or why not, and what it says
//
// and the agent's owner sees it, as the agent does with its own key through the MCP tool wallet_credential:
//
//	GET  /v1/workspaces/{wsID}/agents/{id}/credential   the agent's credential, a new one when what it held is no
//	                                                    longer true; 409 while the agent is frozen or archived
//
// The first three are public, in the rate-limited group; the last is in the authed group, so {wsID} is bound to the
// caller's credential, and it takes the agent's own key, the workspace's owner or an admin: whoever holds a credential
// can show it.

type kyaService interface {
	Current(ctx context.Context, workspaceID, agentID string) (kya.Credential, error)
	Verify(ctx context.Context, token string) (kya.Verification, error)
	JWKS(ctx context.Context) (kya.JWKS, error)
	Revocations(ctx context.Context) (kya.Revocations, error)
}

func mountKYAPublicRoutes(r chi.Router, svc kyaService) {
	r.Get("/.well-known/talyvor-kya/jwks.json", func(w http.ResponseWriter, req *http.Request) {
		set, err := svc.JWKS(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		writeJSONOK(w, http.StatusOK, set)
	})
	r.Get("/.well-known/talyvor-kya/revoked.json", func(w http.ResponseWriter, req *http.Request) {
		list, err := svc.Revocations(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSONOK(w, http.StatusOK, list)
	})
	r.Post("/v1/kya/verify", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Credential string `json:"credential"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Credential) == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"credential": "<the agent's credential>"}`)
			return
		}
		v, err := svc.Verify(req.Context(), strings.TrimSpace(in.Credential))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, v)
	})
}

type agentKeyResolver interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
}

func mountKYAAgentRoutes(r chi.Router, svc kyaService, keys agentKeyResolver) {
	r.Get("/v1/workspaces/{wsID}/agents/{id}/credential", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "id")
		if !mayShowCredential(w, req, keys, agentID) {
			return
		}
		c, err := svc.Current(req.Context(), chi.URLParam(req, "wsID"), agentID)
		var standing *kya.StandingError
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.As(err, &standing):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, c)
		}
	})
}

// mayShowCredential answers whether the caller may be given agentID's credential, and refuses them when not: whoever
// holds a credential can show it, so only the agent itself and those who answer for it are given it.
func mayShowCredential(w http.ResponseWriter, req *http.Request, keys agentKeyResolver, agentID string) bool {
	if _, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
		return true
	}
	keyAgent := ""
	if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
		keyAgent, _, _ = keys.AgentOfKey(req.Context(), actx.APIKeyID)
	}
	if keyAgent != agentID {
		writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may read its credential")
		return false
	}
	return true
}

// B62.6 — the credential in the form Visa's Trusted Agent Protocol publishes (internal/kyatap, docs/kya.md):
//
//	POST /v1/kya/verify/tap   {"authority", "path", "signature_input", "signature", "agentic_consumer"}
//	                          Talyvor's answer on one: valid, or why not, its tag and what the credential says
//	POST /v1/workspaces/{wsID}/agents/{id}/credential/tap   {"authority", "path", "tag"}
//	                          the signature headers and Agentic Consumer Recognition Object for one request to a
//	                          merchant; 409 while the agent is frozen or archived
//
// The first is public, in the rate-limited group; the second in the authed group, for whoever may read the agent's
// credential.

type kyaTAPService interface {
	Present(ctx context.Context, workspaceID, agentID string, in kyatap.Request) (kyatap.Presentation, error)
	Verify(ctx context.Context, in kyatap.Check) (kyatap.Result, error)
}

func mountKYATAPPublicRoutes(r chi.Router, svc kyaTAPService) {
	r.Post("/v1/kya/verify/tap", func(w http.ResponseWriter, req *http.Request) {
		var in kyatap.Check
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil ||
			in.SignatureInput == "" || in.Signature == "" || in.AgenticConsumer.IDToken == "" {
			writeJSONErr(w, http.StatusBadRequest,
				`body must be {"authority", "path", "signature_input", "signature", "agentic_consumer": {"nonce", "idToken", "kid", "alg", "signature"}}`)
			return
		}
		res, err := svc.Verify(req.Context(), in)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, res)
	})
}

func mountKYATAPAgentRoutes(r chi.Router, svc kyaTAPService, keys agentKeyResolver) {
	r.Post("/v1/workspaces/{wsID}/agents/{id}/credential/tap", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "id")
		if !mayShowCredential(w, req, keys, agentID) {
			return
		}
		var in kyatap.Request
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"authority", "path", "tag"}`)
			return
		}
		p, err := svc.Present(req.Context(), chi.URLParam(req, "wsID"), agentID, in)
		var standing *kya.StandingError
		switch {
		case errors.Is(err, kyatap.ErrBadRequest):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.As(err, &standing):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, p)
		}
	})
}
