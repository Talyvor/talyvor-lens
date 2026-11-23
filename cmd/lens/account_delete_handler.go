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
// before that commits, what it has outside Postgres: its cached answers, then its subscription in Stripe. Then every
// replica drops the workspace's cached keys.

type accountDeleter interface {
	PersonAccounts(ctx context.Context, personKey, firstID string) ([]workspace.PersonAccount, error)
	AccountHoldings(ctx context.Context, id string) ([]string, error)
	DeleteAccount(ctx context.Context, id, identity string, beforeCommit func(context.Context) error) error
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
		firstID, err := currentWorkspaceID(req.Context(), in.Identity, d.accounts)
		if err != nil {
			slog.Error("account deletion: account deletions not read", "err", err)
			writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": "your account can't be deleted right now. Try again later."})
			return
		}
		// A person may hold several accounts (B61.7): all of them go, the first last, so a deletion stopped part-way
		// is finished by asking again.
		held, err := d.accounts.PersonAccounts(req.Context(), workspace.PersonKey(in.Identity), firstID)
		if err != nil {
			slog.Error("account deletion: the person's accounts not read", "err", err)
			writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": "your account can't be deleted right now. Try again later."})
			return
		}
		var ids []string
		for _, a := range held {
			if a.ID != firstID {
				ids = append(ids, a.ID)
			}
		}
		for _, a := range held {
			if a.ID == firstID {
				ids = append(ids, a.ID)
			}
		}
		if len(ids) == 0 {
			writeJSONErr(w, http.StatusNotFound, "there is no account for this identity")
			return
		}
		names := make(map[string]string, len(held))
		for _, a := range held {
			names[a.ID] = a.Name
		}
		// Each account is checked before any is deleted; DeleteAccount checks its own again under its lock.
		for _, wsID := range ids {
			if d.chain != nil {
				// shortcut: read before the deletion's transaction, as the chain cannot be locked; anything sent in between
				// is left in an account whose key is gone. Re-read after the commit and alert the operator if that is ever seen.
				switch smart, err := d.chain(req.Context(), wsID); {
				case err != nil:
					slog.Error("account deletion: smart accounts not read", "workspace", wsID, "err", err)
					writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": "your agents' smart accounts can't be read right now, so your account can't be deleted yet. Try again later."})
					return
				case len(smart) > 0:
					writeJSONOK(w, http.StatusConflict, map[string]any{
						"error": "your agents' smart accounts still hold money or tokens (" + joinWords(smart) + "), so your account can't be deleted yet. " +
							"Sweep each one to your own wallet first (the agent's Sweep, signed with your passkey), then try again.",
						"held": []string{"money or tokens in its agents' smart accounts"}, "smart_accounts": smart, "account_id": wsID,
					})
					return
				}
			}
			switch money, err := d.accounts.AccountHoldings(req.Context(), wsID); {
			case err != nil:
				slog.Error("account deletion: holdings not read", "workspace", wsID, "err", err)
				writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": "your account can't be deleted right now. Try again later."})
				return
			case len(money) > 0:
				writeAccountHoldsMoney(w, names[wsID], len(ids), wsID, money)
				return
			}
		}
		for i, wsID := range ids {
			// Stripe goes last: it cannot be undone, so nothing that can still fail comes after it but the commit.
			outside := func(ctx context.Context) error {
				if d.answers != nil {
					if _, err := d.answers.DeleteAllOf(ctx, []string{wsID}); err != nil {
						return &accountDeleteStepError{"your saved answers couldn't be removed right now, so your account can't be deleted yet. Try again later.", err}
					}
				}
				if d.billing != nil {
					if err := d.billing(ctx, wsID); err != nil {
						return &accountDeleteStepError{"your subscription couldn't be cancelled right now, so your account can't be deleted yet. Try again later.", err}
					}
				}
				return nil
			}
			var holds *workspace.AccountHoldsMoneyError
			var step *accountDeleteStepError
			// The person's memberships of other workspaces end with their last account, in its transaction.
			person := ""
			if i == len(ids)-1 {
				person = in.Identity
			}
			switch err := d.accounts.DeleteAccount(req.Context(), wsID, person, outside); {
			case errors.Is(err, workspace.ErrNoAccount):
				continue // deleted by a deletion running beside this one
			case errors.As(err, &holds):
				writeAccountHoldsMoney(w, names[wsID], len(ids), wsID, holds.Held)
				return
			case errors.As(err, &step):
				slog.Error("account deletion: refused", "workspace", wsID, "err", err)
				writeJSONOK(w, http.StatusServiceUnavailable, map[string]string{"error": step.say})
				return
			case err != nil:
				slog.Error("account deletion: failed", "workspace", wsID, "err", err, "accounts_deleted_before", i)
				say := "your account couldn't be deleted. Nothing was deleted; try again later."
				if i > 0 {
					say = "not all of your accounts could be deleted. Try again later to delete the rest."
				}
				writeJSONErr(w, http.StatusInternalServerError, say)
				return
			}
			if d.keys != nil {
				d.keys(wsID)
			}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"deleted": true, "workspace_id": firstID, "workspace_ids": ids})
	}
}

// writeAccountHoldsMoney refuses a deletion because account wsID, named name and one of the of accounts the person holds, still
// holds money or an open money item.
func writeAccountHoldsMoney(w http.ResponseWriter, name string, of int, wsID string, held []string) {
	which := "your account"
	if of > 1 {
		which = "your account " + name
	}
	writeJSONOK(w, http.StatusConflict, map[string]any{
		"error": which + " still holds " + joinWords(held) +
			", so it can't be deleted yet. Move the money out and finish or cancel what is open, then try again.",
		"held": held, "account_id": wsID,
	})
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
