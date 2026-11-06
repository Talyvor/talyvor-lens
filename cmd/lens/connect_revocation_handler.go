package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/talyvor/lens/internal/auth"
)

// connectRevoker is the slice of the agent store a revocation writes to.
type connectRevoker interface {
	RevokeConnect(ctx context.Context, userID, clientID string) error
}

// newConnectRevocationHandler is POST /v1/auth/connect-revocations (B54.13): the BFF, holding LENS_CONNECT_MINT_KEY,
// records that a person disconnected an assistant (client_id), or every assistant (no client_id) when they sign out
// everywhere. Every Connect token minted until then stops paying at once, without waiting out its fifteen minutes.
func newConnectRevocationHandler(authManager *auth.Manager, store connectRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actx, err := authManager.Authenticate(req)
		if err != nil || actx == nil || !actx.HasScope(auth.ScopeConnectMint) { // the global key passes HasScope too
			writeJSONErr(w, http.StatusForbidden, "admin or Connect mint credentials required")
			return
		}
		var in struct {
			UserID   string `json:"user_id"`
			ClientID string `json:"client_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.UserID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"user_id": "<person>", "client_id": "<assistant, or empty for every one>"}`)
			return
		}
		if err := store.RevokeConnect(req.Context(), in.UserID, in.ClientID); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{"user_id": in.UserID, "client_id": in.ClientID, "revoked": true})
	}
}
