package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// credit_score.go — B30.39: A CREDIT SCORE FOR COMPANIES, WITH THE REASONS SHOWN.
//
// A score from 0 to 1000 and a band from A to E, worked out each time it is read from the company's own history
// on Talyvor over the last twelve calendar months (fewer if it is younger): how long it has been here, in how many
// months it funded and spent, the refunds and disputes against it, and the credit line invoices and loan
// instalments it missed. Each of those five is a reason carrying the points it earned of the most it could, shown
// worst first so the main reasons lead. The owner can ask a person to review it: that opens a task on the
// operator's list (`lens credit-scores`) holding the score as the owner saw it.
//
// A score never decides live credit alone: a person approves every live limit. Companies only — credit involving a
// private user is class RED.

// ErrCreditScoreCompaniesOnly: a private user's workspace has no credit score.
var ErrCreditScoreCompaniesOnly = errors.New("economy: Any loan or credit involving a private user is class RED — a credit score is for company workspaces only")

// ErrNoCreditScoreReview: no such review open.
var ErrNoCreditScoreReview = errors.New("economy: no such open credit score review")

// The most points each reason can give; they add up to 1000.
const (
	creditPointsTenure   = 250 // full at creditTenureFullMonths
	creditPointsFunding  = 200
	creditPointsSpend    = 200
	creditPointsDisputes = 100 // less creditPointsPerDispute for each refund or dispute
	creditPointsPayments = 250 // less creditPointsPerMissed for each missed payment; none after a default

	creditTenureFullMonths = 24
	creditPointsPerDispute = 25
	creditPointsPerMissed  = 125
)

// CreditHistory is what a score is worked out from.
type CreditHistory struct {
	MonthsActive       int `json:"months_active"`        // whole months since the workspace was made
	MonthsCounted      int `json:"months_counted"`       // calendar months looked at: up to the last 12
	FundedMonths       int `json:"funded_months"`        // of them, months with money or credits paid in
	SpendMonths        int `json:"spend_months"`         // of them, months with spending
	RefundsAndDisputes int `json:"refunds_and_disputes"` // refunds of its listings and disputes of its escrows
	MissedPayments     int `json:"missed_payments"`      // loan instalments missed and credit line invoices paid late or not at all
	Defaults           int `json:"defaults"`             // loans it defaulted on
}

// CreditScoreReason is one thing the score is made of.
type CreditScoreReason struct {
	Factor    string `json:"factor"` // months_active | regular_funding | steady_spend | refunds_disputes | missed_payments
	Points    int    `json:"points"`
	MaxPoints int    `json:"max_points"`
	Text      string `json:"text"`
}

// CreditScoreReview is the owner asking a person to look at the score: a task on the operator's list while open.
type CreditScoreReview struct {
	ID          string              `json:"id"`
	WorkspaceID string              `json:"workspace_id"`
	Score       int                 `json:"score"`
	Band        string              `json:"band"`
	Reasons     []CreditScoreReason `json:"reasons"`
	Note        string              `json:"note,omitempty"`
	RequestedBy string              `json:"requested_by"`
	Status      string              `json:"status"` // open | closed
	Outcome     string              `json:"outcome,omitempty"`
	ClosedBy    string              `json:"closed_by,omitempty"`
	RequestedAt time.Time           `json:"requested_at"`
	ClosedAt    *time.Time          `json:"closed_at,omitempty"`
}

// CreditScore is a company's score as its owner sees it, with its latest review.
type CreditScore struct {
	WorkspaceID string              `json:"workspace_id"`
	Score       int                 `json:"score"`
	Band        string              `json:"band"`
	Reasons     []CreditScoreReason `json:"reasons"`
	History     CreditHistory       `json:"history"`
	ComputedAt  time.Time           `json:"computed_at"`
	Review      *CreditScoreReview  `json:"review,omitempty"`
}

// creditHistorySQL counts a workspace's history from $2 up to and including $3; an invoice is missed once past due.
const creditHistorySQL = `SELECT
  (SELECT count(DISTINCT to_char(at AT TIME ZONE 'UTC', 'YYYY-MM')) FROM (
     SELECT created_at AS at FROM lxc_ledger WHERE workspace_id = $1 AND type = 'purchase' AND created_at >= $2 AND created_at <= $3
     UNION ALL
     SELECT created_at FROM money_entries WHERE workspace_id = $1 AND kind = 'payment_in' AND created_at >= $2 AND created_at <= $3) f),
  (SELECT count(DISTINCT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM')) FROM lxc_ledger
     WHERE workspace_id = $1 AND type = 'spend' AND created_at >= $2 AND created_at <= $3),
  (SELECT count(*) FROM market_refunds WHERE seller_workspace_id = $1 AND refunded_at >= $2 AND refunded_at <= $3)
  + (SELECT count(*) FROM agent_escrow_events ev JOIN agent_escrows e ON e.id = ev.escrow_id
     WHERE e.payee_workspace_id = $1 AND ev.kind = 'disputed' AND ev.at >= $2 AND ev.at <= $3),
  (SELECT count(*) FROM agent_loan_events ev JOIN agent_loans l ON l.id = ev.loan_id
     WHERE l.borrower_workspace_id = $1 AND ev.kind = 'missed' AND ev.at >= $2 AND ev.at <= $3)
  + (SELECT count(*) FROM credit_line_invoices
     WHERE workspace_id = $1 AND due_at >= $2 AND due_at < $3 AND (paid_at IS NULL OR paid_at > due_at)),
  (SELECT count(*) FROM agent_loan_events ev JOIN agent_loans l ON l.id = ev.loan_id
     WHERE l.borrower_workspace_id = $1 AND ev.kind = 'defaulted' AND ev.at >= $2 AND ev.at <= $3)`

const creditScoreReviewCols = `id, workspace_id, score, band, reasons, note, requested_by, status, outcome, closed_by, requested_at, closed_at`

// CreditScore works out a company's score at now from its history, with its latest review.
func (s *DualTokenStore) CreditScore(ctx context.Context, workspaceID string, now time.Time) (CreditScore, error) {
	now = now.UTC()
	var company bool
	var created time.Time
	err := s.pool.QueryRow(ctx, `SELECT company, created_at FROM workspaces WHERE id = $1`, workspaceID).Scan(&company, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditScore{}, fmt.Errorf("economy: no workspace %q", workspaceID)
	}
	if err != nil {
		return CreditScore{}, fmt.Errorf("economy: credit score: %w", err)
	}
	if !company {
		return CreditScore{}, ErrCreditScoreCompaniesOnly
	}
	created = created.UTC()
	h := CreditHistory{MonthsActive: (now.Year()-created.Year())*12 + int(now.Month()-created.Month())}
	h.MonthsCounted = min(h.MonthsActive+1, 12) // calendar months it was here in, before day-of-month is weighed
	if now.Day() < created.Day() {
		h.MonthsActive--
	}
	h.MonthsActive = max(h.MonthsActive, 0)
	h.MonthsCounted = max(h.MonthsCounted, 1)
	from := time.Date(now.Year(), now.Month()-time.Month(h.MonthsCounted-1), 1, 0, 0, 0, 0, time.UTC)
	if err := s.pool.QueryRow(ctx, creditHistorySQL, workspaceID, from, now).Scan(
		&h.FundedMonths, &h.SpendMonths, &h.RefundsAndDisputes, &h.MissedPayments, &h.Defaults); err != nil {
		return CreditScore{}, fmt.Errorf("economy: credit history: %w", err)
	}
	score, band, reasons := scoreCredit(h)
	cs := CreditScore{WorkspaceID: workspaceID, Score: score, Band: band, Reasons: reasons, History: h, ComputedAt: now}
	rows, err := s.pool.Query(ctx, `SELECT `+creditScoreReviewCols+` FROM credit_score_reviews
	  WHERE workspace_id = $1 ORDER BY requested_at DESC, id LIMIT 1`, workspaceID)
	if err != nil {
		return CreditScore{}, fmt.Errorf("economy: credit score review: %w", err)
	}
	reviews, err := scanCreditScoreReviews(rows)
	if err != nil {
		return CreditScore{}, err
	}
	if len(reviews) > 0 {
		cs.Review = &reviews[0]
	}
	return cs, nil
}

// scoreCredit turns a history into a score, a band and the reasons, worst first.
func scoreCredit(h CreditHistory) (int, string, []CreditScoreReason) {
	payments := max(creditPointsPayments-creditPointsPerMissed*h.MissedPayments, 0)
	paymentsText := fmt.Sprintf("%d credit line or loan payments missed in the last %d months.", h.MissedPayments, h.MonthsCounted)
	if h.Defaults > 0 {
		payments = 0
		paymentsText = fmt.Sprintf("%d loans defaulted on and %d payments missed in the last %d months.", h.Defaults, h.MissedPayments, h.MonthsCounted)
	}
	reasons := []CreditScoreReason{
		{"months_active", creditPointsTenure * min(h.MonthsActive, creditTenureFullMonths) / creditTenureFullMonths, creditPointsTenure,
			fmt.Sprintf("On Talyvor for %d months. Full points come at %d months.", h.MonthsActive, creditTenureFullMonths)},
		{"regular_funding", creditPointsFunding * h.FundedMonths / h.MonthsCounted, creditPointsFunding,
			fmt.Sprintf("Money paid in during %d of the last %d months.", h.FundedMonths, h.MonthsCounted)},
		{"steady_spend", creditPointsSpend * h.SpendMonths / h.MonthsCounted, creditPointsSpend,
			fmt.Sprintf("Spending in %d of the last %d months.", h.SpendMonths, h.MonthsCounted)},
		{"refunds_disputes", max(creditPointsDisputes-creditPointsPerDispute*h.RefundsAndDisputes, 0), creditPointsDisputes,
			fmt.Sprintf("%d refunds or disputes against the company in the last %d months.", h.RefundsAndDisputes, h.MonthsCounted)},
		{"missed_payments", payments, creditPointsPayments, paymentsText},
	}
	score := 0
	for _, r := range reasons {
		score += r.Points
	}
	sort.SliceStable(reasons, func(i, j int) bool {
		return reasons[i].MaxPoints-reasons[i].Points > reasons[j].MaxPoints-reasons[j].Points
	})
	band := "E"
	switch {
	case score >= 800:
		band = "A"
	case score >= 650:
		band = "B"
	case score >= 500:
		band = "C"
	case score >= 350:
		band = "D"
	}
	return score, band, reasons
}

// RequestCreditScoreReview asks a person to review the company's score as it stands at now. While a review is
// open, asking again answers that one.
func (s *DualTokenStore) RequestCreditScoreReview(ctx context.Context, workspaceID, by, note string, now time.Time) (CreditScoreReview, error) {
	cs, err := s.CreditScore(ctx, workspaceID, now)
	if err != nil {
		return CreditScoreReview{}, err
	}
	reasons, err := json.Marshal(cs.Reasons)
	if err != nil {
		return CreditScoreReview{}, fmt.Errorf("economy: credit score review: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO credit_score_reviews (id, workspace_id, score, band, reasons, note, requested_by)
	  VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (workspace_id) WHERE status = 'open' DO NOTHING`,
		"csr_"+uuid.NewString(), workspaceID, cs.Score, cs.Band, reasons, note, by); err != nil {
		return CreditScoreReview{}, fmt.Errorf("economy: credit score review: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+creditScoreReviewCols+` FROM credit_score_reviews
	  WHERE workspace_id = $1 AND status = 'open'`, workspaceID)
	if err != nil {
		return CreditScoreReview{}, fmt.Errorf("economy: credit score review: %w", err)
	}
	reviews, err := scanCreditScoreReviews(rows)
	if err != nil {
		return CreditScoreReview{}, err
	}
	if len(reviews) == 0 {
		return CreditScoreReview{}, errors.New("economy: credit score review: closed as it was asked for; ask again")
	}
	return reviews[0], nil
}

// OpenCreditScoreReviews is the operator's list: every open review, oldest first.
func (s *DualTokenStore) OpenCreditScoreReviews(ctx context.Context) ([]CreditScoreReview, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+creditScoreReviewCols+` FROM credit_score_reviews
	  WHERE status = 'open' ORDER BY requested_at, id`)
	if err != nil {
		return nil, fmt.Errorf("economy: credit score reviews: %w", err)
	}
	return scanCreditScoreReviews(rows)
}

// CloseCreditScoreReview is the operator's answer to an open review; the owner sees the outcome with the score.
func (s *DualTokenStore) CloseCreditScoreReview(ctx context.Context, id, by, outcome string) (CreditScoreReview, error) {
	rows, err := s.pool.Query(ctx, `UPDATE credit_score_reviews SET status = 'closed', outcome = $3, closed_by = $2, closed_at = now()
	  WHERE id = $1 AND status = 'open' RETURNING `+creditScoreReviewCols, id, by, outcome)
	if err != nil {
		return CreditScoreReview{}, fmt.Errorf("economy: close credit score review: %w", err)
	}
	reviews, err := scanCreditScoreReviews(rows)
	if err != nil {
		return CreditScoreReview{}, err
	}
	if len(reviews) == 0 {
		return CreditScoreReview{}, ErrNoCreditScoreReview
	}
	return reviews[0], nil
}

func scanCreditScoreReviews(rows pgx.Rows) ([]CreditScoreReview, error) {
	defer rows.Close()
	var out []CreditScoreReview
	for rows.Next() {
		var r CreditScoreReview
		var reasons []byte
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.Score, &r.Band, &reasons, &r.Note, &r.RequestedBy, &r.Status,
			&r.Outcome, &r.ClosedBy, &r.RequestedAt, &r.ClosedAt); err != nil {
			return nil, fmt.Errorf("economy: credit score review: %w", err)
		}
		if err := json.Unmarshal(reasons, &r.Reasons); err != nil {
			return nil, fmt.Errorf("economy: credit score review reasons: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: credit score reviews: %w", err)
	}
	return out, nil
}
