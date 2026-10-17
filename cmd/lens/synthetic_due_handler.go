package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/sellertax"
)

// B25.7 — A TEST USER'S SLOW MONEY BROUGHT DUE INSIDE ONE TESTER RUN.
//
// A tester run lasts minutes; four of a test user's money functions only happen days later. For a TEST
// (synthetic) workspace only, these synthetic-key routes bring each due now:
//
//	POST /v1/synthetic/workspaces/{wsID}/loans/{loanID}/due          a test loan's next instalment due now: the
//	                                                                   minute tick takes it, or misses it (late),
//	                                                                   and brought due again while late, it defaults
//	POST /v1/synthetic/workspaces/{wsID}/marketplace/bill/pay         the test buyer's bill paid, its sellers'
//	                                                                   earnings past the 14-day holdback: the payout
//	                                                                   run or take-as-credits then pays them
//	POST /v1/synthetic/workspaces/{wsID}/marketplace/bill/{invoiceID}/refund
//	                                                                   that paid bill refunded, as Stripe's
//	                                                                   charge.refunded refunds it
//	POST /v1/synthetic/workspaces/{wsID}/marketplace/payouts/run      B32.99: the weekly payout run for the test
//	                                                                   seller now, its tax-details requests brought
//	                                                                   due first: {"withheld","hold","payout"}
//	POST /v1/synthetic/workspaces/{wsID}/agents/{agentID}/card/authorizations
//	                                                                   {"amount_minor":2000,"currency":"gbp","merchant":"…"}
//	                                                                   a purchase on the test agent's card, decided
//	                                                                   as Stripe Issuing's authorisation request is
//
// Each is refused 403 for a workspace that is not synthetic — read from the database every Lens shares — and
// nothing changes. Each is also written to synthetic_operations, like create and reset, under its own rate.

// syntheticMaxDueCalls is how many of these a minute: a tester run brings several of each due per company.
const syntheticMaxDueCalls = 60

type syntheticQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type syntheticLoans interface {
	BringTestLoanDue(ctx context.Context, workspaceID, loanID string, now time.Time) (economy.Loan, error)
}

type syntheticBills interface {
	PayTestBill(ctx context.Context, buyerWorkspaceID string, now time.Time) (string, int, error)
	RefundTestBill(ctx context.Context, buyerWorkspaceID, invoiceID string) (int, error)
}

type syntheticPayouts interface {
	PayOutTestSeller(ctx context.Context, workspaceID string, now time.Time) (bool, *market.Payout, error)
}

type syntheticSellerTax interface {
	BringRemindersDue(ctx context.Context, workspaceID string) error
}

type syntheticCards interface {
	GetAgentCard(ctx context.Context, workspaceID, agentID string) (economy.AgentCard, error)
}

// syntheticCardAuthorizer is *agentcard.Handler: it prices a purchase and lets the agent's rules decide it.
type syntheticCardAuthorizer interface {
	Authorize(ctx context.Context, a economy.CardAuthorization) (economy.CardDecision, error)
}

type syntheticDueDeps struct {
	db         syntheticQuerier
	loans      syntheticLoans
	bills      syntheticBills
	payouts    syntheticPayouts
	sellerTax  syntheticSellerTax
	cards      syntheticCards
	authorizer syntheticCardAuthorizer
}

func mountSyntheticDueRoutes(r chi.Router, key string, d syntheticDeps) {
	due := d.due
	if due.db == nil {
		return
	}
	limit := &callLimiter{max: syntheticMaxDueCalls, per: syntheticCallsPer}
	r.Post("/v1/synthetic/workspaces/{wsID}/loans/{loanID}/due", syntheticGuard(key, "loan-due", limit, d, due.testOnly(due.loanDue)))
	r.Post("/v1/synthetic/workspaces/{wsID}/marketplace/bill/pay", syntheticGuard(key, "bill-pay", limit, d, due.testOnly(due.billPay)))
	r.Post("/v1/synthetic/workspaces/{wsID}/marketplace/bill/{invoiceID}/refund",
		syntheticGuard(key, "bill-refund", limit, d, due.testOnly(due.billRefund)))
	if due.payouts != nil && due.sellerTax != nil {
		r.Post("/v1/synthetic/workspaces/{wsID}/marketplace/payouts/run", syntheticGuard(key, "payout-run", limit, d, due.testOnly(due.payoutRun)))
	}
	r.Post("/v1/synthetic/workspaces/{wsID}/agents/{agentID}/card/authorizations",
		syntheticGuard(key, "card-authorization", limit, d, due.testOnly(due.cardPurchase)))
}

// testOnly runs run for a synthetic workspace, and refuses any other before anything is read or written.
func (d syntheticDueDeps) testOnly(run func(w http.ResponseWriter, r *http.Request, ws string) (int, string)) func(http.ResponseWriter, *http.Request) (int, string) {
	return func(w http.ResponseWriter, r *http.Request) (int, string) {
		ws := chi.URLParam(r, "wsID")
		var test bool
		if err := d.db.QueryRow(r.Context(), `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, ws).Scan(&test); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return 0, "error: " + err.Error()
		}
		if !test {
			writeJSONErr(w, http.StatusForbidden, "only a test (synthetic) workspace's money can be brought due")
			return 0, "refused: not a test workspace"
		}
		return run(w, r, ws)
	}
}

func (d syntheticDueDeps) loanDue(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	loan, err := d.loans.BringTestLoanDue(r.Context(), ws, chi.URLParam(r, "loanID"), time.Now())
	switch {
	case errors.Is(err, economy.ErrLoanNotFound):
		writeJSONErr(w, http.StatusNotFound, "no active or late test loan of this workspace has that id")
		return 0, "refused: no such loan"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, loan)
	return 1, "ok"
}

func (d syntheticDueDeps) billPay(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	invoiceID, n, err := d.bills.PayTestBill(r.Context(), ws, time.Now())
	switch {
	case errors.Is(err, market.ErrNotTestBuyer):
		writeJSONErr(w, http.StatusForbidden, err.Error())
		return 0, "refused: not a test workspace"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	case n == 0:
		writeJSONErr(w, http.StatusConflict, "the bill holds no metered, unpaid marketplace use (a paid use is metered within a minute)")
		return 0, "refused: nothing on the bill"
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "uses_cleared": n})
	return 1, "ok"
}

func (d syntheticDueDeps) billRefund(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	invoiceID := chi.URLParam(r, "invoiceID")
	n, err := d.bills.RefundTestBill(r.Context(), ws, invoiceID)
	switch {
	case errors.Is(err, market.ErrNoTestBill):
		writeJSONErr(w, http.StatusNotFound, err.Error())
		return 0, "refused: no such bill"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "uses_refunded": n})
	return 1, "ok"
}

// payoutRun is B32.99: a test seller withheld for their tax details, or paid, inside one tester run.
func (d syntheticDueDeps) payoutRun(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	if err := d.sellerTax.BringRemindersDue(r.Context(), ws); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	held, payout, err := d.payouts.PayOutTestSeller(r.Context(), ws, time.Now())
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	out := map[string]any{"withheld": held, "payout": payout}
	if held {
		out["hold"] = sellertax.HoldReason
	}
	writeJSONOK(w, http.StatusOK, out)
	return 1, "ok"
}

func (d syntheticDueDeps) cardPurchase(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	in := struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Merchant    string `json:"merchant"`
		Category    string `json:"category"`
	}{AmountMinor: 2000, Currency: "gbp", Merchant: "Synthetic merchant", Category: "miscellaneous_general_merchandise"}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return 0, "error: bad request"
		}
	}
	if in.AmountMinor <= 0 || len(in.Currency) != 3 {
		writeJSONErr(w, http.StatusBadRequest, "amount_minor must be positive and currency a 3-letter code")
		return 0, "error: bad request"
	}
	card, err := d.cards.GetAgentCard(r.Context(), ws, chi.URLParam(r, "agentID"))
	switch {
	case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrNoAgentCard):
		writeJSONErr(w, http.StatusNotFound, err.Error())
		return 0, "refused: no card"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	currency := strings.ToLower(in.Currency)
	a := economy.CardAuthorization{EventID: "evt_synthetic_" + id, AuthorizationID: "iauth_synthetic_" + id, CardID: card.ID,
		AmountMinor: in.AmountMinor, Currency: currency, MerchantAmountMinor: in.AmountMinor, MerchantCurrency: currency,
		MerchantName: in.Merchant, MerchantCategory: in.Category, MerchantID: "synthetic-merchant", At: time.Now().UTC()}
	dec, err := d.authorizer.Authorize(r.Context(), a)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"authorization_id": a.AuthorizationID, "approved": dec.Approved, "reason": dec.Reason,
		"amount_ulxc": dec.AmountULXC, "approval_id": dec.ApprovalID})
	return 1, "ok"
}
