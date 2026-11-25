package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/workspace/role"
)

// B28.342: a workspace's owner gives other people a role in it (migration 2611231954a, workspace_roles). The owner is
// not a role: whoever signs in to their own account holds every power it has, as before.
// The roles themselves live in package role, so auth can name them without importing workspace.
const (
	RoleViewer   = role.Viewer
	RoleFinance  = role.Finance
	RoleApprover = role.Approver
	RoleAdmin    = role.Admin
)

// Roles are the roles a person may be given, least power first.
var Roles = role.All

// ErrRoleUnknown refuses a role that is not one of Roles.
var ErrRoleUnknown = role.ErrUnknown

// ErrRoleEmail refuses an email that is not one address.
var ErrRoleEmail = errors.New("email must be one email address")

// ErrRoleTaken refuses a session for a role another person already signed in with.
var ErrRoleTaken = errors.New("this role was taken up by another person")

// ErrNoRole is a person with no role in the workspace.
var ErrNoRole = errors.New("no role in this workspace")

// WorkspaceRole is one person's role in a workspace, as the owner's Members page lists it.
type WorkspaceRole struct {
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	SignedIn  bool      `json:"signed_in"` // a person has taken the role up
	UserID    string    `json:"user_id"`   // the person key of who took it up, their role session's UserID; "" until then
	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RoleAccount is an account a person holds a role in, as the account switcher shows it beside their own.
type RoleAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Kind string `json:"kind"`
	Role string `json:"role"`
}

// CheckRole reports ErrRoleUnknown unless role is one of Roles.
func CheckRole(r string) error { return role.Check(r) }

// RoleEmail is email as workspace_roles keys it: one bare address, lower case.
func RoleEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || a.Name != "" || len(email) > 254 {
		return "", ErrRoleEmail
	}
	return strings.ToLower(email), nil
}

func (m *Manager) rolesPool() error {
	if m.pool == nil {
		return errors.New("workspace: roles need a database")
	}
	return nil
}

// SetRole gives email the role in workspaceID, or changes the one it has. by names who did it.
func (m *Manager) SetRole(ctx context.Context, workspaceID, email, role, by string) (WorkspaceRole, error) {
	var out WorkspaceRole
	if err := CheckRole(role); err != nil {
		return out, err
	}
	email, err := RoleEmail(email)
	if err != nil {
		return out, err
	}
	if err := m.rolesPool(); err != nil {
		return out, err
	}
	err = m.pool.QueryRow(ctx, `INSERT INTO workspace_roles (workspace_id, email, role, granted_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, email) DO UPDATE SET role = EXCLUDED.role, updated_at = now()
		RETURNING email, role, person_key IS NOT NULL, COALESCE(person_key, ''), granted_by, granted_at, updated_at`,
		workspaceID, email, role, by).Scan(&out.Email, &out.Role, &out.SignedIn, &out.UserID, &out.GrantedBy, &out.GrantedAt, &out.UpdatedAt)
	if err != nil {
		return out, fmt.Errorf("workspace: set a role: %w", err)
	}
	return out, nil
}

// RemoveRole takes email's role in workspaceID away, ending every session it gave; false when it had none.
func (m *Manager) RemoveRole(ctx context.Context, workspaceID, email string) (bool, error) {
	email, err := RoleEmail(email)
	if err != nil {
		return false, err
	}
	if err := m.rolesPool(); err != nil {
		return false, err
	}
	tag, err := m.pool.Exec(ctx, `DELETE FROM workspace_roles WHERE workspace_id = $1 AND email = $2`, workspaceID, email)
	if err != nil {
		return false, fmt.Errorf("workspace: remove a role: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// WorkspaceRoles lists the roles given in workspaceID, oldest first.
func (m *Manager) WorkspaceRoles(ctx context.Context, workspaceID string) ([]WorkspaceRole, error) {
	if err := m.rolesPool(); err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, `SELECT email, role, person_key IS NOT NULL, COALESCE(person_key, ''), granted_by, granted_at, updated_at
		FROM workspace_roles WHERE workspace_id = $1 ORDER BY granted_at, email`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: list roles: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkspaceRole, error) {
		var r WorkspaceRole
		return r, row.Scan(&r.Email, &r.Role, &r.SignedIn, &r.UserID, &r.GrantedBy, &r.GrantedAt, &r.UpdatedAt)
	})
}

// RoleAccounts lists the accounts the person signed in with email holds a role in: those given to the email that no
// one else has taken up. Oldest first.
func (m *Manager) RoleAccounts(ctx context.Context, email, personKey string) ([]RoleAccount, error) {
	email, err := RoleEmail(email)
	if err != nil {
		return nil, err
	}
	if err := m.rolesPool(); err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, `SELECT w.id, w.name, w.account_type, w.money_kind, r.role
		FROM workspace_roles r JOIN workspaces w ON w.id = r.workspace_id
		WHERE r.email = $1 AND (r.person_key IS NULL OR r.person_key = $2) ORDER BY r.granted_at, w.id`, email, personKey)
	if err != nil {
		return nil, fmt.Errorf("workspace: list a person's roles: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RoleAccount, error) {
		var a RoleAccount
		return a, row.Scan(&a.ID, &a.Name, &a.Type, &a.Kind, &a.Role)
	})
}

// TakeUpRole is the person's role in workspaceID through email, binding the role to them on their first session:
// ErrNoRole when the email has none, ErrRoleTaken when another person took it up first.
func (m *Manager) TakeUpRole(ctx context.Context, workspaceID, email, personKey string) (string, error) {
	email, err := RoleEmail(email)
	if err != nil {
		return "", err
	}
	if err := m.rolesPool(); err != nil {
		return "", err
	}
	var role, holder string
	err = m.pool.QueryRow(ctx, `UPDATE workspace_roles SET person_key = COALESCE(person_key, $3)
		WHERE workspace_id = $1 AND email = $2 RETURNING role, person_key`, workspaceID, email, personKey).Scan(&role, &holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrNoRole
	case err != nil:
		return "", fmt.Errorf("workspace: take up a role: %w", err)
	case holder != personKey:
		return "", ErrRoleTaken
	}
	return role, nil
}

// PersonRole is the role personKey holds in workspaceID now, "" when none: read on every request a role's session
// makes, so a role removed or changed is in force at once.
func (m *Manager) PersonRole(ctx context.Context, workspaceID, personKey string) (string, error) {
	if err := m.rolesPool(); err != nil {
		return "", err
	}
	var role string
	err := m.pool.QueryRow(ctx, `SELECT role FROM workspace_roles WHERE workspace_id = $1 AND person_key = $2`,
		workspaceID, personKey).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("workspace: read a person's role: %w", err)
	}
	return role, nil
}
