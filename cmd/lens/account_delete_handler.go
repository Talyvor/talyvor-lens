package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"

	"github.com/talyvor/lens/internal/workspace"
)

// B52.37 — a person deletes their account. The suite BFF calls this for one identity on the provision secret,
// as it calls POST /v1/provision; Lens deletes that identity's workspace (workspace.Manager.DeleteAccount), and,
// before that commits, what it has outside Postgres: its subscription in Stripe and its cached answers. Then every
// replica drops the workspace's cached keys.

type accountDeleter interface {
	DeleteAccount(ctx context.Context, id string, beforeCommit func(context.Context) error) error
	AccountDeletedAt(ctx context.Context, id string) (time.Time, bool, error)
}

type accountDeleteDeps struct {
	accounts accountDeleter
	answers  syntheticAnswers  // the cached answers, in Redis too
	keys     func(wsID string) // drops every replica's cached keys of the workspace
	// chain is each smart account of the workspace's agents that holds anything now, read on the chain
	// (economy.DualTokenStore.SmartAccountsHolding).
	chain func(ctx context.Context, wsID string) ([]string, error)
	// billing cancels the workspace's subscriptions in Stripe (billing.Service.CancelForAccountDeletion).
	billing func(ctx context.Context, wsID string) error
}

// Its 503s are written with writeJSONOK: each says, in plain words and never with the cause, why the deletion waits,
// which writeJSONErr's 5xx redaction would replace with "internal server error".

// accountDeleteStepError is a step outside Postgres that failed: the caller is told what, in words; the log gets err.
type accountDeleteStepError struct {
	say string
	err error
}

func (e *accountDeleteStepError) Error() string { return e.say + ": " + e.err.Error() }

// mountAccountDeleteRoute registers POST /v1/account/delete beside /v1/provision. Unset secret ⇒ not registered.
func mountAccountDeleteRoute(r chi.Router, secret string, d accountDeleteDeps) {
	if secret == "" {
		return
	}
	r.Post("/v1/account/delete", newAccountDeleteHandler(secret, d))
}

func newAccountDeleteHandler(secret string, d accountDeleteDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(provisionSecretHeader)), []byte(secret)) != 1 {
			writeJSONErr(w, http.StatusUnauthorized, "provisioning credentials required")
			return
		}
		var in struct {
			Identity string `json:"identity"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Identity == "" {
			writeJSONErr(w, http.StatusBadRequest, "identity required")
			return
		}
		wsID, err := currentWorkspaceID(req.Context(), in.Identity, d.accounts)
		if err != nil {
			slog.Error("account deletion: account deletions not read", "err", err)
			writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": "your account can't be deleted right now. Try again later."})
			return
		}
		if d.chain != nil {
			// shortcut: read before the deletion's transaction, as the chain cannot be locked; anything sent in between
			// is left in an account whose key is gone. Re-read after the commit and alert the operator if that is ever seen.
			switch held, err := d.chain(req.Context(), wsID); {
			case err != nil:
				slog.Error("account deletion: smart accounts not read", "workspace", wsID, "err", err)
				writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": "your agents' smart accounts can't be read right now, so your account can't be deleted yet. Try again later."})
				return
			case len(held) > 0:
				writeJSONOK(w, http.StatusConflict, map[string]any{
					"error": "your agents' smart accounts still hold money or tokens (" + joinWords(held) + "), so your account can't be deleted yet. " +
						"Sweep each one to your own wallet first (the agent's Sweep, signed with your passkey), then try again.",
					"held": []string{"money or tokens in its agents' smart accounts"}, "smart_accounts": held,
				})
				return
			}
		}
		outside := func(ctx context.Context) error {
			if d.billing != nil {
				if err := d.billing(ctx, wsID); err != nil {
					return &accountDeleteStepError{"your subscription couldn't be cancelled right now, so your account can't be deleted yet. Try again later.", err}
				}
			}
			if d.answers != nil {
				if _, err := d.answers.DeleteAllOf(ctx, []string{wsID}); err != nil {
					return &accountDeleteStepError{"your saved answers couldn't be removed right now, so your account can't be deleted yet. Try again later.", err}
				}
			}
			return nil
		}
		var holds *workspace.AccountHoldsMoneyError
		var step *accountDeleteStepError
		switch err := d.accounts.DeleteAccount(req.Context(), wsID, outside); {
		case errors.Is(err, workspace.ErrNoAccount):
			writeJSONErr(w, http.StatusNotFound, "there is no account for this identity")
			return
		case errors.As(err, &holds):
			writeJSONOK(w, http.StatusConflict, map[string]any{
				"error": "your account still holds " + joinWords(holds.Held) +
					", so it can't be deleted yet. Move the money out and finish or cancel what is open, then try again.",
				"held": holds.Held,
			})
			return
		case errors.As(err, &step):
			slog.Error("account deletion: refused", "workspace", wsID, "err", err)
			writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": step.say})
			return
		case err != nil:
			slog.Error("account deletion: failed", "workspace", wsID, "err", err)
			writeJSONErr(w, http.StatusInternalServerError, "your account couldn't be deleted. Nothing was deleted; try again later.")
			return
		}
		if d.keys != nil {
			d.keys(wsID)
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"deleted": true, "workspace_id": wsID})
	}
}

// accountDeletedSubject carries a deleted account's workspace id to every replica, which drops its cached keys.
const accountDeletedSubject = "lens.accounts.deleted"

// accountDeletedPublisher is the slice of *nats.Conn that tells the other replicas.
type accountDeletedPublisher interface {
	Publish(subject string, data []byte) error
}

// evictEverywhere drops the workspace's cached keys here, then tells every replica subscribed by
// subscribeAccountDeletions to drop theirs.
func evictEverywhere(evict func(string), nc accountDeletedPublisher) func(string) {
	return func(wsID string) {
		evict(wsID)
		if err := nc.Publish(accountDeletedSubject, []byte(wsID)); err != nil {
			// The other replicas' cached keys of the deleted account then live out their 5-minute TTL.
			slog.Error("account deletion: other replicas not told", "workspace", wsID, "err", err)
		}
	}
}

// joinWords is "a", "a and b", "a, b and c".
func joinWords(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	out := s[0]
	for _, w := range s[1 : len(s)-1] {
		out += ", " + w
	}
	return out + " and " + s[len(s)-1]
}

// subscribeAccountDeletions drops this replica's cached keys of every account another replica deletes.
func subscribeAccountDeletions(nc *nats.Conn, evict func(string)) error {
	_, err := nc.Subscribe(accountDeletedSubject, func(m *nats.Msg) { evict(string(m.Data)) })
	return err
}
