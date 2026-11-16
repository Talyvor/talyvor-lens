package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
	"github.com/talyvor/lens/internal/workspace"
)

// number_move_handler.go — B61.30: LOST PHONE, NEW NUMBER, OR A FORGOTTEN PASSCODE WITH NO KEY.
//
//	POST /v1/admin/people/move-number          {"phone", "new_phone", "name", "date_of_birth", "country"} — behind
//	                                           requireAdminStepUp: the operator's step-up code and an operator_audit
//	                                           row naming them, written before anything runs
//	POST /v1/auth/phone/number-moves/claim     the moves whose person's email addresses are still to be told, taken
//	                                           once — for the suite BFF on the provision secret, which holds them
//
// A person who has neither their number nor a key asks help (the help form); there is no way to do this alone in the
// first version. The operator moves the number only after the person's identity check has passed again against the
// name, date of birth and country on record (economy.RecheckPersonIdentity: the Sandbox provider in Sandbox). The
// move clears the passcode and removes every sign-in key; the person then confirms the new number by text and, with
// that ticket, sets a new passcode and adds a key. For a day every account of the person can look and cannot move
// money, and every device of those accounts is told; the BFF tells the email addresses.

// numberMovePeople is the slice of *phonesignin.Store the move needs.
type numberMovePeople interface {
	PersonByPhone(ctx context.Context, phone string) (phonesignin.Person, bool, error)
	MoveNumber(ctx context.Context, m phonesignin.NumberMove) (phonesignin.Moved, error)
}

// numberMoveIdentity is the slice of *economy.DualTokenStore the move needs.
type numberMoveIdentity interface {
	RecheckPersonIdentity(ctx context.Context, verifiers partners.Verifiers, accounts []string, by string,
		in economy.VerificationRequest) (economy.VerificationCheck, error)
	TellNumberMoved(ctx context.Context, accounts []string)
}

// personAccountLister is *workspace.Manager's PersonAccounts.
type personAccountLister interface {
	PersonAccounts(ctx context.Context, personKey, firstID string) ([]workspace.PersonAccount, error)
}

type numberMoveRequest struct {
	Phone       string `json:"phone"`
	NewPhone    string `json:"new_phone"`
	Name        string `json:"name"`
	DateOfBirth string `json:"date_of_birth"`
	Country     string `json:"country"`
}

// tellInBackground runs tell after the answer, so a slow push service never holds the operator's request.
var tellInBackground = func(tell func(ctx context.Context)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		tell(ctx)
	}()
}

func newNumberMoveHandler(people numberMovePeople, accounts personAccountLister, identity numberMoveIdentity, verifiers partners.Verifiers) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in numberMoveRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		operator := strings.TrimSpace(req.Header.Get(moderatorOperatorHeader))
		oldPhone, err := phonesignin.NormalisePhone(in.Phone)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "phone: "+err.Error())
			return
		}
		newPhone, err := phonesignin.NormalisePhone(in.NewPhone)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "new_phone: "+err.Error())
			return
		}
		if oldPhone == newPhone {
			writeJSONErr(w, http.StatusBadRequest, "not moved: the new number is the one the person has")
			return
		}
		p, found, err := people.PersonByPhone(req.Context(), oldPhone)
		if err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "the person could not be read; try again")
			return
		}
		if !found {
			writeJSONErr(w, http.StatusNotFound, "not moved: nobody holds that number")
			return
		}
		held, err := accounts.PersonAccounts(req.Context(), workspace.PersonKey(p.Identity), deriveWorkspaceID(p.Identity))
		if err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "the person's accounts could not be read; try again")
			return
		}
		ids := make([]string, 0, len(held))
		for _, a := range held {
			ids = append(ids, a.ID)
		}
		check, err := identity.RecheckPersonIdentity(req.Context(), verifiers, ids, "operator:"+operator, economy.VerificationRequest{
			Name: in.Name, DateOfBirth: in.DateOfBirth, Country: in.Country})
		switch {
		case errors.Is(err, economy.ErrVerificationInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, economy.ErrNoIdentityOnRecord):
			writeJSONErr(w, http.StatusConflict, "not moved: the person has no identity check on record to check against")
			return
		case errors.Is(err, economy.ErrNotPersonOnRecord):
			writeJSONErr(w, http.StatusForbidden, "not moved: the name, date of birth and country are not those on record for the person who holds that number")
			return
		case errors.Is(err, economy.ErrIdentityRecheckFailed):
			writeJSONErr(w, http.StatusForbidden, "not moved: "+err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusServiceUnavailable, "the identity check could not be asked; try again")
			return
		}
		moved, err := people.MoveNumber(req.Context(), phonesignin.NumberMove{PersonID: p.ID, OldPhone: oldPhone, NewPhone: newPhone,
			Operator: operator, IdentityCheckRef: check.EvidenceRef, Accounts: ids})
		switch {
		case errors.Is(err, phonesignin.ErrHeld):
			writeJSONErr(w, http.StatusConflict, "not moved: another person holds the new number")
			return
		case errors.Is(err, phonesignin.ErrNotThatPerson):
			writeJSONErr(w, http.StatusConflict, "not moved: the number changed hands while it was checked; start again")
			return
		case err != nil:
			writeJSONErr(w, http.StatusServiceUnavailable, "the number could not be moved; try again")
			return
		}
		tellInBackground(func(ctx context.Context) { identity.TellNumberMoved(ctx, ids) })
		writeJSONOK(w, http.StatusOK, moved)
	}
}

// numberMoveNotices is *phonesignin.Store's ClaimNumberMoveNotices.
type numberMoveNotices interface {
	ClaimNumberMoveNotices(ctx context.Context, limit int) ([]phonesignin.NumberMoveNotice, error)
}

// mountNumberMoveNoticeRoute registers the BFF's claim on the bare router, beside the phone door, on its secret.
func mountNumberMoveNoticeRoute(r chi.Router, secret string, s numberMoveNotices) {
	if secret == "" {
		return
	}
	r.Post("/v1/auth/phone/number-moves/claim", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) {
		out, err := s.ClaimNumberMoveNotices(req.Context(), 100)
		if err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "the moves could not be read; try again")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"moves": out})
	}))
}
