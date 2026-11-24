package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B22.9 — cash-out to money, behind a licensed partner (economy/agent_cash_outs.go).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/cash-outs/challenge  {amount_ulxc, destination} — the challenge that
//	                                                        cash-out is signed over, and the passkeys that may sign it
//	POST /v1/workspaces/{wsID}/agents/{agentID}/cash-outs   {amount_ulxc, destination, assertion} — turn the agent's
//	                                                        credits into money in the owner's bank account; once the
//	                                                        workspace has a passkey or device key, only signed (403)
//	GET  /v1/workspaces/{wsID}/cash-outs                    every cash-out and where it stands
//
// Only the workspace's owner or an admin may cash out — never an agent's own key. Held requests are handed to
// the partner, and its answers recorded, on the agent schedules' tick. Only the test partner exists: it pays
// nothing. Mounted in the authed group.

type cashOutBank interface {
	CashOutChallenge(ctx context.Context, workspaceID, agentID string, amount int64, destination string) (string, []string, error)
	RequestCashOut(ctx context.Context, workspaceID, agentID string, amount int64, destination, requestedBy string, a *passkey.Assertion) (economy.CashOut, error)
	ListCashOuts(ctx context.Context, workspaceID string) ([]economy.CashOut, error)
}

func mountCashOutRoutes(r chi.Router, bank cashOutBank) {
	type cashOutBody struct {
		AmountULXC  int64              `json:"amount_ulxc"`
		Destination string             `json:"destination"`
		Assertion   *passkey.Assertion `json:"assertion"`
	}
	decode := func(w http.ResponseWriter, req *http.Request) (cashOutBody, bool) {
		var in cashOutBody
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_ulxc": …, "destination": "<a label for the bank account>", "assertion": {…}}`)
			return in, false
		}
		return in, true
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/cash-outs/challenge", ownerOrRole(func(w http.ResponseWriter, req *http.Request) {
		in, ok := decode(w, req)
		if !ok {
			return
		}
		c, creds, err := bank.CashOutChallenge(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.AmountULXC, in.Destination)
		if err != nil {
			writeCashOutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	}, moneyRoles...))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/cash-outs", ownerOrRole(func(w http.ResponseWriter, req *http.Request) {
		in, ok := decode(w, req)
		if !ok {
			return
		}
		by := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			by = actx.UserID
		}
		ctx, ok := keyedMove(w, req) // B28.72
		if !ok {
			return
		}
		c, err := bank.RequestCashOut(ctx, chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.AmountULXC, in.Destination, by, in.Assertion)
		if err != nil {
			writeCashOutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, c)
	}, moneyRoles...))
	r.Get("/v1/workspaces/{wsID}/cash-outs", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListCashOuts(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"cash_outs": list})
	}))
}

func writeCashOutErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrCashOut):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrNoCashOutPartner):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, economy.ErrCashOutUnsigned), errors.Is(err, passkey.ErrInvalid):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	default:
		writeTransferErr(w, err)
	}
}
