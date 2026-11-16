package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/phonesignin"
	"github.com/talyvor/lens/internal/workspace"
)

// number_change_handler.go — B61.104: A PERSON CHANGES THEIR OWN PHONE NUMBER.
//
//	POST /v1/auth/phone/change-number   {"person_id", "ticket", "passcode", "caller_address"} — for the suite BFF on
//	                                    the provision secret, for the person whose session it holds
//
// The person confirms the new number with the door's own start and finish (its limits, its Sandbox code), and the
// ticket that gives, with their passcode, moves them onto it. Their keys, passcode and accounts stay. Refused: a number
// another person holds, in the door's words, which only a right code reaches; their own number; a wrong passcode and
// its wait; no passcode set. Every device of their accounts is told; the BFF claims the change with the operator's
// moves (POST /v1/auth/phone/number-moves/claim) and tells the email addresses and the old number.

// numberChanger is the slice of *phonesignin.Store the change needs.
type numberChanger interface {
	ChangeNumber(ctx context.Context, personID, ticket, passcode, address string) (phonesignin.NumberChanged, error)
}

// numberChangeTeller is *economy.DualTokenStore's TellNumberChanged.
type numberChangeTeller interface {
	TellNumberChanged(ctx context.Context, accounts []string)
}

type numberChangeRequest struct {
	PersonID string `json:"person_id"`
	Ticket   string `json:"ticket"`
	Passcode string `json:"passcode"`
	Address  string `json:"caller_address"`
}

type numberChangeResponse struct {
	MoveID string             `json:"move_id"`
	Person phonesignin.Person `json:"person"`
}

// mountNumberChangeRoute registers the change on the bare router, beside the phone door, on its secret.
func mountNumberChangeRoute(r chi.Router, secret string, people numberChanger, accounts personAccountLister, tell numberChangeTeller) {
	if secret == "" {
		return
	}
	r.Post("/v1/auth/phone/change-number", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) {
		var in numberChangeRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if in.PersonID = strings.TrimSpace(in.PersonID); in.PersonID == "" {
			writeJSONErr(w, http.StatusBadRequest, "person_id required: the person whose session asks")
			return
		}
		if in.Ticket == "" {
			writeJSONErr(w, http.StatusBadRequest, "ticket required: the new number confirmed by its code")
			return
		}
		out, err := people.ChangeNumber(req.Context(), in.PersonID, in.Ticket, in.Passcode, strings.TrimSpace(in.Address))
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
		case errors.Is(err, phonesignin.ErrSameNumber):
			writePhoneRefusal(w, http.StatusBadRequest, "same_number", "That is your number already.")
			return
		case errors.Is(err, phonesignin.ErrNotThatPerson):
			writePhoneRefusal(w, http.StatusConflict, "number_moved", "Your number changed while this was checked. Start again.")
			return
		case errors.Is(err, phonesignin.ErrNoPerson):
			writeJSONErr(w, http.StatusNotFound, "no such person")
			return
		case err != nil:
			writeJSONErr(w, http.StatusServiceUnavailable, "the number could not be changed; try again")
			return
		}
		if held, err := accounts.PersonAccounts(req.Context(), workspace.PersonKey(out.Person.Identity), deriveWorkspaceID(out.Person.Identity)); err == nil {
			ids := make([]string, 0, len(held))
			for _, a := range held {
				ids = append(ids, a.ID)
			}
			tellInBackground(func(ctx context.Context) { tell.TellNumberChanged(ctx, ids) })
		}
		writeJSONOK(w, http.StatusOK, numberChangeResponse{MoveID: out.MoveID, Person: out.Person})
	}))
}
