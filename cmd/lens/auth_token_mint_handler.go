package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/auth"
)

// auth_token_mint_handler.go — the /v1/auth/token mint capability, extracted from an inline
// closure in run() so its admin-scoping invariant is provable at the HTTP layer
// (auth_token_mint_handler_test.go). Behavior is unchanged.
//
// This is the ADMIN-SCOPED minting path (property 1 of the per-workspace-credential fix): only
// the global admin key may mint a JWT bound to an ARBITRARY tenant workspace. Consumers that
// today share the one global key — collapsing every tenant into the empty-workspace bucket
// lens:rl::global:* and the "default" spend bucket — can instead be issued a per-tenant JWT
// here, which AuthMiddleware derives a TRUSTED workspace from (manager.go JWT branch). A
// workspace key/JWT is NOT admin, so it can only mint for itself — enforced by the IsAdmin gate.
func newAuthTokenMintHandler(authManager *auth.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if authManager.PrivateKey() == nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not available")
			return
		}
		// ADMIN **OR** THE NARROW MINT CREDENTIAL. err != nil short-circuits before actx is
		// dereferenced, so a missing/invalid credential is a clean 403 (fail-closed).
		//
		// ⚠ This is the ONLY route that accepts auth.ScopeMint. Every other privileged route gates
		// on actx.IsAdmin (29 requireAdmin call sites; zero use RequireScope(ScopeAdmin)), and the
		// mint credential authenticates with IsAdmin=false — so widening here cannot widen them.
		actx, err := authManager.Authenticate(req)
		if err != nil || actx == nil || (!actx.IsAdmin && !actx.HasScope(auth.ScopeMint)) {
			writeJSONErr(w, http.StatusForbidden, "admin or mint credentials required")
			return
		}
		var in struct {
			WorkspaceID string   `json:"workspace_id"`
			UserID      string   `json:"user_id"`
			Scopes      []string `json:"scopes"`
			TTLHours    int      `json:"ttl_hours"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.WorkspaceID == "" {
			writeJSONErr(w, http.StatusBadRequest, "workspace_id required")
			return
		}
		// ⚠ A MINTER MUST NOT CHOOSE ITS OWN TOKEN'S POWER. The request body carries `scopes`, so
		// without this a mint-scoped credential could mint itself an admin-scoped token and the
		// narrowing would be decorative. An admin caller keeps today's behaviour byte-for-byte
		// (whatever it asks for); a mint-scoped caller gets exactly {proxy} and is REFUSED if it
		// asks for anything, rather than having its request silently rewritten — a caller that
		// asked for something it cannot have should be told, not quietly downgraded.
		scopes := in.Scopes
		if !actx.IsAdmin {
			if len(in.Scopes) > 0 {
				writeJSONErr(w, http.StatusForbidden,
					"this credential may not choose token scopes; omit \"scopes\"")
				return
			}
		}
		if len(scopes) == 0 {
			// {proxy} is what a content service needs: the /v1/proxy/* completion routes. A token
			// with no scopes passes no scope gate at all (B50.33), so it is the default.
			scopes = []string{auth.ScopeProxy}
		}
		ttl := auth.ClampTTL(time.Duration(in.TTLHours) * time.Hour)
		tok, err := auth.GenerateToken(in.WorkspaceID, in.UserID, scopes, authManager.PrivateKey(), ttl)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{
			"token":      tok,
			"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
		})
	}
}
