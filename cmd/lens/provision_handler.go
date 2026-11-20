package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
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
	// B61.7: the accounts one person holds.
	PersonAccounts(ctx context.Context, personKey, firstID string) ([]workspace.PersonAccount, error)
	PersonOwnsAccount(ctx context.Context, personKey, firstID, accountID string) (bool, error)
	ReservePersonAccount(ctx context.Context, personKey, firstID, accountID string) error
	ReleasePersonAccount(ctx context.Context, personKey, accountID string) error
}

// Provision's modes (B61.7). Without one, the call answers the identity's first account, making it on first sight, as
// it always has.
const (
	provisionModeList    = "list"    // the accounts the identity holds
	provisionModeOpen    = "open"    // open another account for the identity, with sign-up's answers
	provisionModeSession = "session" // a session token for one named account of the identity
)

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
	Identity string `json:"identity"`
	// Mode is "", list, open or session (B61.7); AccountID names the account a session is for.
	Mode          string `json:"mode"`
	AccountID     string `json:"account_id"`
	DisplayName   string `json:"display_name"`
	CachePoolable *bool  `json:"cache_poolable"`
	TTLHours      int    `json:"ttl_hours"`
	// B59.6: who the account is for (business or personal) and the money it holds (fiat, stablecoin or hybrid), set
	// on the call that makes the workspace and never by a later one; left out, they are business and hybrid.
	AccountType string `json:"account_type"`
	MoneyKind   string `json:"money_kind"`
	// B59.40: the sign-up profile, set like the type on the call that makes the workspace: an ISO 3166-1 alpha-2
	// country, and a business's registration number (trimmed, at most 64 characters). Profile, not an identity check.
	Country            string `json:"country"`
	RegistrationNumber string `json:"registration_number"`
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
func deriveWorkspaceID(identity string) string { return workspace.FirstAccountID(identity) }

// newAccountID is the id of an account a person opens after their first.
var newAccountID = func() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return workspace.NewAccountID(b), nil
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
		firstID := deriveWorkspaceID(in.Identity)
		person := workspace.PersonKey(in.Identity)
		switch in.Mode {
		case "", provisionModeOpen:
		case provisionModeList:
			accounts, err := p.PersonAccounts(req.Context(), person, firstID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{"accounts": accounts})
			return
		case provisionModeSession:
			if in.AccountID == "" {
				writeJSONErr(w, http.StatusBadRequest, "account_id required")
				return
			}
			owned, err := p.PersonOwnsAccount(req.Context(), person, firstID, in.AccountID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !owned {
				writeJSONErr(w, http.StatusNotFound, "no such account")
				return
			}
			writeProvisioned(w, p, mint, in.AccountID, in.TTLHours, false, nil)
			return
		default:
			writeJSONErr(w, http.StatusBadRequest, "mode must be list, open or session")
			return
		}
		if in.AccountType == "" {
			in.AccountType = economy.AccountTypeBusiness
		}
		if in.MoneyKind == "" {
			in.MoneyKind = economy.MoneyKindHybrid
		}
		if err := economy.CheckAccountType(in.AccountType); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := economy.CheckMoneyKind(in.MoneyKind); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		country, registrationNumber, err := economy.CheckSignupProfile(in.AccountType, in.Country, in.RegistrationNumber)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// B61.53: an account is opened only in a country Talyvor serves. A sign-in to an account that exists makes
		// nothing, so the country it names is not checked and the account keeps the one it has.
		_, existed := p.GetWorkspace(firstID)
		if in.Mode == provisionModeOpen || !existed {
			if err := economy.CheckServedCountry(country); err != nil {
				writeCountryNotServed(w)
				return
			}
		}

		// Open another account (B61.7): a new workspace beside the first, sharing nothing with it but its owner. A person
		// with no account yet opens their first, the one a call without a mode would have made.
		wsID := firstID
		if in.Mode == provisionModeOpen && existed {
			id, err := newAccountID()
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err := p.ReservePersonAccount(req.Context(), person, firstID, id); err != nil {
				if errors.Is(err, workspace.ErrPersonAccountLimit) {
					writeJSONErr(w, http.StatusConflict, err.Error())
					return
				}
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			wsID, existed = id, false
		}
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
		// against the natural always-register implementation. existed is the first account's,
		// looked up above; an account opened after it is new by construction.
		if !existed {
			// Pass a pooling choice ONLY when the caller made one, so silence reaches the
			// manager as silence and the new-workspace default applies. On an existing
			// workspace no choice is passed at all — consent is created once, and only the
			// dedicated setter changes it.
			opts := []workspace.RegisterOption{workspace.WithAccount(in.AccountType, in.MoneyKind),
				workspace.WithSignupProfile(country, registrationNumber)}
			if in.CachePoolable != nil {
				opts = append(opts, workspace.WithCachePoolableChoice(*in.CachePoolable))
			}
			if err := p.RegisterWorkspace(req.Context(), workspace.Workspace{ID: wsID, Name: name}, opts...); err != nil {
				if wsID != firstID {
					_ = p.ReleasePersonAccount(req.Context(), person, wsID)
				}
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

		writeProvisioned(w, p, mint, wsID, in.TTLHours, !existed, first)
	}
}

// writeProvisioned answers a session token for wsID, and for no other workspace.
func writeProvisioned(w http.ResponseWriter, p provisioner, mint tokenMinter, wsID string, ttlHours int, created bool, first *economy.WorkspaceStart) {
	poolable, _ := p.CachePoolableConsent(wsID)
	ttl := auth.ClampTTL(time.Duration(ttlHours) * time.Hour)
	tok, err := mint(wsID, wsID, provisionScopes, ttl)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONOK(w, http.StatusOK, provisionResponse{
		WorkspaceID:   wsID,
		Created:       created,
		CachePoolable: poolable,
		Token:         tok,
		ExpiresAt:     time.Now().Add(ttl).UTC().Format(time.RFC3339),
		FirstAgent:    first,
	})
}
