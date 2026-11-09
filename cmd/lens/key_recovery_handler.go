package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B55.48 — a workspace left with no live key gets one back (economy/key_recovery.go).
//
//	POST /v1/workspaces/{wsID}/keys/recovery-code/challenge — {"challenge", "allow_credentials"}
//	POST /v1/workspaces/{wsID}/keys/recovery-code           {"assertion"} — a new code, signed as any key change; the
//	                                                         old one stops working (201 {"recovery_code"}, shown once)
//	POST /v1/workspaces/{wsID}/keys/recover                 {"code", "passkey" | "device"} — the key at once (201
//	                                                         {"key", "recovery_code"}); 403 for a wrong code, 429 with
//	                                                         Retry-After after five wrong in the hour
//	GET  /v1/workspaces/{wsID}/keys/recover                 — {"recovery"}: the waiting recovery, or null
//	POST /v1/workspaces/{wsID}/keys/recover/start           — a key can be added from 72 hours on (201 {"recovery"})
//	POST /v1/workspaces/{wsID}/keys/recover/cancel          — any owner or admin signed in
//	POST /v1/workspaces/{wsID}/keys/recover/complete        {"passkey" | "device"} — from ready_at, for 24 hours
//
// "passkey" is the body POST …/agents/passkeys takes and "device" the one POST /v1/money/devices takes, each over a
// challenge from its own route. Every recovery route answers 409 while the workspace has a live key. Recover, start and
// complete are the owner's own session's alone (recoveryOwner), signed in within the last 10 minutes. Mounted in the
// authed group.

type keyRecoveryBank interface {
	RecoveryCodeChallenge(ctx context.Context, workspaceID string) (challenge string, credentialIDs []string, err error)
	NewRecoveryCode(ctx context.Context, workspaceID, by string, a *passkey.Assertion) (string, error)
	RecoverWithCode(ctx context.Context, workspaceID, ip, by, code string, k economy.RecoveryKey) (economy.RecoveredKey, error)
	WaitingKeyRecovery(ctx context.Context, workspaceID, userID string) (*economy.KeyRecovery, error)
	StartKeyRecovery(ctx context.Context, workspaceID, userID string) (economy.KeyRecovery, error)
	CancelKeyRecovery(ctx context.Context, workspaceID, by string) (economy.KeyRecovery, error)
	CompleteKeyRecovery(ctx context.Context, workspaceID, userID string, k economy.RecoveryKey) (economy.RecoveredKey, error)
}

// recoverySignInAge is how long ago the owner may have signed in to get a key back.
const recoverySignInAge = 10 * time.Minute

const signInAgainSentence = "Sign in again, then try again."

// recoveryOwner admits only the workspace's owner on their own session (a person's token carrying auth.ScopeKeys for
// this workspace): never the operator, an API key, an agent's key or a Connect token. The BFF sends the session's
// sign-in time as X-Talyvor-Signed-In-At (RFC 3339), and one older than recoverySignInAge, or none, is refused.
func recoveryOwner(next func(w http.ResponseWriter, req *http.Request, userID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actx := auth.GetAuthContext(req.Context())
		if actx == nil || actx.AuthMethod != auth.MethodJWT || actx.UserID == "" || !actx.HasScope(auth.ScopeKeys) ||
			actx.WorkspaceID != chi.URLParam(req, "wsID") {
			writeJSONErr(w, http.StatusForbidden, "Only the workspace's owner, signed in, can get a key back.")
			return
		}
		at, err := time.Parse(time.RFC3339Nano, req.Header.Get("X-Talyvor-Signed-In-At"))
		if age := time.Since(at); err != nil || age > recoverySignInAge || age < -time.Minute {
			writeJSONErr(w, http.StatusForbidden, signInAgainSentence)
			return
		}
		next(w, req, actx.UserID)
	}
}

// recoveryKeyBody is the key a recovery adds, as the passkey and phone key routes take them.
type recoveryKeyBody struct {
	Code    string `json:"code"`
	Passkey *struct {
		CredentialID      string `json:"credential_id"`
		Name              string `json:"name"`
		PublicKey         string `json:"public_key"`
		ClientDataJSON    string `json:"client_data_json"`
		AuthenticatorData string `json:"authenticator_data"`
	} `json:"passkey"`
	Device *struct {
		Platform    string `json:"platform"`
		Name        string `json:"name"`
		PublicKey   string `json:"public_key"`
		Challenge   string `json:"challenge"`
		Signature   string `json:"signature"`
		Attestation string `json:"attestation"`
	} `json:"device"`
}

const recoveryKeyShape = `send one new key: {"passkey": {"credential_id", "name", "public_key", "client_data_json", "authenticator_data"}} ` +
	`or {"device": {"platform", "name", "public_key", "challenge", "signature", "attestation"}}, base64url`

func (b recoveryKeyBody) key(person string) (economy.RecoveryKey, bool) {
	switch {
	case b.Passkey != nil && b.Device == nil:
		p := b.Passkey
		pk, err1 := passkey.Decode64(p.PublicKey)
		cdj, err2 := passkey.Decode64(p.ClientDataJSON)
		ad, err3 := passkey.Decode64(p.AuthenticatorData)
		if p.CredentialID == "" || err1 != nil || err2 != nil || err3 != nil {
			return economy.RecoveryKey{}, false
		}
		return economy.RecoveryKey{Name: p.Name, Passkey: &passkey.Registration{CredentialID: p.CredentialID, PublicKey: pk,
			ClientDataJSON: cdj, AuthenticatorData: ad}}, true
	case b.Device != nil && b.Passkey == nil:
		d := b.Device
		pk, err1 := passkey.Decode64(d.PublicKey)
		att, err2 := passkey.Decode64(d.Attestation)
		if d.Platform == "" || d.Challenge == "" || err1 != nil || err2 != nil {
			return economy.RecoveryKey{}, false
		}
		return economy.RecoveryKey{Name: d.Name, Person: person, Device: &devicekey.Registration{Platform: d.Platform, PublicKey: pk,
			Challenge: d.Challenge, Signature: d.Signature, Attestation: att}}, true
	}
	return economy.RecoveryKey{}, false
}

func mountKeyRecoveryRoutes(r chi.Router, bank keyRecoveryBank) {
	answer := func(w http.ResponseWriter, status int, v any, err error) {
		var limited *economy.RecoveryCodeLimited
		var notReady *economy.RecoveryNotReady
		switch {
		case errors.As(err, &limited):
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(limited.RetryAfter.Seconds()))))
			writeJSONErr(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, economy.ErrHasLiveKey):
			writeJSONErr(w, http.StatusConflict, economy.HasLiveKeySentence)
		case errors.As(err, &notReady), errors.Is(err, economy.ErrNoKeyToRecover), errors.Is(err, economy.ErrRecoveryLockedDown),
			errors.Is(err, economy.ErrRecoveryWaiting), errors.Is(err, economy.ErrPasskeyRegistered):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrRecoveryCodeWrong), errors.Is(err, economy.ErrRecoveryNotYours), errors.Is(err, economy.ErrPasskeyRequired):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, economy.ErrRecoveryNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrRecoveryExpired):
			writeJSONErr(w, http.StatusGone, err.Error())
		case errors.Is(err, passkey.ErrInvalid), errors.Is(err, devicekey.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, status, v)
		}
	}
	who := func(req *http.Request) string { by, _ := storedanswers.OwnerOrAdmin(req.Context()); return by }

	r.Post("/v1/workspaces/{wsID}/keys/recovery-code/challenge", keyOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		c, creds, err := bank.RecoveryCodeChallenge(req.Context(), chi.URLParam(req, "wsID"))
		answer(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"}, err)
	}))
	r.Post("/v1/workspaces/{wsID}/keys/recovery-code", keyOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Assertion *passkey.Assertion `json:"assertion"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		code, err := bank.NewRecoveryCode(req.Context(), chi.URLParam(req, "wsID"), who(req), in.Assertion)
		answer(w, http.StatusCreated, map[string]any{"recovery_code": code}, err)
	}))
	r.Post("/v1/workspaces/{wsID}/keys/recover", recoveryOwner(func(w http.ResponseWriter, req *http.Request, userID string) {
		var in recoveryKeyBody
		_ = json.NewDecoder(req.Body).Decode(&in)
		k, ok := in.key(who(req))
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, recoveryKeyShape)
			return
		}
		out, err := bank.RecoverWithCode(req.Context(), chi.URLParam(req, "wsID"), clientIP(req), who(req), in.Code, k)
		answer(w, http.StatusCreated, out, err)
	}))
	r.Get("/v1/workspaces/{wsID}/keys/recover", func(w http.ResponseWriter, req *http.Request) {
		var userID string
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			userID = actx.UserID
		}
		rec, err := bank.WaitingKeyRecovery(req.Context(), chi.URLParam(req, "wsID"), userID)
		answer(w, http.StatusOK, map[string]any{"recovery": rec}, err)
	})
	r.Post("/v1/workspaces/{wsID}/keys/recover/start", recoveryOwner(func(w http.ResponseWriter, req *http.Request, userID string) {
		rec, err := bank.StartKeyRecovery(req.Context(), chi.URLParam(req, "wsID"), userID)
		answer(w, http.StatusCreated, map[string]any{"recovery": rec}, err)
	}))
	r.Post("/v1/workspaces/{wsID}/keys/recover/cancel", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		// Any owner or admin signed in: the operator or a person's session, never a key.
		if actx := auth.GetAuthContext(req.Context()); !actx.IsAdmin && actx.AuthMethod != auth.MethodJWT {
			writeJSONErr(w, http.StatusForbidden, "Only an owner or an admin, signed in, can cancel a recovery.")
			return
		}
		rec, err := bank.CancelKeyRecovery(req.Context(), chi.URLParam(req, "wsID"), who(req))
		answer(w, http.StatusOK, map[string]any{"recovery": rec}, err)
	}))
	r.Post("/v1/workspaces/{wsID}/keys/recover/complete", recoveryOwner(func(w http.ResponseWriter, req *http.Request, userID string) {
		var in recoveryKeyBody
		_ = json.NewDecoder(req.Body).Decode(&in)
		k, ok := in.key(who(req))
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, recoveryKeyShape)
			return
		}
		out, err := bank.CompleteKeyRecovery(req.Context(), chi.URLParam(req, "wsID"), userID, k)
		answer(w, http.StatusCreated, out, err)
	}))
}
