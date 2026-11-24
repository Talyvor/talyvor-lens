package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/workspace"
)

// B61.32 — pay a person on Talyvor by their name, phone number or email (economy/people_found.go,
// economy/person_payments.go).
//
// For the suite BFF alone, on the provision secret, for the person whose session it holds:
//
//	POST /v1/people/finding        {person_id} — how the person is found: their Talyvor name, the email the BFF
//	                               confirmed, their phone number, the three switches and the account they receive into
//	POST /v1/people/finding/change {person_id, name?, email?, by_name?, by_phone?, by_email?, receive_account_id?} —
//	                               changes what is given and answers the settings as they are now
//
// For an account, in the authed group — its owner or an admin, or an agent's own key:
//
//	POST /v1/money/people/find          {to} — {shown_name}, or 404 with the one answer for nobody found; 429 at the
//	                                    account's limit of lookups an hour
//	POST /v1/money/people/pay/challenge {the payment, as it will be sent} — the challenge an owner signs it over
//	POST /v1/money/people/pay           {to, amount_minor, currency, account_id, note, funding, idempotency_key,
//	                                    assertion} — pays the person once under the key, the owner's signed and from
//	                                    the company's account, an agent's from its own and judged by its rules

type peopleFinding interface {
	PersonFinding(ctx context.Context, personID string) (economy.PersonFinding, error)
	SetPersonFinding(ctx context.Context, personID string, ch economy.PersonFindingChange) (economy.PersonFinding, error)
}

func mountPeopleFindingRoutes(r chi.Router, secret string, s peopleFinding) {
	if secret == "" {
		return
	}
	r.Post("/v1/people/finding", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			PersonID string `json:"person_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || !needPerson(w, &in.PersonID) {
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"person_id"}`)
			}
			return
		}
		f, err := s.PersonFinding(req.Context(), in.PersonID)
		writeFindingErrOr(w, f, err)
	}))
	r.Post("/v1/people/finding/change", phoneSignInHandler(secret, func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			PersonID string `json:"person_id"`
			economy.PersonFindingChange
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || !needPerson(w, &in.PersonID) {
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"person_id", "name", "email", "by_name", "by_phone", "by_email", "receive_account_id"}`)
			}
			return
		}
		f, err := s.SetPersonFinding(req.Context(), in.PersonID, in.PersonFindingChange)
		writeFindingErrOr(w, f, err)
	}))
}

func writeFindingErrOr(w http.ResponseWriter, f economy.PersonFinding, err error) {
	switch {
	case errors.Is(err, economy.ErrNoSuchPerson):
		writePhoneRefusal(w, http.StatusNotFound, "no_person", "No such person.")
	case errors.Is(err, economy.ErrPersonName), errors.Is(err, economy.ErrPersonEmail), errors.Is(err, economy.ErrPersonFinding):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrPersonNameTaken):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSONOK(w, http.StatusOK, f)
	}
}

type personPayer interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	FindPerson(ctx context.Context, workspaceID, to string) (economy.FoundPerson, error)
	PersonPaymentChallenge(ctx context.Context, workspaceID string, in economy.PersonPayment) (string, []string, error)
	PayPerson(ctx context.Context, workspaceID string, in economy.PersonPayment) (economy.PersonPayment, error)
	PayPersonSigned(ctx context.Context, workspaceID string, in economy.PersonPayment, a *passkey.Assertion) (economy.PersonPayment, error)
}

// personPaymentBody is a payment to a person as it is sent.
type personPaymentBody struct {
	To             string             `json:"to"`
	AmountMinor    int64              `json:"amount_minor"`
	Currency       string             `json:"currency"`
	AccountID      string             `json:"account_id"`
	Note           string             `json:"note"`
	Funding        string             `json:"funding"`
	IdempotencyKey string             `json:"idempotency_key"`
	Assertion      *passkey.Assertion `json:"assertion"`
}

func (b personPaymentBody) payment(req *http.Request, agentID string) economy.PersonPayment {
	if k := req.Header.Get("Idempotency-Key"); k != "" {
		b.IdempotencyKey = k
	}
	return economy.PersonPayment{To: b.To, AmountMinor: b.AmountMinor, Currency: b.Currency, AccountID: b.AccountID, Note: b.Note,
		Funding: b.Funding, IdempotencyKey: b.IdempotencyKey, AgentID: agentID}
}

func mountPersonPaymentRoutes(r chi.Router, bank personPayer) {
	caller := func(w http.ResponseWriter, req *http.Request, roles ...string) (ws, agentID string, ok bool) {
		return moneyCaller(w, req, bank.AgentOfKey, roles...)
	}
	r.Post("/v1/money/people/find", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := caller(w, req)
		if !ok {
			return
		}
		var in struct {
			To string `json:"to"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || strings.TrimSpace(in.To) == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to": "<a Talyvor name, phone number or email>"}`)
			return
		}
		found, err := bank.FindPerson(req.Context(), ws, in.To)
		if err != nil {
			writePersonPaymentErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, found)
	})
	r.Post("/v1/money/people/pay/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, moneyRoles...)
		if !ok {
			return
		}
		if agentID != "" {
			writeJSONErr(w, http.StatusForbidden, "an agent's own key pays by its rules and signs nothing: only the owner or an admin signs a payment")
			return
		}
		var in personPaymentBody
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be the payment: {"to", "amount_minor", "currency", "note", "idempotency_key"}`)
			return
		}
		c, creds, err := bank.PersonPaymentChallenge(req.Context(), ws, in.payment(req, ""))
		if err != nil {
			writePersonPaymentErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	})
	r.With(withTask).Post("/v1/money/people/pay", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, moneyRoles...)
		if !ok {
			return
		}
		var in personPaymentBody
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to", "amount_minor", "currency", "note", "idempotency_key"}`)
			return
		}
		var p economy.PersonPayment
		var err error
		if agentID == "" { // a person paying signs it (B54.33); an agent's own key is judged by its rules
			p, err = bank.PayPersonSigned(req.Context(), ws, in.payment(req, ""), in.Assertion)
		} else {
			p, err = bank.PayPerson(req.Context(), ws, in.payment(req, agentID))
		}
		if err != nil {
			writePersonPaymentErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, p)
	})
}

func writePersonPaymentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrPersonNotFound):
		writePhoneRefusal(w, http.StatusNotFound, "not_found", economy.PersonNotFoundWords)
	case errors.Is(err, economy.ErrPersonLookupLimit):
		writeJSONErr(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, economy.ErrConnectApprovalsWaiting):
		writeJSONErr(w, http.StatusTooManyRequests, err.Error())
	case writePendingApproval(w, err):
	case errors.Is(err, economy.ErrPersonPayment), errors.Is(err, economy.ErrPersonFinding), errors.Is(err, economy.ErrPayYourself):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrPersonCurrency), errors.Is(err, economy.ErrPersonCannotBePaid), errors.Is(err, economy.ErrPersonPhoneSandbox):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrPersonFirstPaymentKey), errors.Is(err, workspace.ErrMoneyWall):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	default:
		writePaymentErr(w, err)
	}
}
