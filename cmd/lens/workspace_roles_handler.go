package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// workspace_roles_handler.go — B28.342: the owner gives people a role in the workspace, and a person signs in to it
// with that role.
//
//	GET    /v1/workspaces/{wsID}/roles              the roles given, oldest first (the owner's session only)
//	PUT    /v1/workspaces/{wsID}/roles              {"email", "role"}: give an email a role, or change it
//	DELETE /v1/workspaces/{wsID}/roles?email=…      take it away; every session it gave ends at once
//	POST   /v1/provision/roles                      the BFF, on the provisioning secret: {"identity", "email",
//	                                                "mode": "list"} lists the accounts the person holds a role in;
//	                                                {"mode": "session", "account_id"} mints a role session for one
//
// What each role may do is internal/auth/roles.go's, decided in Authenticate on every request.

type roleBook interface {
	SetRole(ctx context.Context, workspaceID, email, role, by string) (workspace.WorkspaceRole, error)
	RemoveRole(ctx context.Context, workspaceID, email string) (bool, error)
	WorkspaceRoles(ctx context.Context, workspaceID string) ([]workspace.WorkspaceRole, error)
}

type roleSessions interface {
	RoleAccounts(ctx context.Context, email, personKey string) ([]workspace.RoleAccount, error)
	TakeUpRole(ctx context.Context, workspaceID, email, personKey string) (string, error)
}

// The roles a handler admits beside the owner (B28.342), each handler by name: moneyRoles where auth's financeRoutes
// reach, approvalRoles where its approverRoutes reach, adminRoles for its adminRoutes. Every other handler refuses a
// role session, because storedanswers.OwnerOrAdmin does.
var (
	moneyRoles    = []string{workspace.RoleFinance, workspace.RoleAdmin}
	approvalRoles = []string{workspace.RoleApprover, workspace.RoleAdmin}
	adminRoles    = []string{workspace.RoleAdmin}
)

// ownerOrRole is ownerOnly that also admits a role session holding one of roles (B28.342).
func ownerOrRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrRole(req.Context(), roles...); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or a person whose role allows it may do this")
			return
		}
		next(w, req)
	}
}

// mayAlwaysAllow refuses 403 an "always allow" from anyone but the owner or an Admin (B50.59): it makes a standing rule
// for the agent, and changing an agent's rules is adminRoles' as on every other rules route. An Approver still approves
// or declines without it.
func mayAlwaysAllow(w http.ResponseWriter, req *http.Request, allow *economy.PayeeAllowance) bool {
	if allow == nil {
		return true
	}
	if _, ok := storedanswers.OwnerOrRole(req.Context(), adminRoles...); !ok {
		writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may change an agent's rules: approve without always allow")
		return false
	}
	return true
}

// roleTokenMinter mints a role session.
type roleTokenMinter func(workspaceID, personKey, role string, ttl time.Duration) (string, error)

func writeRoleErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workspace.ErrRoleUnknown), errors.Is(err, workspace.ErrRoleEmail):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrNoRole):
		writeJSONErr(w, http.StatusNotFound, "no such account")
	case errors.Is(err, workspace.ErrRoleTaken):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

func mountWorkspaceRoleRoutes(r chi.Router, book roleBook) {
	r.Get("/v1/workspaces/{wsID}/roles", keyOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		roles, err := book.WorkspaceRoles(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeRoleErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"items": roles, "roles": workspace.Roles})
	}))
	r.Put("/v1/workspaces/{wsID}/roles", keyOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"email": "…", "role": "viewer" | "finance" | "approver" | "admin"}`)
			return
		}
		by, _ := storedanswers.OwnerOrAdmin(req.Context())
		role, err := book.SetRole(req.Context(), chi.URLParam(req, "wsID"), in.Email, in.Role, by)
		if err != nil {
			writeRoleErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, role)
	}))
	r.Delete("/v1/workspaces/{wsID}/roles", keyOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		removed, err := book.RemoveRole(req.Context(), chi.URLParam(req, "wsID"), req.URL.Query().Get("email"))
		if err != nil {
			writeRoleErr(w, err)
			return
		}
		if !removed {
			writeJSONErr(w, http.StatusNotFound, "that email has no role in this workspace")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// mountRoleSessionRoute registers POST /v1/provision/roles on the provisioning secret, and nothing when it is unset.
// It takes the email the BFF sends on trust: a role is given to an email, so whoever names it holds the role. The
// BFF must send only an address the person proved with a code sent to it, in this sign-in or an earlier one, never
// one they typed and did not prove.
func mountRoleSessionRoute(r chi.Router, secret string, sessions roleSessions, mint roleTokenMinter) {
	if secret == "" {
		return
	}
	r.Post("/v1/provision/roles", func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(provisionSecretHeader)), []byte(secret)) != 1 {
			writeJSONErr(w, http.StatusUnauthorized, "provisioning credentials required")
			return
		}
		var in struct {
			Identity  string `json:"identity"`
			Email     string `json:"email"` // an email the person proved with a code sent to it, never one only typed
			Mode      string `json:"mode"`
			AccountID string `json:"account_id"`
			TTLHours  int    `json:"ttl_hours"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.Identity == "" || in.Email == "" {
			writeJSONErr(w, http.StatusBadRequest, "identity and email required")
			return
		}
		person := workspace.PersonKey(in.Identity)
		switch in.Mode {
		case provisionModeList:
			accounts, err := sessions.RoleAccounts(req.Context(), in.Email, person)
			if err != nil {
				writeRoleErr(w, err)
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{"accounts": accounts})
		case provisionModeSession:
			if in.AccountID == "" {
				writeJSONErr(w, http.StatusBadRequest, "account_id required")
				return
			}
			role, err := sessions.TakeUpRole(req.Context(), in.AccountID, in.Email, person)
			if err != nil {
				writeRoleErr(w, err)
				return
			}
			ttl := auth.ClampTTL(time.Duration(in.TTLHours) * time.Hour)
			tok, err := mint(in.AccountID, person, role, ttl)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{
				"workspace_id": in.AccountID,
				"role":         role,
				"token":        tok,
				"expires_at":   time.Now().Add(ttl).UTC().Format(time.RFC3339),
			})
		default:
			writeJSONErr(w, http.StatusBadRequest, "mode must be list or session")
		}
	})
}
