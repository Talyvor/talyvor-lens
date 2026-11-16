package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/phonesignin"
	"github.com/talyvor/lens/internal/workspace"
)

// B61.24 — a passkey signs its person in, for the suite BFF alone on the provision secret, beside the phone door. The
// BFF calls the key routes only for the person whose session it holds.

// keySignIn is the narrow slice of *phonesignin.Store the key routes need.
type keySignIn interface {
	SignInChallenge(ctx context.Context, origin, personID string) (phonesignin.KeyChallenge, error)
	SignInWithKey(ctx context.Context, a passkey.Assertion, address string) (phonesignin.KeySignIn, error)
	RegisterChallenge(ctx context.Context, personID, challenge string) (phonesignin.KeyChallenge, error)
	AddKey(ctx context.Context, personID string, k phonesignin.NewKey, proof phonesignin.KeyProof, address string) (phonesignin.PersonKey, error)
	ListKeys(ctx context.Context, personID string) ([]phonesignin.PersonKey, error)
	RemoveKey(ctx context.Context, personID, keyID string, proof phonesignin.KeyProof, address string) error
	PersonIdentity(ctx context.Context, personID string) (string, error)
	PhoneCheckDue(p phonesignin.Person) bool
}

// approvalKeyChallenges issues a workspace's approval-key registration challenges (economy.DualTokenStore), so the
// one ceremony at sign-up's key step makes the account's approval key and the person's sign-in key.
type approvalKeyChallenges interface {
	PasskeyRegistrationChallenge(ctx context.Context, workspaceID string) (challenge, rpID string, err error)
	DeviceRegistrationChallenge(ctx context.Context, workspaceID string) (string, error)
}

// personAccounts says whether an account is one of a person's (workspace.Manager, B61.7).
type personAccounts interface {
	PersonOwnsAccount(ctx context.Context, personKey, firstID, accountID string) (bool, error)
}

type keyChallengeRequest struct {
	// Origin is where the web asks from; "" is the phone app, whose device key answers.
	Origin   string `json:"origin"`
	PersonID string `json:"person_id"`
}

type keyFinishRequest struct {
	passkey.Assertion
	Address string `json:"caller_address"`
}

type keyFinishResponse struct {
	Person        phonesignin.Person `json:"person"`
	KeyID         string             `json:"key_id"`
	Ticket        string             `json:"ticket"`
	ExpiresAt     string             `json:"expires_at"`
	PhoneCheckDue bool               `json:"phone_check_due"`
}

type keyRegisterChallengeRequest struct {
	PersonID string `json:"person_id"`
	// WorkspaceID, when set, is the person's account whose approval key the same ceremony makes; Kind says which
	// (passkey, the default, or device).
	WorkspaceID string `json:"workspace_id"`
	Kind        string `json:"kind"`
}

type keyRegisterRequest struct {
	PersonID string `json:"person_id"`
	Name     string `json:"name"`
	Passkey  *struct {
		CredentialID      string `json:"credential_id"`
		PublicKey         string `json:"public_key"`
		ClientDataJSON    string `json:"client_data_json"`
		AuthenticatorData string `json:"authenticator_data"`
	} `json:"passkey"`
	Device *struct {
		Platform    string `json:"platform"`
		PublicKey   string `json:"public_key"`
		Challenge   string `json:"challenge"`
		Signature   string `json:"signature"`
		Attestation string `json:"attestation"`
	} `json:"device"`
	// KeyTicket is a ticket from signing in with a key the person has; Passcode is theirs (B61.23). Either proves a
	// new key once the person has a key or a passcode.
	KeyTicket string `json:"key_ticket"`
	Passcode  string `json:"passcode"`
	Address   string `json:"caller_address"`
}

type keyRemoveRequest struct {
	PersonID string `json:"person_id"`
	KeyID    string `json:"key_id"`
	// KeyTicket and Passcode prove it is the person, as they do for a new key.
	KeyTicket string `json:"key_ticket"`
	Passcode  string `json:"passcode"`
	Address   string `json:"caller_address"`
}

// mountKeySignInRoutes registers the key routes on the bare router, as mountPhoneSignInRoutes does. Unset secret ⇒
// not registered.
func mountKeySignInRoutes(r chi.Router, secret string, s keySignIn, approval approvalKeyChallenges, accounts personAccounts) {
	if secret == "" {
		return
	}
	r.Post("/v1/auth/passkey/challenge", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { keyChallenge(w, req, s) }))
	r.Post("/v1/auth/passkey/finish", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { keyFinish(w, req, s) }))
	r.Post("/v1/auth/passkey/register/challenge", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) {
		keyRegisterChallenge(w, req, s, approval, accounts)
	}))
	r.Post("/v1/auth/passkey/register", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { keyRegister(w, req, s) }))
	r.Post("/v1/auth/passkey/keys", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { keyList(w, req, s) }))
	r.Post("/v1/auth/passkey/keys/remove", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { keyRemove(w, req, s) }))
}

// writeKeyRefusal answers a key refused, in words; false when err is none of the key rules'.
func writeKeyRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, phonesignin.ErrKeysOff):
		writeJSONErr(w, http.StatusServiceUnavailable, "sign-in keys are not set up on this server")
	case errors.Is(err, phonesignin.ErrKeyChallenge):
		writePhoneRefusal(w, http.StatusBadRequest, "challenge_ended", "That sign-in has ended. Try again.")
	case errors.Is(err, phonesignin.ErrKeyOrigin):
		writePhoneRefusal(w, http.StatusBadRequest, "origin", "That sign-in came from somewhere it may not. Try again from the app.")
	case errors.Is(err, phonesignin.ErrNoKey):
		writePhoneRefusal(w, http.StatusUnauthorized, "no_key", "That key does not sign anyone in. Use your phone number instead.")
	case errors.Is(err, phonesignin.ErrKeyRefused):
		writePhoneRefusal(w, http.StatusUnauthorized, "key_refused", "That key could not be checked. Use Face ID, your fingerprint or your device's PIN, and try again.")
	case errors.Is(err, phonesignin.ErrKeyKept):
		writePhoneRefusal(w, http.StatusConflict, "key_kept", "That key is already kept, or was removed and cannot be added again.")
	case errors.Is(err, phonesignin.ErrKeyProof):
		writePhoneRefusal(w, http.StatusForbidden, "proof_needed", "To add a key, enter your passcode or use a key you already have.")
	case errors.Is(err, phonesignin.ErrLastWayIn):
		writePhoneRefusal(w, http.StatusConflict, "last_way_in", "This key is your last way in. Set a passcode or add another key first.")
	case errors.Is(err, phonesignin.ErrNoPerson):
		writePhoneRefusal(w, http.StatusNotFound, "no_person", "No such person.")
	default:
		return false
	}
	return true
}

func decodeKeyBody(w http.ResponseWriter, req *http.Request, v any) bool {
	if err := json.NewDecoder(req.Body).Decode(v); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func needPerson(w http.ResponseWriter, personID *string) bool {
	if *personID = strings.TrimSpace(*personID); *personID == "" {
		writeJSONErr(w, http.StatusBadRequest, "person_id required: the person whose session asks")
		return false
	}
	return true
}

func keyChallenge(w http.ResponseWriter, req *http.Request, s keySignIn) {
	var in keyChallengeRequest
	if !decodeKeyBody(w, req, &in) {
		return
	}
	c, err := s.SignInChallenge(req.Context(), strings.TrimSpace(in.Origin), strings.TrimSpace(in.PersonID))
	if err != nil {
		if !writeKeyRefusal(w, err) {
			writeJSONErr(w, http.StatusServiceUnavailable, "the sign-in could not be started; try again")
		}
		return
	}
	// No credential is named: the browser offers the person's key itself, with no number typed.
	writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c.Challenge, "rp_id": c.RPID, "expires_at": c.ExpiresAt.UTC().Format(time.RFC3339),
		"user_verification": "required"})
}

func keyFinish(w http.ResponseWriter, req *http.Request, s keySignIn) {
	var in keyFinishRequest
	if !decodeKeyBody(w, req, &in) {
		return
	}
	out, err := s.SignInWithKey(req.Context(), in.Assertion, strings.TrimSpace(in.Address))
	if err != nil {
		if !writeKeyRefusal(w, err) {
			writeJSONErr(w, http.StatusServiceUnavailable, "the key could not be checked; try again")
		}
		return
	}
	writeJSONOK(w, http.StatusOK, keyFinishResponse{Person: out.Person, KeyID: out.KeyID, Ticket: out.Ticket,
		ExpiresAt: out.ExpiresAt.UTC().Format(time.RFC3339), PhoneCheckDue: s.PhoneCheckDue(out.Person)})
}

func keyRegisterChallenge(w http.ResponseWriter, req *http.Request, s keySignIn, approval approvalKeyChallenges, accounts personAccounts) {
	var in keyRegisterChallengeRequest
	if !decodeKeyBody(w, req, &in) || !needPerson(w, &in.PersonID) {
		return
	}
	var shared string
	if ws := strings.TrimSpace(in.WorkspaceID); ws != "" && approval != nil {
		// Lens writes into an account only for a person who holds it (B61.7): nothing is issued otherwise.
		identity, err := s.PersonIdentity(req.Context(), in.PersonID)
		if err != nil {
			if !writeKeyRefusal(w, err) {
				writeJSONErr(w, http.StatusServiceUnavailable, "the key could not be started; try again")
			}
			return
		}
		owned := false
		if accounts != nil {
			owned, err = accounts.PersonOwnsAccount(req.Context(), workspace.PersonKey(identity), deriveWorkspaceID(identity), ws)
			if err != nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "the key could not be started; try again")
				return
			}
		}
		if !owned {
			writePhoneRefusal(w, http.StatusForbidden, "not_your_account", "That account is not one of yours.")
			return
		}
		switch in.Kind {
		case "", "passkey":
			shared, _, err = approval.PasskeyRegistrationChallenge(req.Context(), ws)
		case "device":
			shared, err = approval.DeviceRegistrationChallenge(req.Context(), ws)
		default:
			writeJSONErr(w, http.StatusBadRequest, `kind must be "passkey" or "device"`)
			return
		}
		if err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "the key could not be started; try again")
			return
		}
	}
	c, err := s.RegisterChallenge(req.Context(), in.PersonID, shared)
	if err != nil {
		if !writeKeyRefusal(w, err) {
			writeJSONErr(w, http.StatusServiceUnavailable, "the key could not be started; try again")
		}
		return
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c.Challenge, "rp_id": c.RPID, "user_handle": c.UserHandle,
		"expires_at": c.ExpiresAt.UTC().Format(time.RFC3339), "alg": -7, "attestation": "none",
		"resident_key": "required", "user_verification": "required"})
}

func keyRegister(w http.ResponseWriter, req *http.Request, s keySignIn) {
	var in keyRegisterRequest
	if !decodeKeyBody(w, req, &in) || !needPerson(w, &in.PersonID) {
		return
	}
	k := phonesignin.NewKey{Name: in.Name}
	switch {
	case in.Passkey != nil && in.Device == nil:
		pk, err1 := passkey.Decode64(in.Passkey.PublicKey)
		cdj, err2 := passkey.Decode64(in.Passkey.ClientDataJSON)
		ad, err3 := passkey.Decode64(in.Passkey.AuthenticatorData)
		if in.Passkey.CredentialID == "" || err1 != nil || err2 != nil || err3 != nil {
			writeJSONErr(w, http.StatusBadRequest, `passkey must be {"credential_id", "public_key", "client_data_json", "authenticator_data"}, base64url`)
			return
		}
		k.Passkey = &passkey.Registration{CredentialID: in.Passkey.CredentialID, PublicKey: pk, ClientDataJSON: cdj, AuthenticatorData: ad}
	case in.Device != nil && in.Passkey == nil:
		pk, err1 := passkey.Decode64(in.Device.PublicKey)
		att, err2 := passkey.Decode64(in.Device.Attestation)
		if err1 != nil || err2 != nil {
			writeJSONErr(w, http.StatusBadRequest, `device must be {"platform", "public_key", "challenge", "signature", "attestation"}, base64url`)
			return
		}
		k.Device = &devicekey.Registration{Platform: in.Device.Platform, PublicKey: pk, Challenge: in.Device.Challenge,
			Signature: in.Device.Signature, Attestation: att}
	default:
		writeJSONErr(w, http.StatusBadRequest, `give "passkey" or "device", one of them`)
		return
	}
	key, err := s.AddKey(req.Context(), in.PersonID, k, phonesignin.KeyProof{Ticket: in.KeyTicket, Passcode: in.Passcode}, strings.TrimSpace(in.Address))
	if err != nil {
		if !writeKeyRefusal(w, err) {
			writeJSONErr(w, http.StatusServiceUnavailable, "the key could not be kept; try again")
		}
		return
	}
	writeJSONOK(w, http.StatusCreated, key)
}

func keyList(w http.ResponseWriter, req *http.Request, s keySignIn) {
	var in struct {
		PersonID string `json:"person_id"`
	}
	if !decodeKeyBody(w, req, &in) || !needPerson(w, &in.PersonID) {
		return
	}
	keys, err := s.ListKeys(req.Context(), in.PersonID)
	if err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "the keys could not be read; try again")
		return
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"keys": keys})
}

func keyRemove(w http.ResponseWriter, req *http.Request, s keySignIn) {
	var in keyRemoveRequest
	if !decodeKeyBody(w, req, &in) || !needPerson(w, &in.PersonID) {
		return
	}
	proof := phonesignin.KeyProof{Ticket: in.KeyTicket, Passcode: in.Passcode}
	if err := s.RemoveKey(req.Context(), in.PersonID, strings.TrimSpace(in.KeyID), proof, strings.TrimSpace(in.Address)); err != nil {
		if !writeKeyRefusal(w, err) {
			writeJSONErr(w, http.StatusServiceUnavailable, "the key could not be removed; try again")
		}
		return
	}
	writeJSONOK(w, http.StatusOK, map[string]bool{"removed": true})
}
