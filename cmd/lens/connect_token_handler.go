package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// connectAgents is the slice of the agent store the Connect mint reads: which agents a workspace has, and whose, and
// the database's clock a token's issued-at is stamped from.
type connectAgents interface {
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
	ConnectClock(ctx context.Context) (time.Time, error)
}

// newConnectTokenMintHandler is POST /v1/auth/connect-token (B52.4): the BFF, holding LENS_CONNECT_MINT_KEY, mints the
// token an assistant connected through Claude or ChatGPT calls Lens with — one agent, the scopes the person granted
// (auth.ConnectScopes at most), fifteen minutes, and only for an agent of that workspace the person owns. What the
// token may then do is decided in auth, not here.
func newConnectTokenMintHandler(authManager *auth.Manager, agents connectAgents) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if authManager.PrivateKey() == nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not available")
			return
		}
		actx, err := authManager.Authenticate(req)
		if err != nil || actx == nil || !actx.HasScope(auth.ScopeConnectMint) { // the global key passes HasScope too
			writeJSONErr(w, http.StatusForbidden, "admin or Connect mint credentials required")
			return
		}
		var in struct {
			WorkspaceID string   `json:"workspace_id"`
			UserID      string   `json:"user_id"`
			ClientID    string   `json:"client_id"`
			AgentID     string   `json:"agent_id"`
			Scopes      []string `json:"scopes"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.WorkspaceID == "" || in.UserID == "" || in.AgentID == "" {
			writeJSONErr(w, http.StatusBadRequest, "workspace_id, user_id and agent_id are required")
			return
		}
		book, err := agents.AgentBook(req.Context(), in.WorkspaceID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !slices.ContainsFunc(book.Agents, func(a economy.Agent) bool { return a.ID == in.AgentID && a.OwnerUserID == in.UserID }) {
			writeJSONErr(w, http.StatusForbidden, "the agent is not one of this workspace's that the person owns")
			return
		}
		issuedAt, err := agents.ConnectClock(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		tok, err := auth.GenerateConnectToken(in.WorkspaceID, in.UserID, in.ClientID, in.AgentID, in.Scopes, authManager.PrivateKey(), auth.ConnectTokenTTL, issuedAt)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{
			"token":      tok,
			"expires_at": time.Now().Add(auth.ConnectTokenTTL).UTC().Format(time.RFC3339),
		})
	}
}
