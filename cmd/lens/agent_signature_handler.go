package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
)

// B45.24 — Talyvor checks outside agents' signatures (internal/agentsig, docs/agent-signatures.md). Every request to
// the A2A endpoints is checked against Web Bot Auth and answered with a Talyvor-Agent-Trust header; a signature that
// is present and does not hold is 401. An outside agent's developer sees how Talyvor reads their signature at:
//
//	GET  /v1/a2a/signature-check         under Web Bot Auth
//	POST /v1/a2a/signature-check/money   under Talyvor's money profile: method, full URL and body digest signed,
//	                                     at most 5 minutes, a nonce accepted once
//
// Both answer the trust level: "Verified agent of <business>", "Signed by <domain> (business not checked)" or
// "Unverified agent". The businesses Talyvor has checked are the operator's:
//
//	GET    /v1/admin/agent-operators            every checked signing domain
//	PUT    /v1/admin/agent-operators/{domain}   {"business": "Acme Inc."}  — behind step-up
//	DELETE /v1/admin/agent-operators/{domain}                              — behind step-up

func mountAgentSignatureRoutes(pub chi.Router, v *agentsig.Verifier) {
	report := func(w http.ResponseWriter, req *http.Request) {
		res, _ := agentsig.FromContext(req.Context())
		writeJSONOK(w, http.StatusOK, res)
	}
	wba, money := v.Middleware(agentsig.WebBotAuth), v.Middleware(agentsig.Money)
	pub.With(wba).Get("/v1/a2a/signature-check", report)
	pub.With(money).Post("/v1/a2a/signature-check/money", report)
}

// checkedOperatorStore is agentsig.Store.
type checkedOperatorStore interface {
	CheckedOperators(ctx context.Context) ([]agentsig.CheckedOperator, error)
	SetChecked(ctx context.Context, domain, business, by string) (agentsig.CheckedOperator, error)
	RemoveChecked(ctx context.Context, domain, by string) (bool, error)
}

func newCheckedOperatorsListHandler(s checkedOperatorStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ops, err := s.CheckedOperators(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the checked agent operators")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"operators": ops})
	}
}

// newCheckedOperatorSetHandler records that a signing domain belongs to a business Talyvor has checked. Gated by
// requireStepUp at the mount, which also makes X-Talyvor-Operator present: it is the row's checked_by and the operator
// audit row's actor.
func newCheckedOperatorSetHandler(s checkedOperatorStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		domain, ok := agentsig.NormaliseDomain(chi.URLParam(req, "domain"))
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "the domain is a bare host name, e.g. agent.acme.com")
			return
		}
		var in struct {
			Business string `json:"business"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		business := strings.TrimSpace(in.Business)
		if business == "" || len([]rune(business)) > 200 {
			writeJSONErr(w, http.StatusBadRequest, "business is the checked business's name, 1 to 200 characters")
			return
		}
		o, err := s.SetChecked(req.Context(), domain, business, strings.TrimSpace(req.Header.Get(moderatorOperatorHeader)))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not record the checked operator")
			return
		}
		writeJSONOK(w, http.StatusOK, o)
	}
}

func newCheckedOperatorRemoveHandler(s checkedOperatorStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		domain, _ := agentsig.NormaliseDomain(chi.URLParam(req, "domain"))
		gone, err := s.RemoveChecked(req.Context(), domain, strings.TrimSpace(req.Header.Get(moderatorOperatorHeader)))
		switch {
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not remove the checked operator")
		case !gone:
			writeJSONErr(w, http.StatusNotFound, "that domain is not a checked operator")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}
