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
	Redeem(ctx context.Context, ticket, identity, address string) (phonesignin.Person, bool, error)
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
	Address  string `json:"caller_address"`
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
	p, created, err := s.Redeem(req.Context(), in.Ticket, strings.TrimSpace(in.Identity), strings.TrimSpace(in.Address))
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
