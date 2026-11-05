package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/talyvor/lens/internal/money"
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
//	POST /v1/synthetic/workspaces/{wsID}/money/cards/{cardID}/charge
//	                                                                   B53.18: {"merchant":"Tesco","amount_minor":3120}
//	                                                                   that merchant charges the test workspace's
//	                                                                   single-use card, as its issuer asks Lens:
//	                                                                   {"authorization_id","approved","reason","mcc",
//	                                                                   "country","channel","wallet"}
//	POST /v1/synthetic/workspaces/{wsID}/money/schedules/{scheduleID}/due
//	                                                                   B45.42: a test plan's signed standing order's
//	                                                                   next tick due now: the minute tick runs it once
//	POST /v1/synthetic/workspaces/{wsID}/card-link                    B32.102: {"workspace_id":"<another test workspace>"}
//	                                                                   the two linked as though both paid with one
//	                                                                   card, so a trust panel's linked buyer is testable
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

// syntheticMoneyCards is *economy.DualTokenStore: B53.18's synthetic charge asks it as a money card's issuer would.
type syntheticMoneyCards interface {
	AuthorizeCardCharge(ctx context.Context, partner string, a money.Authorization) (bool, string, error)
}

// syntheticSchedules is *economy.DualTokenStore: B45.42 brings a standing order's next tick due.
type syntheticSchedules interface {
	BringTestScheduleDue(ctx context.Context, workspaceID, scheduleID string, now time.Time) (economy.AgentSchedule, error)
}

type syntheticDueDeps struct {
	db         syntheticQuerier
	loans      syntheticLoans
	bills      syntheticBills
	payouts    syntheticPayouts
	sellerTax  syntheticSellerTax
	cards      syntheticCards
	authorizer syntheticCardAuthorizer
	moneyCards syntheticMoneyCards
	schedules  syntheticSchedules
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
	if due.moneyCards != nil {
		r.Post("/v1/synthetic/workspaces/{wsID}/money/cards/{cardID}/charge",
			syntheticGuard(key, "money-card-charge", limit, d, due.testOnly(due.moneyCardCharge)))
	}
	if due.schedules != nil {
		r.Post("/v1/synthetic/workspaces/{wsID}/money/schedules/{scheduleID}/due",
			syntheticGuard(key, "schedule-due", limit, d, due.testOnly(due.scheduleDue)))
	}
	r.Post("/v1/synthetic/workspaces/{wsID}/card-link", syntheticGuard(key, "card-link", limit, d, due.testOnly(due.cardLink)))
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

func (d syntheticDueDeps) scheduleDue(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	sc, err := d.schedules.BringTestScheduleDue(r.Context(), ws, chi.URLParam(r, "scheduleID"), time.Now())
	switch {
	case errors.Is(err, economy.ErrScheduleNotFound):
		writeJSONErr(w, http.StatusNotFound, "no running standing order of this workspace has that id")
		return 0, "refused: no such schedule"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, sc)
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

// cardLink is B32.102: a fresh synthetic fingerprint on both workspaces, written only when the other is a test one too.
// Test-mode payments never capture a fingerprint (B25.2), so this is the only way a test user shares a card.
func (d syntheticDueDeps) cardLink(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	var in struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return 0, "error: bad request"
	}
	if in.WorkspaceID == "" || in.WorkspaceID == ws {
		writeJSONErr(w, http.StatusBadRequest, "workspace_id must name another test workspace")
		return 0, "error: bad request"
	}
	// Never a real card's hash: sha256 of a random value.
	fp := sha256.Sum256([]byte(uuid.NewString()))
	var linked bool
	if err := d.db.QueryRow(r.Context(), `WITH test AS (SELECT count(*) = 2 AS ok FROM workspaces WHERE id IN ($1, $2) AND synthetic),
		ins AS (INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash)
			SELECT w, $3 FROM unnest(ARRAY[$1, $2]::text[]) AS w, test WHERE test.ok ON CONFLICT DO NOTHING)
		SELECT ok FROM test`, ws, in.WorkspaceID, "synthetic:"+hex.EncodeToString(fp[:])).Scan(&linked); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	if !linked {
		writeJSONErr(w, http.StatusForbidden, "only two test (synthetic) workspaces can be linked by a card")
		return 0, "refused: not a test workspace"
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"workspace_id": ws, "linked_to": in.WorkspaceID})
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

// moneyCardCharge is B53.18: merchant charging one of the test workspace's single-use money cards, asked of Lens as its
// issuer asks it — the card's own partner and ref, a fresh authorisation id, and the merchant's details as a test issuer
// reports them. Lens's own decision (merchant, cap, once, expiry) is the only thing that approves it.
func (d syntheticDueDeps) moneyCardCharge(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	var in struct {
		Merchant    string `json:"merchant"`
		AmountMinor int64  `json:"amount_minor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return 0, "error: bad request"
	}
	if strings.TrimSpace(in.Merchant) == "" || in.AmountMinor <= 0 {
		writeJSONErr(w, http.StatusBadRequest, "merchant must be named and amount_minor positive, in the card's own currency")
		return 0, "error: bad request"
	}
	var partner, ref, currency string
	err := d.db.QueryRow(r.Context(), `SELECT partner, partner_ref, currency FROM money_cards
		WHERE id = $1 AND workspace_id = $2 AND partner IS NOT NULL AND partner_ref IS NOT NULL`, chi.URLParam(r, "cardID"), ws).
		Scan(&partner, &ref, &currency)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSONErr(w, http.StatusNotFound, "no issued money card of this workspace has that id")
		return 0, "refused: no card"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	details := money.TestMerchantDetails(in.Merchant)
	a := money.Authorization{ID: "iauth_synthetic_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Card: ref, Merchant: in.Merchant,
		Amount: money.Amount{Minor: in.AmountMinor, Asset: currency}, CardDetails: details}
	approved, reason, err := d.moneyCards.AuthorizeCardCharge(r.Context(), partner, a)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"authorization_id": a.ID, "approved": approved, "reason": reason, "mcc": details.MCC,
		"country": details.Country, "channel": details.Channel, "wallet": details.Wallet})
	return 1, "ok"
}
