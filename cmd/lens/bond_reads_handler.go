package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/provenance"
)

// bondReader is the read seam for the provenance-bond read-back (*provenance.BondReader satisfies it).
type bondReader interface {
	ListByWorkspace(ctx context.Context, workspaceID string, limit int) ([]provenance.Bond, error)
	GetByID(ctx context.Context, workspaceID, bondID string) (provenance.Bond, bool, error)
}

// newBondListHandler serves GET /v1/bonds — the caller's OWN bonds (self-scoped via the authenticated
// WorkspaceID; a caller can never name another workspace). Always a JSON array.
func newBondListHandler(authn verdictAuthenticator, reader bondReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		bonds, err := reader.ListByWorkspace(req.Context(), ac.WorkspaceID, limit)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if bonds == nil {
			bonds = []provenance.Bond{}
		}
		writeJSONOK(w, http.StatusOK, bonds)
	}
}

// newBondGetHandler serves GET /v1/bonds/{bond_id} — owner-scoped. A bond the caller does not own resolves
// to not-found → 404 (never 403): no cross-tenant existence oracle.
func newBondGetHandler(authn verdictAuthenticator, reader bondReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		bondID := chi.URLParam(req, "bond_id")
		if bondID == "" {
			writeJSONErr(w, http.StatusBadRequest, "missing bond_id")
			return
		}
		bond, ok, err := reader.GetByID(req.Context(), ac.WorkspaceID, bondID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeJSONErr(w, http.StatusNotFound, "bond not found")
			return
		}
		writeJSONOK(w, http.StatusOK, bond)
	}
}

// registerBondReadRoutes registers the bond read-back routes ONLY when H5 bonds are enabled — flag-off ⇒ the
// routes are never registered (chi 404), matching their POST (/v1/bonds) and admin-settle write siblings.
func registerBondReadRoutes(authed chi.Router, enabled bool, authn verdictAuthenticator, reader bondReader) {
	if !enabled {
		return
	}
	authed.Get("/v1/bonds", newBondListHandler(authn, reader))
	authed.Get("/v1/bonds/{bond_id}", newBondGetHandler(authn, reader))
}
