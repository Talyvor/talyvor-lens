package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B40.4 — device keys: the phone app's hardware key, which signs approvals and mandates as a passkey does
// (economy/device_keys.go, internal/devicekey).
//
//	POST /v1/money/devices/challenge     — {"challenge"}: the phone signs devicekey.Message(challenge) and binds its
//	                                       attestation to devicekey.Binding(challenge, public key)
//	POST /v1/money/devices               {"platform": "ios"|"android", "name", "public_key", "challenge",
//	                                      "signature", "attestation"}, base64url — keeps the key (201)
//	GET  /v1/money/devices               — the workspace's device keys, revoked ones included
//	POST /v1/money/devices/{id}/revoke   — revoked at once: its signatures are refused from now on
//
// A device key then signs wherever an "assertion" is taken for an approval or a mandate:
// {"device_id", "challenge", "signature"}. Every route is the owner's or an admin's. Mounted in the authed group.

type deviceKeyBank interface {
	DeviceRegistrationChallenge(ctx context.Context, workspaceID string) (string, error)
	RegisterDeviceKey(ctx context.Context, workspaceID, person, name string, r devicekey.Registration) (economy.DeviceKey, error)
	ListDeviceKeys(ctx context.Context, workspaceID string) ([]economy.DeviceKey, error)
	RevokeDeviceKey(ctx context.Context, workspaceID, id, by string) (economy.DeviceKey, error)
}

func mountDeviceKeyRoutes(r chi.Router, bank deviceKeyBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (ws, who string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		who, isOwner := storedanswers.OwnerOrAdmin(req.Context())
		if ws == "" || !isOwner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may register, list or revoke its device keys")
			return "", "", false
		}
		return ws, who, true
	}
	r.Post("/v1/money/devices/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		c, err := bank.DeviceRegistrationChallenge(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c})
	})
	r.Post("/v1/money/devices", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Platform    string `json:"platform"`
			Name        string `json:"name"`
			PublicKey   string `json:"public_key"`
			Challenge   string `json:"challenge"`
			Signature   string `json:"signature"`
			Attestation string `json:"attestation"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		pk, err1 := passkey.Decode64(in.PublicKey)
		att, err2 := passkey.Decode64(in.Attestation)
		if in.Platform == "" || in.Challenge == "" || err1 != nil || err2 != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"platform", "name", "public_key", "challenge", "signature", "attestation"}, base64url`)
			return
		}
		d, err := bank.RegisterDeviceKey(req.Context(), ws, who, in.Name, devicekey.Registration{Platform: in.Platform,
			PublicKey: pk, Challenge: in.Challenge, Signature: in.Signature, Attestation: att})
		switch {
		case errors.Is(err, devicekey.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, d)
		}
	})
	r.Get("/v1/money/devices", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		list, err := bank.ListDeviceKeys(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"devices": list})
	})
	r.Post("/v1/money/devices/{id}/revoke", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := owner(w, req)
		if !ok {
			return
		}
		d, err := bank.RevokeDeviceKey(req.Context(), ws, chi.URLParam(req, "id"), who)
		switch {
		case errors.Is(err, economy.ErrDeviceKeyNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, d)
		}
	})
}
