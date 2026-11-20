package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/keel"
)

// keelFindingsLister is the read seam for the drift-findings endpoints (Query-only; *keel.Reader satisfies
// it). Kept an interface so the gates are testable without a DB.
type keelFindingsLister interface {
	ListFindings(ctx context.Context, limit int) ([]keel.ListedFinding, error)
	ListFindingsForWorkspacePage(ctx context.Context, workspaceID string, limit, offset int) ([]keel.ListedFinding, error)
}

// keelFindingsAuthenticator resolves the caller's identity (the authManager satisfies it). The tenant read's
// scope comes from the returned AuthContext.WorkspaceID — NEVER a caller-supplied param.
type keelFindingsAuthenticator interface {
	Authenticate(*http.Request) (*auth.AuthContext, error)
}

// newKeelFindingsHandler serves the recorded drift findings as JSON. Wrapped by requireAdmin at the mount
// site — a tenant must never read another tenant's drift attribution. Rows carry only a self workspace +
// cohort aggregates (no counterparty raw value), so an admin forensic read leaks nothing cross-tenant.
func newKeelFindingsHandler(l keelFindingsLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		findings, err := l.ListFindings(req.Context(), 100)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if findings == nil {
			findings = []keel.ListedFinding{}
		}
		writeJSONOK(w, http.StatusOK, findings)
	}
}

// newKeelFindingsWorkspaceHandler serves GET /v1/keel/findings — a tenant reads ONLY its OWN drift findings,
// scoped to the authenticated AuthContext.WorkspaceID. A caller-supplied workspace_id is IGNORED (the request
// is never parsed for one). Fail-closed: no resolved workspace → 401. Every row carries mode + attribution.
// Mirrors the proven K4 workspace-scoped verdict handler.
func newKeelFindingsWorkspaceHandler(authn keelFindingsAuthenticator, l keelFindingsLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Pagination: ?limit=&offset= (newest-first). Absent/garbage → 0 → store default page; the
		// response stays a bare array, so the shape is unchanged.
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(req.URL.Query().Get("offset"))
		findings, err := l.ListFindingsForWorkspacePage(req.Context(), ac.WorkspaceID, limit, offset)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if findings == nil {
			findings = []keel.ListedFinding{}
		}
		writeJSONOK(w, http.StatusOK, findings)
	}
}
