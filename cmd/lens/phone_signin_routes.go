package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/phonesignin"
)

// B61.22 — the front door by phone number, for the suite BFF alone, on the provision secret it already holds: a
// texted code confirms a number, and its one-use ticket finds or makes the person who holds it. The BFF passes the
// caller's address for the limits.

// phoneSignIn is the narrow slice of *phonesignin.Store the routes need.
type phoneSignIn interface {
	Start(ctx context.Context, phone, address string) (phonesignin.Started, error)
	Finish(ctx context.Context, phone, code, address string) (phonesignin.Finished, error)
	RedeemWithPasscode(ctx context.Context, ticket, identity, passcode, address string) (phonesignin.Person, bool, error)
	CheckPasscode(ctx context.Context, personID, passcode, address string) error
	ChangePasscode(ctx context.Context, personID, old, passcode, address string) error
	PhoneCheckDue(p phonesignin.Person) bool
	Refused(ctx context.Context, phone, address, detail string) error
}

type phoneStartRequest struct {
	Phone   string `json:"phone"`
	Address string `json:"caller_address"`
}

type phoneStartResponse struct {
	ExpiresAt string `json:"expires_at"`
	Sender    string `json:"sender"`
	// SandboxCode is the code the Sandbox sender did not send, for the screen to show under the Sandbox label.
	SandboxCode string `json:"sandbox_code,omitempty"`
}

type phoneFinishRequest struct {
	Phone   string `json:"phone"`
	Code    string `json:"code"`
	Address string `json:"caller_address"`
}

type phoneFinishResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expires_at"`
	HasPerson bool   `json:"has_person"`
	Sandbox   bool   `json:"sandbox"`
}

type phonePersonRequest struct {
	Ticket   string `json:"ticket"`
	Identity string `json:"identity"`
	// Passcode (B61.23): for a person who has one, theirs, or the ticket signs nobody in; for a new person, or one
	// who has not set one, the passcode to set.
	Passcode string `json:"passcode"`
	Address  string `json:"caller_address"`
}

// passcodeRequest is the phone app's lock (check) and a change: the BFF calls them only for the person whose
// session it holds, so the passcode alone opens nothing.
type passcodeRequest struct {
	PersonID    string `json:"person_id"`
	Passcode    string `json:"passcode"`
	OldPasscode string `json:"old_passcode"`
	Address     string `json:"caller_address"`
}

type phonePersonResponse struct {
	Person  phonesignin.Person `json:"person"`
	Created bool               `json:"created"`
	// PhoneCheckDue: the number was confirmed only by the Sandbox sender and a real sender is set now.
	PhoneCheckDue bool `json:"phone_check_due"`
}

// mountPhoneSignInRoutes registers the door on the bare router, beside /v1/provision and for the same reason: its
// caller carries LENS_PROVISION_SECRET, not an API key. Unset secret ⇒ not registered.
func mountPhoneSignInRoutes(r chi.Router, secret string, s phoneSignIn) {
	if secret == "" {
		return
	}
	r.Post("/v1/auth/phone/start", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { phoneStart(w, req, s) }))
	r.Post("/v1/auth/phone/finish", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { phoneFinish(w, req, s) }))
	r.Post("/v1/auth/phone/person", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { phonePerson(w, req, s) }))
	// B61.23 — the passcode, for a session of the person the BFF holds.
	r.Post("/v1/auth/passcode/check", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { passcodeCheck(w, req, s) }))
	r.Post("/v1/auth/passcode/change", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) { passcodeChange(w, req, s) }))
}

// writePasscodeRefusal answers a passcode refused; false when err is none of the passcode's own. A wrong try's
// answer never says how close it was.
func writePasscodeRefusal(w http.ResponseWriter, err error) bool {
	var (
		wait *phonesignin.PasscodeWaitError
		weak *phonesignin.WeakPasscodeError
	)
	switch {
	case errors.As(err, &wait):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(time.Until(wait.RetryAt).Seconds()+0.999))))
		at := wait.RetryAt.UTC().Format(time.RFC3339)
		writeJSONOK(w, http.StatusTooManyRequests, map[string]string{"error": "Too many wrong passcodes. Try again at " + at + ".",
			"reason": "passcode_wait", "retry_at": at})
	case errors.As(err, &weak):
		writePhoneRefusal(w, http.StatusBadRequest, "weak_passcode", weak.Reason)
	case errors.Is(err, phonesignin.ErrPasscodeNeeded):
		writePhoneRefusal(w, http.StatusBadRequest, "passcode_needed", "Enter your passcode.")
	case errors.Is(err, phonesignin.ErrWrongPasscode):
		writePhoneRefusal(w, http.StatusBadRequest, "wrong_passcode", "That passcode is not right.")
	case errors.Is(err, phonesignin.ErrPasscodeOff):
		writePhoneRefusal(w, http.StatusForbidden, "passcode_off", "Your passcode is off after too many wrong tries. Sign in with your key and set a new one.")
	case errors.Is(err, phonesignin.ErrNoPasscode):
		writePhoneRefusal(w, http.StatusNotFound, "no_passcode", "No passcode is set.")
	case errors.Is(err, phonesignin.ErrPasscodeUnavailable):
		writeJSONErr(w, http.StatusServiceUnavailable, "the passcode cannot be set or checked now; try again later")
	default:
		return false
	}
	return true
}

func decodePasscode(w http.ResponseWriter, req *http.Request) (passcodeRequest, bool) {
	var in passcodeRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
		return in, false
	}
	if in.PersonID = strings.TrimSpace(in.PersonID); in.PersonID == "" {
		writeJSONErr(w, http.StatusBadRequest, "person_id required: the person whose session asks")
		return in, false
	}
	in.Address = strings.TrimSpace(in.Address)
	return in, true
}

func passcodeCheck(w http.ResponseWriter, req *http.Request, s phoneSignIn) {
	in, ok := decodePasscode(w, req)
	if !ok {
		return
	}
	if err := s.CheckPasscode(req.Context(), in.PersonID, in.Passcode, in.Address); err != nil {
		if !writePasscodeRefusal(w, err) {
			writeJSONErr(w, http.StatusServiceUnavailable, "the passcode could not be checked; try again")
		}
		return
	}
	writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
}

func passcodeChange(w http.ResponseWriter, req *http.Request, s phoneSignIn) {
	in, ok := decodePasscode(w, req)
	if !ok {
		return
	}
	if err := s.ChangePasscode(req.Context(), in.PersonID, in.OldPasscode, in.Passcode, in.Address); err != nil {
		if !writePasscodeRefusal(w, err) {
			writeJSONErr(w, http.StatusServiceUnavailable, "the passcode could not be changed; try again")
		}
		return
	}
	writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
}

func phoneSignInHandler(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(provisionSecretHeader)), []byte(secret)) != 1 {
			writeJSONErr(w, http.StatusUnauthorized, "provisioning credentials required")
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, 8<<10)
		next(w, req)
	}
}

func writePhoneRefusal(w http.ResponseWriter, status int, reason, message string) {
	writeJSONOK(w, status, map[string]string{"error": message, "reason": reason})
}

func phoneStart(w http.ResponseWriter, req *http.Request, s phoneSignIn) {
	var in phoneStartRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Address = strings.TrimSpace(in.Address)
	if in.Address == "" {
		writeJSONErr(w, http.StatusBadRequest, "caller_address required: the address the person asked from, for the limits")
		return
	}
	phone, err := phonesignin.NormalisePhone(in.Phone)
	if err != nil {
		if aerr := s.Refused(req.Context(), "", in.Address, "not a number in a country"); aerr != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "the code could not be sent; try again")
			return
		}
		writePhoneRefusal(w, http.StatusBadRequest, "phone", err.Error())
		return
	}
	out, err := s.Start(req.Context(), phone, in.Address)
	var limit *phonesignin.LimitError
	switch {
	case errors.As(err, &limit):
		wait := max(1, int(time.Until(limit.RetryAt).Seconds()+0.999))
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeJSONOK(w, http.StatusTooManyRequests, map[string]string{"error": "Too many codes asked for. Try again at " +
			limit.RetryAt.UTC().Format(time.RFC3339) + ".", "reason": "limited", "retry_at": limit.RetryAt.UTC().Format(time.RFC3339)})
		return
	case err != nil:
		writeJSONErr(w, http.StatusServiceUnavailable, "the code could not be sent; try again")
		return
	}
	writeJSONOK(w, http.StatusOK, phoneStartResponse{ExpiresAt: out.ExpiresAt.UTC().Format(time.RFC3339), Sender: out.Sender, SandboxCode: out.SandboxCode})
}

func phoneFinish(w http.ResponseWriter, req *http.Request, s phoneSignIn) {
	var in phoneFinishRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	phone, err := phonesignin.NormalisePhone(in.Phone)
	if err != nil {
		writePhoneRefusal(w, http.StatusBadRequest, "phone", err.Error())
		return
	}
	out, err := s.Finish(req.Context(), phone, strings.TrimSpace(in.Code), strings.TrimSpace(in.Address))
	switch {
	case errors.Is(err, phonesignin.ErrWrongCode):
		writePhoneRefusal(w, http.StatusBadRequest, "wrong_code", "That code is not right. Check the text and try again.")
		return
	case errors.Is(err, phonesignin.ErrNoCode):
		writePhoneRefusal(w, http.StatusBadRequest, "code_ended", "That code has ended. Ask for a new one.")
		return
	case err != nil:
		writeJSONErr(w, http.StatusServiceUnavailable, "the code could not be checked; try again")
		return
	}
	writeJSONOK(w, http.StatusOK, phoneFinishResponse{Ticket: out.Ticket, ExpiresAt: out.ExpiresAt.UTC().Format(time.RFC3339),
		HasPerson: out.HasPerson, Sandbox: out.Sandbox})
}

func phonePerson(w http.ResponseWriter, req *http.Request, s phoneSignIn) {
	var in phonePersonRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Ticket == "" {
		writeJSONErr(w, http.StatusBadRequest, "ticket required")
		return
	}
	p, created, err := s.RedeemWithPasscode(req.Context(), in.Ticket, strings.TrimSpace(in.Identity), in.Passcode, strings.TrimSpace(in.Address))
	if writePasscodeRefusal(w, err) {
		return
	}
	switch {
	case errors.Is(err, phonesignin.ErrTicket):
		writePhoneRefusal(w, http.StatusBadRequest, "ticket_ended", "That confirmation has ended. Confirm the number again.")
		return
	case errors.Is(err, phonesignin.ErrHeld):
		writePhoneRefusal(w, http.StatusConflict, "held", "That number or identity belongs to another person.")
		return
	case err != nil:
		writeJSONErr(w, http.StatusServiceUnavailable, "the person could not be found; try again")
		return
	}
	writeJSONOK(w, http.StatusOK, phonePersonResponse{Person: p, Created: created, PhoneCheckDue: s.PhoneCheckDue(p)})
}

// newPeopleCountsHandler is GET /v1/admin/people/counts: how many people and what the door did in a day, never whose.
func newPeopleCountsHandler(s *phonesignin.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		c, err := s.Counts(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "the counts could not be read")
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	}
}
