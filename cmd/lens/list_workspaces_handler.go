package main

import (
	"net/http"

	"github.com/talyvor/lens/internal/workspace"
)

// workspaceRoster is the read seam for the workspace list surfaces (*workspace.Manager satisfies it). Both
// methods already exist on the manager; this endpoint only surfaces them over HTTP.
type workspaceRoster interface {
	GetWorkspace(id string) (*workspace.Workspace, bool)
	ListWorkspaces() []*workspace.Workspace
}

// newListMyWorkspacesHandler serves GET /v1/workspaces — the workspaces the caller's credential can act on.
// It ALWAYS returns a JSON array. Today that array has a single element (a credential maps to exactly one
// workspace), but returning an array from day one means a future multi-workspace user token widens it with
// NO contract change — a workspace switcher can be built against a stable shape now. Self-scoped via the
// authenticated WorkspaceID (the caller can never name another workspace); fail-closed to 401 with none.
func newListMyWorkspacesHandler(authn verdictAuthenticator, roster workspaceRoster) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		out := []*workspace.Workspace{}
		if ws, ok := roster.GetWorkspace(ac.WorkspaceID); ok {
			out = append(out, ws)
		}
		writeJSONOK(w, http.StatusOK, out)
	}
}

// newAdminListWorkspacesHandler serves GET /v1/admin/workspaces — the full tenant roster. Wrapped by
// requireAdmin at the mount site (a non-admin must never enumerate other tenants). Returns a JSON array.
// Synthetic workspaces are left out; ?synthetic=only lists them alone (B17.7).
func newAdminListWorkspacesHandler(roster workspaceRoster) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		audience, err := workspace.ParseAudience(req.URL.Query().Get(workspace.AudienceQueryParam))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		out := []*workspace.Workspace{}
		for _, ws := range roster.ListWorkspaces() {
			if audience.Includes(ws) {
				out = append(out, ws)
			}
		}
		writeJSONOK(w, http.StatusOK, out)
	}
}
