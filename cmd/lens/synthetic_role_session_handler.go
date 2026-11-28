package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/workspace"
)

// B17.584 — the testers sign a role person in to a test workspace on the synthetic key, so the role scenarios run
// without the provisioning secret (which mints any person's session, and the testers are never given):
//
//	POST /v1/synthetic/workspaces/{wsID}/role-session  {"email", "role", "ttl_hours"} → {"workspace_id", "role",
//	                                     "token", "expires_at"}: the same role session POST /v1/provision/roles'
//	                                     session mode mints, for a role the owner gave that email in that workspace.
//
// Only a test (synthetic, active) workspace: any other is 404, as if it were not there. Only the role the owner gave:
// an email with none is 404 and one holding another role 403. The TTL is clamped as the real route's is.

// syntheticRoleIdentity is the identity a test role person signs in as: one per email, so the role is taken up by the
// same person every run.
func syntheticRoleIdentity(email string) string { return "synthetic-role|" + email }

func (d syntheticDeps) roleSession(w http.ResponseWriter, r *http.Request) (int, string) {
	ws := chi.URLParam(r, "wsID")
	var in struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		TTLHours int    `json:"ttl_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return 0, "error: bad request"
	}
	email, err := workspace.RoleEmail(in.Email)
	if err != nil || in.Role == "" {
		writeJSONErr(w, http.StatusBadRequest, "email and role required")
		return 0, "error: bad request"
	}
	var test bool
	if err := d.plans.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1 AND synthetic AND active)`, ws).Scan(&test); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	if !test {
		writeJSONErr(w, http.StatusNotFound, "no such test workspace")
		return 0, "refused: not a test workspace"
	}
	person := workspace.PersonKey(syntheticRoleIdentity(email))
	role, err := d.roles.TakeUpRole(r.Context(), ws, email, person)
	if err != nil {
		writeRoleErr(w, err)
		if errors.Is(err, workspace.ErrNoRole) {
			return 0, "refused: no role"
		}
		return 0, "error: " + err.Error()
	}
	if role != in.Role {
		writeJSONErr(w, http.StatusForbidden, "that email holds another role in this workspace")
		return 0, "refused: another role"
	}
	ttl := auth.ClampTTL(time.Duration(in.TTLHours) * time.Hour)
	tok, err := d.roleMint(ws, person, role, ttl)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, map[string]any{
		"workspace_id": ws,
		"role":         role,
		"token":        tok,
		"expires_at":   time.Now().Add(ttl).UTC().Format(time.RFC3339),
	})
	return 1, "ok"
}
