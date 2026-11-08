package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B22.5 — loans between companies: offer, borrow, repay (economy/company_loans.go).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/loans   {to, principal_ulxc, interest_bps, instalments, every,
//	                                                    late_fee_ulxc, memo} — offer a loan to another company's agent
//	GET  /v1/workspaces/{wsID}/loans                    the loans it lends and borrows, with every instalment
//	GET  /v1/workspaces/{wsID}/loans/{loanID}
//	POST /v1/workspaces/{wsID}/loans/{loanID}/accept    the borrower's side: the principal is paid out, less the lending fee (B56.22)
//	POST /v1/workspaces/{wsID}/loans/{loanID}/decline
//	POST /v1/workspaces/{wsID}/loans/{loanID}/withdraw  the lender's side, before it is answered
//
// Offering takes the lending agent's own key, the workspace's owner or an admin; answering and withdrawing
// take the owner or an admin. Instalments are taken on the agent schedules' tick. Mounted in the authed group.

type companyLoanBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	OfferLoan(ctx context.Context, workspaceID, lenderAgentID, borrowerAddress string, terms economy.LoanTerms) (economy.Loan, error)
	AnswerLoan(ctx context.Context, workspaceID, loanID string, accept bool) (economy.Loan, error)
	WithdrawLoan(ctx context.Context, workspaceID, loanID string) error
	GetLoan(ctx context.Context, workspaceID, loanID string) (economy.Loan, error)
	ListLoans(ctx context.Context, workspaceID string) ([]economy.Loan, error)
}

func mountCompanyLoanRoutes(r chi.Router, bank companyLoanBank) {
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/loans", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		_, owner := storedanswers.OwnerOrAdmin(req.Context())
		if !owner {
			actx := auth.GetAuthContext(req.Context())
			if actx == nil || actx.APIKeyID == "" {
				writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may lend its money")
				return
			}
			if keyAgent, _, _ := bank.AgentOfKey(req.Context(), actx.APIKeyID); keyAgent != agentID {
				writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may lend its money")
				return
			}
		}
		var in struct {
			To            string `json:"to"`
			PrincipalULXC int64  `json:"principal_ulxc"`
			InterestBPS   int    `json:"interest_bps"`
			Instalments   int    `json:"instalments"`
			Every         string `json:"every"`
			LateFeeULXC   int64  `json:"late_fee_ulxc"`
			Memo          string `json:"memo"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.To == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to": "<wallet id or @handle>", "principal_ulxc": …, "interest_bps": …, "instalments": …, "every": "day|week|month", "late_fee_ulxc": …, "memo": "<optional>"}`)
			return
		}
		l, err := bank.OfferLoan(req.Context(), chi.URLParam(req, "wsID"), agentID, in.To, economy.LoanTerms{PrincipalULXC: in.PrincipalULXC,
			InterestBPS: in.InterestBPS, Instalments: in.Instalments, Every: in.Every, LateFeeULXC: in.LateFeeULXC, Memo: in.Memo})
		if err != nil {
			writeLoanErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, l)
	})
	r.Get("/v1/workspaces/{wsID}/loans", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListLoans(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if list == nil {
			list = []economy.Loan{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"loans": list})
	})
	r.Get("/v1/workspaces/{wsID}/loans/{loanID}", func(w http.ResponseWriter, req *http.Request) {
		l, err := bank.GetLoan(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "loanID"))
		if err != nil {
			writeLoanErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})
	for _, answer := range []string{"accept", "decline"} {
		accept := answer == "accept"
		r.Post("/v1/workspaces/{wsID}/loans/{loanID}/"+answer, ownerOnly(func(w http.ResponseWriter, req *http.Request) {
			l, err := bank.AnswerLoan(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "loanID"), accept)
			if err != nil {
				writeLoanErr(w, err)
				return
			}
			writeJSONOK(w, http.StatusOK, l)
		}))
	}
	r.Post("/v1/workspaces/{wsID}/loans/{loanID}/withdraw", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		if err := bank.WithdrawLoan(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "loanID")); err != nil {
			writeLoanErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"status": "withdrawn"})
	}))
}

func writeLoanErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrLoanNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrLoanTerms):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrLoanCompaniesOnly):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	default:
		writeTransferErr(w, err)
	}
}
