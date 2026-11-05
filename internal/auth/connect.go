package auth

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// B52.4 — the Connect token: what an assistant connected through Claude or ChatGPT calls Lens with, instead of the
// person's own token. The BFF mints one per connection (POST /v1/auth/connect-token, the mint credential), bound to
// one agent and the scopes the person granted, and Lens alone decides what it reaches, so a bug in the BFF cannot
// widen it.
const (
	// ConnectAudience marks a JWT as a Connect token. Only GenerateConnectToken sets an audience.
	ConnectAudience = "talyvor-connect"
	MethodConnect   = "connect"
	// ScopePaymentsSend lets a Connect token pay from its agent, inside the agent's rules.
	ScopePaymentsSend = "payments:send"
	// ConnectTokenTTL is how long a Connect token lives; the BFF mints a fresh one as it needs.
	ConnectTokenTTL = 15 * time.Minute
)

// ConnectScopes are the only scopes a Connect token may carry.
var ConnectScopes = []string{ScopePaymentsSend}

// ErrConnectRoute is a Connect token used anywhere but where it may be: AuthMiddleware answers it 403.
var ErrConnectRoute = errors.New("auth: a Connect token may only pay from its own agent")

// GenerateConnectToken signs a Connect token for one agent of workspaceID, on behalf of clientID (the assistant).
func GenerateConnectToken(workspaceID, userID, clientID, agentID string, scopes []string, key *ecdsa.PrivateKey, ttl time.Duration) (string, error) {
	if key == nil {
		return "", errors.New("auth: jwt signing key not configured")
	}
	if workspaceID == "" || clientID == "" || agentID == "" || len(scopes) == 0 {
		return "", errors.New("auth: a Connect token names its workspace, client, agent and at least one scope")
	}
	for _, s := range scopes {
		if !slices.Contains(ConnectScopes, s) {
			return "", fmt.Errorf("auth: a Connect token may not carry the scope %q", s)
		}
	}
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodES256, TokenClaims{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Scopes:      append([]string{}, scopes...),
		AgentID:     agentID,
		ClientID:    clientID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    JWTIssuer,
			Audience:  jwt.ClaimStrings{ConnectAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(min(ttl, ConnectTokenTTL))),
			Subject:   userID,
		},
	})
	t.Header["kid"] = JWTKid
	return t.SignedString(key)
}

// connectContext is the identity a Connect token resolves to on r, or ErrConnectRoute when r is not its agent's
// pay route. The check lives here, in Authenticate, so every handler — those behind AuthMiddleware and those that
// call Authenticate themselves — refuses the token everywhere else.
func connectContext(r *http.Request, c *TokenClaims) (*AuthContext, error) {
	// An escaped path (RawPath set) is refused: chi routes on it, so it could name another route than Path does.
	if c.AgentID == "" || c.ClientID == "" || r.Method != http.MethodPost || r.URL.RawPath != "" ||
		r.URL.Path != "/v1/workspaces/"+c.WorkspaceID+"/agents/"+c.AgentID+"/pay" {
		return nil, ErrConnectRoute
	}
	actx := &AuthContext{
		WorkspaceID:     c.WorkspaceID,
		UserID:          c.UserID,
		Scopes:          c.Scopes,
		AuthMethod:      MethodConnect,
		ConnectAgentID:  c.AgentID,
		ConnectClientID: c.ClientID,
	}
	if c.ExpiresAt != nil {
		actx.ExpiresAt = c.ExpiresAt.Time
	}
	return actx, nil
}
