package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// provisionSecretHeader carries the shared provisioning secret.
const provisionSecretHeader = "X-Gateway-Auth"

// provisioner is the narrow slice of *workspace.Manager the route needs.
type provisioner interface {
	GetWorkspace(id string) (*workspace.Workspace, bool)
	RegisterWorkspace(ctx context.Context, ws workspace.Workspace, opts ...workspace.RegisterOption) error
	CachePoolableConsent(wsID string) (poolable bool, known bool)
}

// workspaceStarter gives a new workspace its test money and first agent (B45.7): started is false, and
// nothing changes, when it already has an agent.
type workspaceStarter func(ctx context.Context, workspaceID, ownerUserID string) (out economy.WorkspaceStart, started bool, err error)

// newWorkspaceStarter starts a workspace in bank with testMoneyULXC of test money and a first agent whose proxy key
// is minted in the same transaction.
func newWorkspaceStarter(bank *economy.DualTokenStore, testMoneyULXC int64) workspaceStarter {
	return newWorkspaceStarterNamed(bank, economy.FirstAgentName, nil, testMoneyULXC)
}

// newWorkspaceStarterNamed is newWorkspaceStarter with the agent called name and its rules changed by adjust (B45.33).
func newWorkspaceStarterNamed(bank *economy.DualTokenStore, name string, adjust func(*economy.AgentRules), testMoneyULXC int64) workspaceStarter {
	return func(ctx context.Context, workspaceID, ownerUserID string) (economy.WorkspaceStart, bool, error) {
		return bank.StartWorkspaceAgent(ctx, workspaceID, ownerUserID, name, adjust, testMoneyULXC,
			func(ctx context.Context, tx pgx.Tx, workspaceID, name string) (string, string, error) {
				raw, key, err := tenant.CreateAPIKeyTx(ctx, tx, workspaceID, name, []string{"proxy"}, nil)
				if err != nil {
					return "", "", err
				}
				return raw, key.ID, nil
			})
	}
}

// tokenMinter mints the workspace-scoped session JWT.
type tokenMinter func(workspaceID, userID string, scopes []string, ttl time.Duration) (string, error)

// provisionScopes is what a provisioned session token may do.
var provisionScopes = []string{auth.ScopeAnalytics, auth.ScopeKeys}

type provisionRequest struct {
	Identity      string `json:"identity"`
	DisplayName   string `json:"display_name"`
	CachePoolable *bool  `json:"cache_poolable"`
	TTLHours      int    `json:"ttl_hours"`
}

type provisionResponse struct {
	WorkspaceID   string `json:"workspace_id"`
	Created       bool   `json:"created"`
	CachePoolable bool   `json:"cache_poolable"`
	Token         string `json:"token"`
	ExpiresAt     string `json:"expires_at"`
	// FirstAgent is the agent the new workspace starts with, its key shown this once (B45.7); absent on a
	// workspace that already existed.
	FirstAgent *economy.WorkspaceStart `json:"first_agent,omitempty"`
}

// deriveWorkspaceID turns an opaque identity into this deployment's workspace id.
func deriveWorkspaceID(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "u" + strings.ToLower(enc)[:26]
}

func mountProvisionRoute(r chi.Router, secret string, p provisioner, start workspaceStarter, mint tokenMinter) {
	if secret == "" {
		return
	}
	r.Post("/v1/provision", newProvisionHandler(secret, p, start, mint))
}

func newProvisionHandler(secret string, p provisioner, start workspaceStarter, mint tokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(provisionSecretHeader)), []byte(secret)) != 1 {
			writeJSONErr(w, http.StatusUnauthorized, "provisioning credentials required")
			return
		}
		var in provisionRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.Identity == "" {
			writeJSONErr(w, http.StatusBadRequest, "identity required")
			return
		}

		wsID := deriveWorkspaceID(in.Identity)
		name := in.DisplayName
		if name == "" {
			name = wsID
		}

		// CHECK-THEN-CREATE — not an upsert, and not a style preference.
		//
		// insertWorkspaceSQL's ON CONFLICT (id) DO UPDATE rewrites name, cache_prefix,
		// spend_limit_usd, both allowlists, max_tokens_*, active, logging_policy and
		// distill_policy from the request body. Only the THREE CONSENT columns are
		// deliberately preserved. This body carries none of those settings, so calling
		// RegisterWorkspace on an EXISTING workspace would zero a paying tenant's spend cap
		// and revert their logging_policy to the metadata default on their next login — the
		// #129 regression shape, now reachable on every single sign-in rather than a restart.
		//
		// So: register ONLY on first sight. Afterwards, mint and return created:false.
		// TestProvision_SecondCallPreservesNonConsentSettings is the lock, and it fails
		// against the natural always-register implementation.
		_, existed := p.GetWorkspace(wsID)
		if !existed {
			// Pass a pooling choice ONLY when the caller made one, so silence reaches the
			// manager as silence and the new-workspace default applies. On an existing
			// workspace no choice is passed at all — consent is created once, and only the
			// dedicated setter changes it.
			var opts []workspace.RegisterOption
			if in.CachePoolable != nil {
				opts = append(opts, workspace.WithCachePoolableChoice(*in.CachePoolable))
			}
			if err := p.RegisterWorkspace(req.Context(), workspace.Workspace{ID: wsID, Name: name}, opts...); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		// B45.7: the first agent is owned by the person the session token names (wsID, below). Asked on
		// first sight only — an existing workspace is never given test money or an agent on a later login —
		// and a first sight that raced another gets started=false and no second agent.
		var first *economy.WorkspaceStart
		if !existed && start != nil {
			out, started, err := start(req.Context(), wsID, wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "workspace created, but its first agent was not: "+err.Error())
				return
			}
			if started {
				first = &out
			}
		}

		poolable, _ := p.CachePoolableConsent(wsID)
		ttl := auth.ClampTTL(time.Duration(in.TTLHours) * time.Hour)
		tok, err := mint(wsID, wsID, provisionScopes, ttl)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, provisionResponse{
			WorkspaceID:   wsID,
			Created:       !existed,
			CachePoolable: poolable,
			Token:         tok,
			ExpiresAt:     time.Now().Add(ttl).UTC().Format(time.RFC3339),
			FirstAgent:    first,
		})
	}
}
