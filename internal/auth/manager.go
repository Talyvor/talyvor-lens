package auth

// manager.go — production auth: JWT tokens, scoped keys, and a
// unified Authenticate path that handles four credential
// shapes (JWT, workspace key, global admin key, legacy X-API-Key
// header) behind one entry point.
//
// Sits alongside the existing KeyStore (apikeys.go) — the
// legacy API-key middleware still works for backwards compat.
// New routes use Manager.Authenticate + RequireScope.

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/internal/sessionkey"
	"github.com/talyvor/lens/internal/tenant"
)

// ─── constants ───────────────────────────────────

const (
	// JWTIssuer is the iss claim Lens stamps + validates.
	JWTIssuer = "talyvor-lens"

	// DefaultTokenTTL is the JWT expiry when callers leave
	// ttl_hours unset.
	DefaultTokenTTL = 24 * time.Hour

	// MaxTokenTTL caps the longest-lived token we'll mint. 30
	// days matches the spec — anything longer should use an
	// API key.
	MaxTokenTTL = 30 * 24 * time.Hour

	// jwtCacheTTL is how long a validated token stays in the
	// validation cache. Tokens are immutable + signed, so this
	// is purely a CPU optimisation — re-running HS256 on every
	// request is wasteful.
	jwtCacheTTL = 5 * time.Minute
)

// Standard scopes — keep in sync with tenant.ValidScopes plus
// the "keys" scope which only manager.go honours.
const (
	ScopeProxy     = "proxy"
	ScopeAnalytics = "analytics"
	ScopeAdmin     = "admin"
	// ScopeMint authorises ONE thing: minting a per-workspace token via POST /v1/auth/token.
	//
	// ⚠ WHY IT EXISTS. Talyvor Docs and Talyvor Track both need per-workspace Lens tokens, and the
	// mint route requires IsAdmin — which only the global key carries. So both services were
	// obliged to hold LENS_API_KEY, a credential bearing ScopeAdmin: LXC grants, royalty
	// adjudication, injection patterns, and token minting for EVERY tenant. Docs handles
	// user-authored content, a rich-text editor, embeds and HTML — the largest attack surface in
	// the suite — so a Docs compromise would have become Lens admin over every workspace.
	// config.go already states the BFF "never has to hold LENS_API_KEY"; this makes that true of
	// the content services too.
	//
	// ⚠ IT IS A SCOPE, NOT A SECOND GLOBAL KEY. A credential bearing it authenticates with
	// IsAdmin=false, so every one of the 29 requireAdmin gates refuses it by the check they
	// already perform — the refusal is enforced by existing code, not by a convention that the
	// key "should only be used for minting".
	ScopeMint = "mint"
	ScopeKeys = "keys"

	// ScopeOperatorRead authorises ONE thing: GET on the cross-tenant admin READS the operator
	// screen is built from. It is the read-side sibling of ScopeMint and exists for the same
	// reason — a consumer that needed one capability was obliged to hold every capability.
	//
	// ⚠ WHY IT EXISTS. suite #87 shipped the operator boundary (OPERATOR_SUBS, an (issuer, sub)
	// allowlist) and every route behind it answers 501, because the BFF holds no credential Lens's
	// admin reads accept. The only credential that reaches them today is LENS_API_KEY, which is
	// IsAdmin — it mints LXC, approves conversion rates, revokes payouts and mints tokens for every
	// tenant. Giving a session gateway that key to render a six-row table is the trade #402 already
	// refused once for the content services.
	//
	// ⚠ IT IS A SCOPE, NOT A SECOND GLOBAL KEY. A credential bearing it authenticates with
	// IsAdmin=false, so all 29 requireAdmin gates refuse it by the check they already perform.
	// Reach is granted ONLY where a registration site opts in by name, and which sites those are is
	// asserted against the router source in cmd/lens/admin_route_classification_test.go.
	//
	// ⚠ IT IS NOT IN tenant.ValidScopes, AND MUST NOT BE. Workspace API keys are tenant-created; a
	// scope a tenant can name for itself is a scope a tenant can grant itself, and this one reads
	// across every workspace. operator_read_scope_is_not_tenant_grantable is the guard.
	ScopeOperatorRead = "operator_read"

	// ScopeMarketModeration is B20.13's moderator key (internal/moderatorkey): the marketplace review
	// queue — read it, approve a listing, take one down — and nothing else. Like operator_read it is
	// NOT in tenant.ValidScopes: only the operator command `lens moderator-keys` creates one.
	ScopeMarketModeration = "market_moderation"
)

// AuthMethod values shipped on AuthContext.AuthMethod.
const (
	MethodJWT          = "jwt"
	MethodWorkspaceKey = "workspace_key"
	MethodGlobalKey    = "global_key"
	// MethodMintKey is the narrow mint credential (LENS_MINT_KEY). Named distinctly from
	// MethodGlobalKey so a log line, an audit export or a future policy can tell them apart —
	// "which credential minted this token" is exactly the question an incident asks.
	MethodMintKey = "mint_key"
	// MethodOperatorReadKey is the narrow operator READ credential (LENS_OPERATOR_READ_KEY).
	// Distinct from MethodGlobalKey and MethodMintKey for the same reason those are distinct from
	// each other: "which credential read every tenant's finances" is exactly the question an
	// incident asks, and a shared method name cannot answer it.
	MethodOperatorReadKey = "operator_read_key"
	// MethodSessionKey is the browser-chat session credential (internal/sessionkey). Named
	// distinctly from MethodWorkspaceKey for the reason the mint/operator methods are named
	// distinctly from each other: "which credential spent this workspace's balance" is exactly the
	// question an incident asks, and a shared method name cannot answer it. ⚠ IT IS ALSO THE SIGNAL
	// THE MINT ROUTE REFUSES ON — a session key must not be able to mint another session key.
	MethodSessionKey   = "session_key"
	MethodLegacyHeader = "legacy_header"
	// MethodModeratorKey is the marketplace moderator credential (B20.13). Every admin gate and the
	// tenant middleware refuse it 403 by this method; only requireAdminOrModerator admits it.
	MethodModeratorKey = "moderator_key"
)

// ─── errors ──────────────────────────────────────

// Single ErrInvalidAuth value covers every "creds didn't work"
// case so the wire response can never reveal *which* lookup
// failed — preventing key-enumeration probes.
var ErrInvalidAuth = errors.New("auth: invalid credentials")

// ErrMissingCredentials is the "no Authorization header at all"
// case. The caller decides whether to 401 or fall through.
var ErrMissingCredentials = errors.New("auth: missing credentials")

// ErrAuthUnavailable is "we could not check this credential", which is not "this credential is
// wrong" (B27.5). It is returned when the credential's store failed to answer — a database
// hiccup — so the middleware answers 503 with Retry-After instead of 401, and the browser Chat
// retries rather than telling a signed-in person they are signed out. It reveals nothing about
// the key: an unknown, expired or revoked key is still ErrInvalidAuth.
var ErrAuthUnavailable = errors.New("auth: credential store unavailable")

// AuthUnavailableRetryAfter is the Retry-After, in seconds, sent with an ErrAuthUnavailable 503.
const AuthUnavailableRetryAfter = "2"

// ─── types ───────────────────────────────────────

// TokenClaims is the JWT payload Lens signs + verifies.
type TokenClaims struct {
	WorkspaceID string   `json:"workspace_id"`
	UserID      string   `json:"user_id"`
	Scopes      []string `json:"scopes"`
	jwt.RegisteredClaims
}

// AuthContext is the resolved identity downstream handlers see.
// Use GetAuthContext(ctx) to retrieve it after Manager.Middleware
// runs.
type AuthContext struct {
	WorkspaceID string   `json:"workspace_id"`
	UserID      string   `json:"user_id,omitempty"`
	Scopes      []string `json:"scopes"`
	AuthMethod  string   `json:"auth_method"`
	IsAdmin     bool     `json:"is_admin"`
	// ExpiresAt is when the credential itself stops being valid. Populated for the two shapes that
	// HAVE a lifetime — JWTs (from the exp claim) and session keys — and ZERO for the static
	// admin/mint/operator keys, which have none.
	//
	// ⚠ IT EXISTS SO A SESSION KEY CAN NEVER OUTLIVE THE SESSION THAT ASKED FOR IT. The mint route
	// clamps the new key's TTL to the caller's remaining life, and it can only do that if the
	// caller's expiry reaches the handler. Without it the clamp would be an intention.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// APIKeyID is the scoped API key's ID for API-key (workspace-key) auth; EMPTY for JWT/admin/other auth
	// methods. Consumed by the F4 allocator (C.1) to key the per-agent LXC sub-budget on the scoped key —
	// so an EMPTY APIKeyID (JWT/admin) structurally cannot enter the agent-allocation path.
	APIKeyID string `json:"api_key_id,omitempty"`
	// SessionKeyID is the browser-chat session key's ID; EMPTY for every other method. It keys the
	// per-session spend bound (B9.8) — never the agent allocator, which reads APIKeyID only.
	SessionKeyID string `json:"session_key_id,omitempty"`
	// ModeratorKeyID is the moderator key's ID (B20.13); ZERO for every other method. Every use of
	// the key is recorded against it.
	ModeratorKeyID int64 `json:"moderator_key_id,omitempty"`
}

// HasScope reports whether the resolved identity carries `scope`.
// Admin contexts (global key) implicitly carry every scope.
func (a *AuthContext) HasScope(scope string) bool {
	if a == nil {
		return false
	}
	if a.IsAdmin {
		return true
	}
	for _, s := range a.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ─── token helpers ───────────────────────────────

// GenerateToken signs an ES256 JWT carrying the given scope set.
// Caller controls TTL — Manager.GenerateTokenWithCap caps it
// against MaxTokenTTL at the API-endpoint boundary.
func GenerateToken(workspaceID, userID string, scopes []string, key *ecdsa.PrivateKey, ttl time.Duration) (string, error) {
	if key == nil {
		return "", errors.New("auth: jwt signing key not configured")
	}
	now := time.Now()
	claims := TokenClaims{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Scopes:      append([]string{}, scopes...),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    JWTIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Subject:   userID,
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["kid"] = JWTKid
	signed, err := t.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, nil
}

// ValidateToken parses + verifies a signed ES256 JWT. Returns the
// claims when everything checks out, or an error wrapping
// ErrInvalidAuth when anything fails (signature, expiry, issuer).
func ValidateToken(tokenString string, key *ecdsa.PublicKey) (*TokenClaims, error) {
	if tokenString == "" {
		return nil, ErrMissingCredentials
	}
	if key == nil {
		return nil, errors.New("auth: jwt verification key not configured")
	}
	parsed, err := jwt.ParseWithClaims(tokenString, &TokenClaims{},
		func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method: %v", t.Header["alg"])
			}
			return key, nil
		},
		jwt.WithIssuer(JWTIssuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAuth, err)
	}
	claims, ok := parsed.Claims.(*TokenClaims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidAuth
	}
	return claims, nil
}

// ─── manager ─────────────────────────────────────

// Manager is the unified authenticator. Wraps the existing
// KeyStore (global keys) and tenant.Store (workspace keys), and
// validates JWTs against the EC public key.
//
// Lifecycle: build once at startup, share across goroutines —
// the JWT cache uses RWMutex.
type Manager struct {
	globalKey       string
	mintKey         string
	operatorReadKey string
	privateKey      *ecdsa.PrivateKey
	publicKey       *ecdsa.PublicKey
	keyStore        *KeyStore
	tenantStore     *tenant.Store
	sessionKeys     sessionKeyValidator
	moderatorKeys   moderatorKeyValidator

	mu       sync.RWMutex
	jwtCache map[string]*jwtCacheEntry
}

type jwtCacheEntry struct {
	claims  *TokenClaims
	expires time.Time
}

// WithMintKey attaches the narrow mint credential (LENS_MINT_KEY). Optional: unset ⇒ no mint
// credential exists and the only way to mint stays the global key, i.e. today's behaviour exactly.
// A deployment that never sets it is unchanged by this feature.
func (m *Manager) WithMintKey(k string) *Manager { m.mintKey = k; return m }

// WithOperatorReadKey attaches the narrow operator READ credential (LENS_OPERATOR_READ_KEY).
// Optional: unset ⇒ the credential does not exist, every admin read stays reachable only by the
// global key, and a deployment that never sets it is byte-for-byte unchanged by this feature.
func (m *Manager) WithOperatorReadKey(k string) *Manager { m.operatorReadKey = k; return m }

// sessionKeyValidator is the narrow slice of *sessionkey.Store this package needs. An interface
// rather than the concrete type so auth does not force every consumer of Manager to construct a
// session-key store, and so the nil case ("this deployment mints no session keys") is expressible.
type sessionKeyValidator interface {
	Validate(ctx context.Context, raw string) (*sessionkey.SessionKey, error)
}

// WithSessionKeys attaches the browser-chat session credential store (W4.6.1 step 4). Optional:
// unset ⇒ the credential shape does not exist, every tlv_sk_ bearer is refused, and a deployment
// that never calls this is byte-for-byte unchanged by the feature.
func (m *Manager) WithSessionKeys(v sessionKeyValidator) *Manager { m.sessionKeys = v; return m }

// moderatorKeyValidator is the slice of *moderatorkey.Store this package needs.
type moderatorKeyValidator interface {
	Validate(ctx context.Context, raw string) (*moderatorkey.Key, error)
}

// WithModeratorKeys attaches the marketplace moderator key store (B20.13). Unset ⇒ every tlv_mod_
// bearer is refused.
func (m *Manager) WithModeratorKeys(v moderatorKeyValidator) *Manager { m.moderatorKeys = v; return m }

func NewManager(globalKey string, privateKey *ecdsa.PrivateKey, keyStore *KeyStore, tenantStore *tenant.Store) *Manager {
	var pub *ecdsa.PublicKey
	if privateKey != nil {
		pub = &privateKey.PublicKey
	}
	return &Manager{
		globalKey:   globalKey,
		privateKey:  privateKey,
		publicKey:   pub,
		keyStore:    keyStore,
		tenantStore: tenantStore,
		jwtCache:    map[string]*jwtCacheEntry{},
	}
}

// PrivateKey exposes the EC signing key so the /v1/auth/token
// endpoint can mint new tokens via GenerateToken.
func (m *Manager) PrivateKey() *ecdsa.PrivateKey { return m.privateKey }

// PublicKey exposes the EC verification key so the /v1/auth/jwks
// endpoint can publish it as a JWKS document.
func (m *Manager) PublicKey() *ecdsa.PublicKey { return m.publicKey }

// ─── credential extraction + Authenticate ───────

// extractCredential returns (raw, location). location is purely
// informational — useful for logging the auth method. Empty raw
// means no credential is present in any recognised location.
func extractCredential(r *http.Request) (raw, location string) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer "), "authorization"
	}
	if v := r.Header.Get("X-Talyvor-Key"); v != "" {
		return v, "x-talyvor-key"
	}
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v, "x-api-key"
	}
	return "", ""
}

// Authenticate is the single entry point. Tries each credential
// shape in priority order:
//  1. JWT bearer (looks like a JWT — 3 segments separated by ".")
//  2. Workspace key (starts with tenant.KeyPrefix)
//  3. Global admin key (exact match with LENS_API_KEY)
//  4. Legacy X-API-Key header → treated as global key fallback
//
// Returns ErrMissingCredentials when no credential is present at
// all, and ErrInvalidAuth (deliberately opaque) for every "we
// recognised this but it's wrong" case.
func (m *Manager) Authenticate(r *http.Request) (*AuthContext, error) {
	raw, _ := extractCredential(r)
	if raw == "" {
		return nil, ErrMissingCredentials
	}

	// JWT detection: a JWT has three dot-separated base64 parts
	// — workspace keys never contain dots, so this is unambiguous.
	if strings.Count(raw, ".") == 2 {
		claims, err := m.validateJWTCached(raw)
		if err != nil {
			return nil, ErrInvalidAuth
		}
		actx := &AuthContext{
			WorkspaceID: claims.WorkspaceID,
			UserID:      claims.UserID,
			Scopes:      claims.Scopes,
			AuthMethod:  MethodJWT,
			IsAdmin:     false,
		}
		if claims.ExpiresAt != nil {
			actx.ExpiresAt = claims.ExpiresAt.Time
		}
		return actx, nil
	}

	// Session key (W4.6.1 step 4) — the browser-chat credential.
	//
	// ⚠ THE SCOPE SET IS THIS LITERAL AND NOTHING ELSE. internal/sessionkey has no scopes column,
	// so there is no row, no request body and no caller that can widen it. That is the direct
	// lesson of docs/model2-session-credentials.md, where the neighbouring credential takes its
	// scopes from a request body through a route with no gate — a credential minting a credential
	// carrying scopes it does not itself hold. A session key cannot repeat that shape.
	//
	// ⚠ IsAdmin IS FALSE AND APIKeyID IS EMPTY, both load-bearing: IsAdmin short-circuits HasScope
	// (it would satisfy every gate in the binary), and APIKeyID keys the F4 per-agent LXC
	// sub-budget allocator, which a credential that dies in an hour must not enter.
	//
	// Ordered BEFORE the workspace-key branch only for readability — the prefixes are disjoint, and
	// TestSessionKeyPrefixIsDisjointFromTheWorkspaceKeyPrefix is what keeps that true.
	if m.sessionKeys != nil && strings.HasPrefix(raw, sessionkey.KeyPrefix) {
		sk, err := m.sessionKeys.Validate(r.Context(), raw)
		if errors.Is(err, sessionkey.ErrInvalid) || errors.Is(err, sessionkey.ErrExpired) {
			return nil, ErrInvalidAuth
		}
		if err != nil {
			// B27.5 — the lookup itself failed. Refusing 401 here signed the person out of Chat
			// for a database hiccup; the key may be perfectly good.
			slog.Warn("auth: session key lookup failed", slog.String("err", err.Error()))
			return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
		}
		return &AuthContext{
			WorkspaceID:  sk.WorkspaceID,
			UserID:       sk.UserID,
			Scopes:       []string{ScopeProxy},
			AuthMethod:   MethodSessionKey,
			IsAdmin:      false,
			ExpiresAt:    sk.ExpiresAt,
			SessionKeyID: sk.ID,
		}, nil
	}

	// Moderator key (B20.13). The scope set is this literal: market_moderation and nothing else.
	// IsAdmin is FALSE, so HasScope(ScopeAdmin) is false and no admin gate is satisfied by it; the
	// gates additionally answer it 403 by AuthMethod. Validated against the DB every time, so a
	// revoked key is refused (401) on the next request. The prefix is disjoint from tlv_ws_/tlv_sk_.
	if m.moderatorKeys != nil && strings.HasPrefix(raw, moderatorkey.KeyPrefix) {
		k, err := m.moderatorKeys.Validate(r.Context(), raw)
		if err != nil {
			return nil, ErrInvalidAuth
		}
		return &AuthContext{
			Scopes:         []string{ScopeMarketModeration},
			AuthMethod:     MethodModeratorKey,
			IsAdmin:        false,
			ModeratorKeyID: k.ID,
		}, nil
	}

	// Workspace key — always validate against the DB, never
	// cached, so a revoked key stops working immediately.
	if strings.HasPrefix(raw, tenant.KeyPrefix) {
		if m.tenantStore == nil {
			return nil, ErrInvalidAuth
		}
		key, err := m.tenantStore.ValidateAPIKey(r.Context(), raw)
		if err != nil {
			return nil, ErrInvalidAuth
		}
		return &AuthContext{
			WorkspaceID: key.WorkspaceID,
			Scopes:      key.Scopes,
			AuthMethod:  MethodWorkspaceKey,
			IsAdmin:     false,
			APIKeyID:    key.ID, // F4 C.0: the scoped key's ID (the per-agent LXC sub-budget key). API-key branch ONLY.
		}, nil
	}

	// Global admin key — exact match. Always carries every
	// scope (HasScope shortcircuits via IsAdmin).
	if m.globalKey != "" && raw == m.globalKey {
		return &AuthContext{
			WorkspaceID: "",
			Scopes:      []string{ScopeProxy, ScopeAnalytics, ScopeAdmin, ScopeKeys},
			AuthMethod:  MethodGlobalKey,
			IsAdmin:     true,
		}, nil
	}

	// Narrow mint credential. IsAdmin is FALSE and the scope set is exactly {mint} — it cannot be
	// widened here, and HasScope(ScopeAdmin) is false for it, so every admin gate refuses it.
	//
	// Ordered AFTER the global-key branch on purpose: if an operator sets LENS_MINT_KEY to the
	// same string as LENS_API_KEY, the admin branch wins and behaviour is unchanged rather than
	// silently DEMOTED — a downgrade would break admin, which is worse than the gap this closes.
	if m.mintKey != "" && raw == m.mintKey {
		return &AuthContext{
			WorkspaceID: "",
			Scopes:      []string{ScopeMint},
			AuthMethod:  MethodMintKey,
			IsAdmin:     false,
		}, nil
	}

	// Narrow operator READ credential. IsAdmin is FALSE and the scope set is exactly
	// {operator_read}, so HasScope(ScopeAdmin) is false and every requireAdmin gate refuses it;
	// only the sites that opted into requireAdminOrOperatorRead admit it, and only for GET.
	//
	// ⚠ ORDERED LAST ON PURPOSE, and the two collisions differ:
	//   · same value as LENS_API_KEY  ⇒ the admin branch wins. A silent DEMOTION would take admin
	//     away from a deployment that already worked, which is worse than the gap this closes —
	//     the same argument #402 made for the mint key.
	//   · same value as LENS_MINT_KEY ⇒ the mint branch wins, so the operator reads 401 and the
	//     screen is visibly broken rather than silently over-privileged. Neither key is a superset
	//     of the other, so one of them has to lose; losing loudly on the read side is the safer
	//     half. Both are asserted in cmd/lens/operator_read_credential_test.go.
	if m.operatorReadKey != "" && raw == m.operatorReadKey {
		return &AuthContext{
			WorkspaceID: "",
			Scopes:      []string{ScopeOperatorRead},
			AuthMethod:  MethodOperatorReadKey,
			IsAdmin:     false,
		}, nil
	}

	return nil, ErrInvalidAuth
}

func (m *Manager) validateJWTCached(token string) (*TokenClaims, error) {
	m.mu.RLock()
	if e, ok := m.jwtCache[token]; ok && time.Now().Before(e.expires) {
		m.mu.RUnlock()
		return e.claims, nil
	}
	m.mu.RUnlock()

	claims, err := ValidateToken(token, m.publicKey)
	if err != nil {
		return nil, err
	}
	// The cache entry must never outlive the credential: bounded by
	// min(token exp, cache TTL). Without the bound, a token expiring 1s
	// after first use stayed accepted for up to jwtCacheTTL more — the
	// cache lifetime was silently decoupled from the credential lifetime.
	cacheExp := time.Now().Add(jwtCacheTTL)
	if claims.ExpiresAt != nil && claims.ExpiresAt.Before(cacheExp) {
		cacheExp = claims.ExpiresAt.Time
	}
	m.mu.Lock()
	m.jwtCache[token] = &jwtCacheEntry{claims: claims, expires: cacheExp}
	m.mu.Unlock()
	return claims, nil
}

// ─── middleware ──────────────────────────────────

// authContextCtxKey is the context slot for the resolved
// AuthContext. Use GetAuthContext to read it back.
type authContextCtxKey struct{}

// Middleware returns a chi-compatible handler that requires every
// request to authenticate via Manager.Authenticate. The resolved
// AuthContext lands on the request context.
//
// `onSuccess` and `onFailure` are optional callbacks for the auth
// logging hook described in the spec. main.go wires them to the
// structured slog logger.
func (m *Manager) Middleware(
	onSuccess func(ctx *AuthContext, r *http.Request),
	onFailure func(reason string, r *http.Request),
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCtx, err := m.Authenticate(r)
			if errors.Is(err, ErrAuthUnavailable) {
				if onFailure != nil {
					onFailure(err.Error(), r)
				}
				writeAuthUnavailable(w)
				return
			}
			if err != nil {
				if onFailure != nil {
					onFailure(err.Error(), r)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			if onSuccess != nil {
				onSuccess(authCtx, r)
			}
			ctx := context.WithValue(r.Context(), authContextCtxKey{}, authCtx)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WithAuthContext attaches a resolved AuthContext to ctx — the same slot
// GetAuthContext reads and AuthMiddleware populates internally. Exported so
// handlers and tests can inject identity symmetrically with WithAPIKey (the
// context key type is unexported, so this is the only entry point from outside
// the package).
func WithAuthContext(ctx context.Context, actx *AuthContext) context.Context {
	return context.WithValue(ctx, authContextCtxKey{}, actx)
}

// GetAuthContext returns the resolved AuthContext, or nil when
// the middleware didn't run (or the request was unauthenticated
// public).
func GetAuthContext(ctx context.Context) *AuthContext {
	if v, ok := ctx.Value(authContextCtxKey{}).(*AuthContext); ok {
		return v
	}
	return nil
}

// WorkspaceIdentity resolves the authenticated caller's workspace and whether it
// is the global admin, unifying the two credential slots AuthMiddleware
// populates: the AuthContext (JWT / global key — the only IsAdmin carrier) takes
// precedence, then the APIKey (DB workspace/team keys, never admin). Returns
// ("", false) for an unauthenticated context — fails closed. Handlers in any
// package use this to derive workspace identity from the credential rather than
// caller-supplied input (the #146 cross-tenant authorization fix).
func WorkspaceIdentity(ctx context.Context) (workspaceID string, isAdmin bool) {
	if actx := GetAuthContext(ctx); actx != nil {
		return actx.WorkspaceID, actx.IsAdmin
	}
	if k := GetAPIKey(ctx); k != nil {
		return k.WorkspaceID, false
	}
	return "", false
}

// RequireScope produces a middleware that enforces a single scope. Place it
// AFTER AuthMiddleware — it reads the identity that middleware stamped and 403s
// a credential that presents scopes but not this one.
//
// Two back-compat carve-outs keep existing keys working (they must, or a retrofit
// silently revokes access):
//
//   - An AuthContext with an EMPTY scope set is grandfathered as all-scopes.
//     Every workspace key/JWT minted before enforcement carries an empty set
//     (tenant.ValidateScopes has always accepted empty), so treating empty as
//     "deny" would break every one of them. A key opts into least privilege by
//     being created with an explicit, non-empty scope set.
//   - A request with no AuthContext but a validated fast-path api_keys
//     credential (auth.KeyStore — the APIKey struct has no scope field at all)
//     is grandfathered too. It authenticated; there is nothing to check it
//     against.
//
// Admin is unchanged: the global key sets IsAdmin, which short-circuits HasScope.
// Only a request that reached here with NO credential (i.e. not behind
// AuthMiddleware) is rejected — 401.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ctx := GetAuthContext(r.Context()); ctx != nil {
				// Grandfather empty scopes; HasScope covers admin + an
				// explicit match. A non-empty set missing the scope is denied.
				if len(ctx.Scopes) == 0 || ctx.HasScope(scope) {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"forbidden: missing scope ` + scope + `"}`)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/json; scope is a route constant, never request input
				return
			}
			// No AuthContext ⇒ a fast-path api_keys credential (no scope field);
			// grandfather it. A request with no credential at all still 401s.
			if GetAPIKey(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		})
	}
}

// ClampTTL applies the MaxTokenTTL ceiling — callers can pass a
// requested duration straight from the API body without
// pre-validating it themselves.
func ClampTTL(requested time.Duration) time.Duration {
	if requested <= 0 {
		return DefaultTokenTTL
	}
	if requested > MaxTokenTTL {
		return MaxTokenTTL
	}
	return requested
}
