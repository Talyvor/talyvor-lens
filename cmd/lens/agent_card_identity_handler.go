package main

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/a2acard"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B45.23 — every agent's identity card: an A2A v1.0 Agent Card, always signed, naming who answers for the agent
// (internal/a2acard, docs/agent-card.md). Anyone checks one without an account:
//
//	GET  /.well-known/agent-card.json                 Talyvor's own card
//	GET  /.well-known/talyvor-agent-cards/jwks.json   the key Talyvor's own card is signed with
//	GET  /v1/a2a/agents/{agentID}/agent-card.json     an agent's card; 404 once it is archived
//	GET  /v1/a2a/agents/{agentID}/jwks.json           the key an agent's card is signed with now
//	POST /v1/a2a/verify   <the card>                  Talyvor's answer: valid, or why not, and who answers for it
//
// and the agent's owner replaces its key, which revokes the old one:
//
//	POST /v1/workspaces/{wsID}/agents/{id}/agent-card/rotate   the card signed with the new key
//
// The first five are public, in the rate-limited group; rotate is in the authed group, so {wsID} is bound to the
// caller's credential, and it takes the workspace's owner or an admin.

type agentCardService interface {
	Card(ctx context.Context, agentID string) (a2acard.Card, error)
	TalyvorCard() (a2acard.Card, error)
	JWKS(ctx context.Context, agentID string) (kya.JWKS, error)
	TalyvorJWKS() kya.JWKS
	Rotate(ctx context.Context, workspaceID, agentID string) (a2acard.Card, error)
	Verify(ctx context.Context, raw []byte) (a2acard.Verification, error)
}

func writeAgentCardErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, a2acard.ErrArchived):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

func mountAgentCardPublicRoutes(r chi.Router, svc agentCardService) {
	r.Get("/.well-known/agent-card.json", func(w http.ResponseWriter, req *http.Request) {
		c, err := svc.TalyvorCard()
		if err != nil {
			writeAgentCardErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		writeJSONOK(w, http.StatusOK, c)
	})
	r.Get("/.well-known/talyvor-agent-cards/jwks.json", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		writeJSONOK(w, http.StatusOK, svc.TalyvorJWKS())
	})
	r.Get("/v1/a2a/agents/{agentID}/agent-card.json", func(w http.ResponseWriter, req *http.Request) {
		c, err := svc.Card(req.Context(), chi.URLParam(req, "agentID"))
		if err != nil {
			writeAgentCardErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=60")
		writeJSONOK(w, http.StatusOK, c)
	})
	r.Get("/v1/a2a/agents/{agentID}/jwks.json", func(w http.ResponseWriter, req *http.Request) {
		set, err := svc.JWKS(req.Context(), chi.URLParam(req, "agentID"))
		if err != nil {
			writeAgentCardErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=60") // a rotated key leaves within a minute
		writeJSONOK(w, http.StatusOK, set)
	})
	r.Post("/v1/a2a/verify", func(w http.ResponseWriter, req *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 64<<10))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "the body must be the Agent Card, at most 64 KiB")
			return
		}
		v, err := svc.Verify(req.Context(), raw)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, v)
	})
}

func mountAgentCardOwnerRoutes(r chi.Router, svc agentCardService) {
	r.Post("/v1/workspaces/{wsID}/agents/{id}/agent-card/rotate", func(w http.ResponseWriter, req *http.Request) {
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may replace an agent's card key")
			return
		}
		c, err := svc.Rotate(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "id"))
		if err != nil {
			writeAgentCardErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
}
