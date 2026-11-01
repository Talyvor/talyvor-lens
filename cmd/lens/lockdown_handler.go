package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B41.116 — lost-phone lockdown (economy/lockdown.go), from any signed-in device.
//
//	GET  /v1/money/lockdown       — {"active", "started_at", "started_by", "reason"}
//	POST /v1/money/lockdown       {"device_ids", "passkey_ids", "reason"} — revokes the lost phone's device keys and
//	                                passkeys (every one of them when none is named), freezes every card and pauses every
//	                                agent, at once and with no signature; nothing restarts until it is lifted
//	POST /v1/money/lockdown/lift  — lifts it once an identity check (POST /v1/workspaces/{wsID}/verification/identity) has passed
//	                                since it began; 403 until then. Agents and cards then restart one by one, as before.
//
// Every route is the owner's or an admin's (the device key routes' rule). Mounted in the authed group.

type lockdownBank interface {
	LockDown(ctx context.Context, workspaceID, by, reason string, deviceIDs, passkeyIDs []string) (economy.Lockdown, error)
	LockdownStatus(ctx context.Context, workspaceID string) (economy.Lockdown, error)
	LiftLockdown(ctx context.Context, workspaceID string) error
}

func mountLockdownRoutes(r chi.Router, bank lockdownBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (ws, who string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		who, isOwner := storedanswers.OwnerOrAdmin(req.Context())
		if ws == "" || !isOwner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may lock it down or lift a lockdown")
			return "", "", false
		}
		return ws, who, true
	}
	r.Get("/v1/money/lockdown", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		l, err := bank.LockdownStatus(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})
	r.Post("/v1/money/lockdown", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			DeviceIDs  []string `json:"device_ids"`
			PasskeyIDs []string `json:"passkey_ids"`
			Reason     string   `json:"reason"`
		}
		if req.ContentLength != 0 && json.NewDecoder(req.Body).Decode(&in) != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"device_ids", "passkey_ids", "reason"}, each optional`)
			return
		}
		l, err := bank.LockDown(req.Context(), ws, who, in.Reason, in.DeviceIDs, in.PasskeyIDs)
		switch {
		case errors.Is(err, economy.ErrDeviceKeyNotFound), errors.Is(err, economy.ErrPasskeyNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, l)
		}
	})
	r.Post("/v1/money/lockdown/lift", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		err := bank.LiftLockdown(req.Context(), ws)
		switch {
		case errors.Is(err, economy.ErrIdentityRecheck):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"active": false})
		}
	})
}
