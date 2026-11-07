package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// connectAgents is the slice of the agent store the Connect mint reads: which agents a workspace has, and whose.
type connectAgents interface {
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
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
		tok, err := auth.GenerateConnectToken(in.WorkspaceID, in.UserID, in.ClientID, in.AgentID, in.Scopes, authManager.PrivateKey(), auth.ConnectTokenTTL)
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

// connectLive is the check a Connect token's payment makes on every call (B54.13).
type connectLive interface {
	ConnectLive(ctx context.Context, workspaceID, agentID, userID, clientID string, issuedAt time.Time) error
}

// connectPayment admits a Connect token's payment from agentID of wsID, or answers the refusal and returns false: its
// own agent only, with payments:send or payments:prepare (B52.41), while the person still owns the agent and has not
// ended the connection, and with an Idempotency-Key. The context it returns files any approval for the assistant, and
// on a payments:prepare token makes every payment wait for one.
func connectPayment(w http.ResponseWriter, req *http.Request, bank connectLive, connect *auth.AuthContext, wsID, agentID string) (context.Context, bool) {
	if connect.WorkspaceID != wsID || connect.ConnectAgentID != agentID ||
		!connect.HasScope(auth.ScopePaymentsSend) && !connect.HasScope(auth.ScopePaymentsPrepare) {
		writeJSONErr(w, http.StatusForbidden, "this connection may not pay from this agent")
		return nil, false
	}
	if err := bank.ConnectLive(req.Context(), wsID, agentID, connect.UserID, connect.ConnectClientID, connect.ConnectIssuedAt); errors.Is(err, economy.ErrConnectEnded) {
		writeJSONErr(w, http.StatusForbidden, err.Error())
		return nil, false
	} else if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if req.Header.Get("Idempotency-Key") == "" { // an assistant that retries without one would pay twice
		writeJSONErr(w, http.StatusBadRequest, "a payment through a connected assistant needs an Idempotency-Key, so a retry pays once")
		return nil, false
	}
	ctx := economy.WithConnectClient(req.Context(), connect.ConnectClientID)
	if connect.HasScope(auth.ScopePaymentsPrepare) { // with both scopes, the narrower one holds
		ctx = economy.WithConnectPrepareOnly(ctx)
	}
	return ctx, true
}

// writeConnectPending answers a Connect payment that waits for the owner's approval: which approval, and that it waits.
func writeConnectPending(w http.ResponseWriter, need *economy.ApprovalNeededError) {
	writeJSONOK(w, http.StatusAccepted, map[string]any{"status": "pending_approval", "approval_id": need.ApprovalID,
		"amount_ulxc": need.AmountULXC, "message": need.Error()})
}
