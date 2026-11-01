package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.16 — saved payees outside Talyvor, with confirmation of payee (economy/money_payees.go).
//
//	POST /v1/money/payees     {name, country, sort_code, account_number | iban, bic | routing_number, account_number
//	                          | address, network "base"} — saves the payee with what its bank said of the name: check
//	                          exact_match, close_match (with suggested_name) or no_match; a USDC address on Base (B46.18)
//	                          is check address, paid once the owner confirms it; a sanctioned name is refused (403)
//	GET  /v1/money/payees     — the workspace's payees, each with its check and whether it needs confirmation
//	POST /v1/money/payees/{id}/challenge — the challenge a close match or no match is confirmed over
//	POST /v1/money/payees/{id}/confirm   {"assertion": {credential_id, client_data_json, authenticator_data,
//	                          signature}} — the owner confirms it with a passkey; until then it cannot be paid
//	POST /v1/money/payees/{id}/called/challenge — B41.111: the challenge the owner signs to say they called the payee
//	POST /v1/money/payees/{id}/called    {"assertion": {...}} — they called it on a number they already had: its
//	                          24-hour cooling-off (cooling_off_until) ends and the payments it held are paid
//	POST /v1/money/payees/{id}/details/challenge {country, sort_code, account_number | iban, bic | routing_number,
//	                          account_number | address} — B41.112: the challenge changing its details to these is signed over
//	POST /v1/money/payees/{id}/details {the same details, "assertion": {...}} — changes them with a passkey: the name is
//	                          checked afresh at the new account's bank, and every payment to it is held for 24 hours
//	                          (held_until), with a notice to call them on a number the owner already knows
//
// The workspace is the key's; only its owner or an admin may use these. Mounted in the authed group.

type moneyPayeeBank interface {
	CreateOutsidePayee(ctx context.Context, workspaceID string, in economy.OutsidePayee) (economy.OutsidePayee, error)
	OutsidePayees(ctx context.Context, workspaceID string) ([]economy.OutsidePayee, error)
	PayeeChallenge(ctx context.Context, workspaceID, payeeID string) (challenge string, credentialIDs []string, err error)
	ConfirmOutsidePayee(ctx context.Context, workspaceID, payeeID string, a *passkey.Assertion) (economy.OutsidePayee, error)
	PayeeCalledChallenge(ctx context.Context, workspaceID, payeeID string) (challenge string, credentialIDs []string, err error)
	ConfirmPayeeCalled(ctx context.Context, workspaceID, payeeID string, a *passkey.Assertion) (economy.OutsidePayee, error)
	PayeeDetailsChallenge(ctx context.Context, workspaceID, payeeID string, in economy.OutsidePayee) (challenge string, credentialIDs []string, err error)
	ChangeOutsidePayeeDetails(ctx context.Context, workspaceID, payeeID string, in economy.OutsidePayee, a *passkey.Assertion) (economy.OutsidePayee, error)
}

func mountMoneyPayeeRoutes(r chi.Router, bank moneyPayeeBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "payees are a workspace's: use a workspace's key")
			return "", false
		}
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may manage its payees")
			return "", false
		}
		return ws, true
	}
	r.Post("/v1/money/payees", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in economy.OutsidePayee
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name", "country", and "sort_code" + "account_number", "iban" (+ "bic"), "routing_number" + "account_number" or "address" (+ "network")}`)
			return
		}
		p, err := bank.CreateOutsidePayee(req.Context(), ws, economy.OutsidePayee{Name: in.Name, Country: in.Country, SortCode: in.SortCode,
			AccountNumber: in.AccountNumber, IBAN: in.IBAN, BIC: in.BIC, RoutingNumber: in.RoutingNumber, Network: in.Network, Address: in.Address})
		switch {
		case errors.Is(err, economy.ErrPayee):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, screening.ErrBlocked):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, economy.ErrPayeeCheckUnavailable):
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		case errors.Is(err, partners.ErrScreeningUnavailable):
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, p)
		}
	})
	r.Get("/v1/money/payees", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		list, err := bank.OutsidePayees(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"payees": list})
	})
	r.Post("/v1/money/payees/{id}/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		c, creds, err := bank.PayeeChallenge(req.Context(), ws, chi.URLParam(req, "id"))
		switch {
		case errors.Is(err, economy.ErrPayeeNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrPayeeConfirmed), errors.Is(err, economy.ErrPayeePasskey):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
		}
	})
	r.Post("/v1/money/payees/{id}/confirm", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Assertion *passkey.Assertion `json:"assertion"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		p, err := bank.ConfirmOutsidePayee(req.Context(), ws, chi.URLParam(req, "id"), in.Assertion)
		switch {
		case errors.Is(err, economy.ErrPayeeNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrPayeeConfirmed):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrPayeePasskey), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, p)
		}
	})
	r.Post("/v1/money/payees/{id}/called/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		c, creds, err := bank.PayeeCalledChallenge(req.Context(), ws, chi.URLParam(req, "id"))
		switch {
		case errors.Is(err, economy.ErrPayeeNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrPayeePasskey):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
		}
	})
	r.Post("/v1/money/payees/{id}/called", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Assertion *passkey.Assertion `json:"assertion"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		p, err := bank.ConfirmPayeeCalled(req.Context(), ws, chi.URLParam(req, "id"), in.Assertion)
		switch {
		case errors.Is(err, economy.ErrPayeeNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrPayeePasskey), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil && p.CalledAt == nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default: // kept; a held payment that could not be paid now is tried again by the minute's run
			writeJSONOK(w, http.StatusOK, p)
		}
	})
	type detailsBody struct {
		economy.OutsidePayee
		Assertion *passkey.Assertion `json:"assertion"`
	}
	r.Post("/v1/money/payees/{id}/details/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in detailsBody
		_ = json.NewDecoder(req.Body).Decode(&in)
		c, creds, err := bank.PayeeDetailsChallenge(req.Context(), ws, chi.URLParam(req, "id"), in.OutsidePayee)
		switch {
		case errors.Is(err, economy.ErrPayeeNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrPayee):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPayeePasskey):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required",
				"notice": economy.PayeeDetailsNotice})
		}
	})
	r.Post("/v1/money/payees/{id}/details", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in detailsBody
		_ = json.NewDecoder(req.Body).Decode(&in)
		p, err := bank.ChangeOutsidePayeeDetails(req.Context(), ws, chi.URLParam(req, "id"), in.OutsidePayee, in.Assertion)
		switch {
		case errors.Is(err, economy.ErrPayeeNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrPayee):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPayeePasskey), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, economy.ErrPayeeCheckUnavailable):
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, p)
		}
	})
}
