package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/phonesignin"
)

// passcode_replace_handler.go — B61.107: A FORGOTTEN PASSCODE, SET AGAIN.
//
//	POST /v1/auth/passcode/replace   {"ticket", "key_ticket", "passcode", "caller_address"} — for the suite BFF on the
//	                                 provision secret, for a locked or signed-out phone
//
// The person confirms their number with the door's start and finish (ticket) and signs in with one of their keys
// (key_ticket, from /v1/auth/passkey/finish); both, of the same person, set the new passcode with no old one, and an
// off passcode comes back on. Either alone, or two people's, sets nothing and spends neither.

// passcodeReplacer is the slice of *phonesignin.Store the route needs.
type passcodeReplacer interface {
	ReplaceForgottenPasscode(ctx context.Context, phoneTicket, keyTicket, passcode, address string) (phonesignin.Person, error)
}

type passcodeReplaceRequest struct {
	Ticket    string `json:"ticket"`
	KeyTicket string `json:"key_ticket"`
	Passcode  string `json:"passcode"`
	Address   string `json:"caller_address"`
}

type passcodeReplaceResponse struct {
	OK     bool               `json:"ok"`
	Person phonesignin.Person `json:"person"`
}

// mountPasscodeReplaceRoute registers the route on the bare router, beside the phone door, on its secret.
func mountPasscodeReplaceRoute(r chi.Router, secret string, s passcodeReplacer) {
	if secret == "" {
		return
	}
	r.Post("/v1/auth/passcode/replace", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) {
		var in passcodeReplaceRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		p, err := s.ReplaceForgottenPasscode(req.Context(), in.Ticket, in.KeyTicket, in.Passcode, strings.TrimSpace(in.Address))
		if writePasscodeRefusal(w, err) {
			return
		}
		switch {
		case errors.Is(err, phonesignin.ErrTicket):
			writePhoneRefusal(w, http.StatusBadRequest, "ticket_ended", "That confirmation has ended. Confirm the number again.")
			return
		case errors.Is(err, phonesignin.ErrKeyTicket):
			writePhoneRefusal(w, http.StatusForbidden, "key_needed", "To set a new passcode, confirm it is you with Face ID or your key.")
			return
		case errors.Is(err, phonesignin.ErrTwoPeople):
			writePhoneRefusal(w, http.StatusForbidden, "not_same_person", "That key is not for this number. Use the key you made on this number.")
			return
		case errors.Is(err, phonesignin.ErrNoPerson):
			writePhoneRefusal(w, http.StatusNotFound, "no_person", "Nobody has joined with that number.")
			return
		case err != nil:
			slog.Warn("phone door: the new passcode could not be set", "cause", errorCause(err))
			writeJSONErr(w, http.StatusServiceUnavailable, "the passcode could not be set; try again")
			return
		}
		writeJSONOK(w, http.StatusOK, passcodeReplaceResponse{OK: true, Person: p})
	}))
}
