package auth

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	wsrole "github.com/talyvor/lens/internal/workspace/role"
)

// roles.go — B28.342: a person the workspace's owner gave a role (Viewer, Finance, Approver or Admin) signs in to the
// workspace with a role session: a JWT naming the role and the person, never the owner's scopes. What a role may do
// is decided HERE, in Authenticate, like a Connect token's reach, so every handler — behind AuthMiddleware or calling
// Authenticate itself — refuses a role everything its list does not name. The role is read again on every request,
// so a role the owner removes or changes is in force at once.

// ErrRoleRoute is a role session used for something its role may not do: AuthMiddleware answers it 403.
var ErrRoleRoute = errors.New("auth: your role in this workspace does not allow this")

// RoleScopes are a role session's scopes: reads. No role session carries ScopeKeys, so every gate that admits only the
// owner's own session (keyOwnerOnly) refuses it by the check it already performs.
var RoleScopes = []string{ScopeAnalytics}

// roleSource is the role a person holds in a workspace now, "" when none (workspace.Manager.PersonRole).
type roleSource interface {
	PersonRole(ctx context.Context, workspaceID, personKey string) (string, error)
}

// WithRoles is how Authenticate reads a role session's role: without it, every role session is refused.
func (m *Manager) WithRoles(r roleSource) *Manager { m.roles = r; return m }

// GenerateRoleToken signs a role session for personKey in workspaceID.
func GenerateRoleToken(workspaceID, personKey, role string, key *ecdsa.PrivateKey, ttl time.Duration) (string, error) {
	if key == nil {
		return "", errors.New("auth: jwt signing key not configured")
	}
	if workspaceID == "" || personKey == "" {
		return "", errors.New("auth: a role session names its workspace and its person")
	}
	if err := wsrole.Check(role); err != nil {
		return "", err
	}
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodES256, TokenClaims{
		WorkspaceID: workspaceID,
		UserID:      personKey,
		Scopes:      append([]string{}, RoleScopes...),
		Role:        role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    JWTIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Subject:   personKey,
			ID:        uuid.NewString(), // one per sign-in, as GenerateToken's
		},
	})
	t.Header["kid"] = JWTKid
	return t.SignedString(key)
}

// roleContext is the identity a role session resolves to on r, with the role it holds now: ErrInvalidAuth when it
// holds none any more, ErrRoleRoute when its role may not do what r asks.
func (m *Manager) roleContext(r *http.Request, actx *AuthContext) (*AuthContext, error) {
	if m.roles == nil || actx.UserID == "" || actx.WorkspaceID == "" {
		return nil, ErrInvalidAuth
	}
	role, err := m.roles.PersonRole(r.Context(), actx.WorkspaceID, actx.UserID)
	if err != nil {
		return nil, errors.Join(ErrAuthUnavailable, err)
	}
	if role == "" {
		return nil, ErrInvalidAuth
	}
	actx.Role = role
	actx.Scopes = append([]string{}, RoleScopes...)
	if !RoleAllows(role, r.Method, r.URL.Path) || r.URL.RawPath != "" {
		return nil, ErrRoleRoute
	}
	return actx, nil
}

// ownerOnlySegments are the parts of a path no role reaches, whatever the method: the workspace's keys, passkeys,
// devices, recovery, lockdown and roles stay its owner's, and so do the owner's own identity, verification, billing,
// business details and the requests they file (closing the account, the data export and its download).
var ownerOnlySegments = []string{
	"keys", "api-keys", "key-rotations", "provider-keys", "session-keys", "partner-keys", "passkey", "passkeys",
	"devices", "enrolments", "recover", "recovery-code", "recovery-signer", "lockdown", "roles",
	"connect-token", "connect-revocations", "permission-mandates", "mandates",
	"verification", "requests", "export", "billing", "identity", "business-details",
}

// financeRoutes are what Finance may do beside reading: move money in, out and between the workspace's accounts and
// agents. "*" is one segment.
var financeRoutes = []string{
	"/v1/workspaces/*/agents/*/fund",
	"/v1/workspaces/*/agents/*/fund-requests",
	"/v1/workspaces/*/agents/fund-requests/*/approve",
	"/v1/workspaces/*/agents/fund-requests/*/decline",
	"/v1/workspaces/*/agents/*/withdraw",
	"/v1/workspaces/*/agents/*/cash-outs",
	"/v1/workspaces/*/agents/*/cash-outs/challenge",
	"/v1/money/accounts/*/funding",
	"/v1/money/accounts/*/withdraw",
	"/v1/money/accounts/*/withdraw/challenge",
	"/v1/money/payments",
	"/v1/money/payments/challenge",
	"/v1/money/payments/quote",
	"/v1/money/convert",
	"/v1/money/convert/challenge",
	"/v1/money/convert/quote",
	"/v1/money/people/pay",
	"/v1/money/people/pay/challenge",
	"/v1/money/bills/pay",
	"/v1/money/payment-requests",
	"/v1/money/payment-requests/*/pay",
	"/v1/money/payment-requests/*/cancel",
	"/v1/money/payouts",
	"/v1/money/invoices",
	"/v1/money/invoices/*/send",
}

// approverRoutes are what Approver may do beside reading: approve or decline what waits for an approval.
var approverRoutes = []string{
	"/v1/workspaces/*/agents/approvals/decide",
	"/v1/workspaces/*/agents/approvals/challenge",
	"/v1/workspaces/*/agents/approvals/*/approve",
	"/v1/workspaces/*/agents/approvals/*/deny",
	"/v1/workspaces/*/agents/approvals/*/challenge",
	"/v1/money/payment-requests/*/approve",
	"/v1/money/payment-requests/*/approval",
	"/v1/money/payouts/*/approve",
	"/v1/money/payouts/*/approve/challenge",
}

// adminRoutes are what Admin may do beside what Finance and Approver may: make an agent, set its rules and pause it.
// Each is "METHOD path".
var adminRoutes = []string{
	"POST /v1/workspaces/*/agents",
	"PUT /v1/workspaces/*/agents/*/rules",
	"POST /v1/workspaces/*/agents/*/rules/challenge",
	"POST /v1/workspaces/*/agents/*/pause",
}

// RoleAllows reports whether role may make a method request of path. Every role reads (GET and HEAD) everything but
// the owner's own parts; beside that Viewer does nothing, Finance moves money (financeRoutes), Approver decides
// approvals (approverRoutes), and Admin does both and adminRoutes. Anything not named is refused. This is the first
// gate: a handler still admits a role session only where it names that role (storedanswers.OwnerOrRole), and
// storedanswers.OwnerOrAdmin refuses every role session.
func RoleAllows(role, method, path string) bool {
	if wsrole.Check(role) != nil || !strings.HasPrefix(path, "/v1/") {
		return false
	}
	segs := strings.Split(strings.TrimSuffix(path, "/"), "/")
	for _, s := range segs[1:] {
		if s == "" || slices.Contains(ownerOnlySegments, s) {
			return false
		}
	}
	if len(segs) > 2 && (segs[2] == "admin" || segs[2] == "auth" || segs[2] == "operator" || segs[2] == "synthetic") {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	}
	switch role {
	case wsrole.Admin:
		return method == http.MethodPost && (matchesAny(financeRoutes, segs) || matchesAny(approverRoutes, segs)) ||
			matchesMethod(adminRoutes, method, segs)
	case wsrole.Finance:
		return method == http.MethodPost && matchesAny(financeRoutes, segs)
	case wsrole.Approver:
		return method == http.MethodPost && matchesAny(approverRoutes, segs)
	}
	return false
}

// matchesMethod is matchesAny for "METHOD path" patterns.
func matchesMethod(patterns []string, method string, segs []string) bool {
	for _, p := range patterns {
		if m, path, _ := strings.Cut(p, " "); m == method && matchesAny([]string{path}, segs) {
			return true
		}
	}
	return false
}

func matchesAny(patterns []string, segs []string) bool {
	for _, p := range patterns {
		ps := strings.Split(p, "/")
		if len(ps) != len(segs) {
			continue
		}
		ok := true
		for i := range ps {
			if ps[i] != "*" && ps[i] != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
