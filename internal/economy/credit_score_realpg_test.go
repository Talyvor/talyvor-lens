package economy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// B30.39 — on the migrated schema, two companies with seeded histories: one on Talyvor for two and a half years
// that funded and spent every month with a clean record, and one four months old that funded once, had a refund
// and a dispute against it, missed a credit line invoice and defaulted on a loan. They get different bands, each
// with its reasons, worst first. A private user's workspace has no score. The owner asking for a human review
// opens one task on the operator's list; asking again while it is open is the same task, and the operator
// closing it shows the owner the outcome.
func TestCreditScore_TwoHistoriesTwoBandsAndAReviewOpensAnOperatorTask(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ago := func(months int) time.Time { return now.AddDate(0, -months, 0) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for ws, w := range map[string]struct {
		company bool
		created time.Time
	}{"co-steady": {true, ago(30)}, "co-shaky": {true, ago(4)}, "person": {false, ago(30)}} {
		exec(`INSERT INTO workspaces (id, name, cache_prefix, company, created_at) VALUES ($1, $1, $1, $2, $3)`, ws, w.company, w.created)
	}
	ledger := func(ws, typ string, at time.Time) {
		exec(`INSERT INTO lxc_ledger (workspace_id, amount, balance_after, type, created_at) VALUES ($1, 1, 1, $2, $3)`, ws, typ, at)
	}
	for m := 0; m < 12; m++ {
		ledger("co-steady", "purchase", ago(m))
		ledger("co-steady", "spend", ago(m))
	}
	ledger("co-shaky", "purchase", ago(3))
	ledger("co-shaky", "spend", ago(3))
	exec(`INSERT INTO market_refunds (use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc, gross_usd_micros, reason, refunded_at)
	      VALUES ('use-1', 'lst-1', 'co-steady', 'co-shaky', 1, 1, 'taken down', $1)`, ago(2))
	exec(`INSERT INTO agent_escrows (id, payer_workspace_id, payer_agent_id, payee_workspace_id, payee_agent_id, amount_ulxc, class, release_at, status)
	      VALUES ('escrow-1', 'co-steady', 'ag-1', 'co-shaky', 'ag-2', 1, 'AMBER', $1, 'disputed')`, ago(1))
	exec(`INSERT INTO agent_escrow_events (escrow_id, kind, actor, detail, at) VALUES ('escrow-1', 'disputed', 'payer', 'not delivered', $1)`, ago(1))
	exec(`INSERT INTO credit_line_invoices (id, workspace_id, period_end, amount_ulxc, amount_cents, stripe_invoice_id, due_at)
	      VALUES ('cli-1', 'co-shaky', $1, 1, 1, 'in_1', $2)`, ago(2), ago(1))
	exec(`INSERT INTO agent_loans (id, lender_workspace_id, lender_agent_id, borrower_workspace_id, borrower_agent_id, principal_ulxc, interest_bps, instalments, every, status)
	      VALUES ('loan-1', 'co-steady', 'ag-1', 'co-shaky', 'ag-2', 1, 0, 2, 'week', 'defaulted')`)
	for _, kind := range []string{"missed", "defaulted"} {
		exec(`INSERT INTO agent_loan_events (loan_id, kind, at) VALUES ('loan-1', $1, $2)`, kind, ago(1))
	}

	steady, err := s.CreditScore(ctx, "co-steady", now)
	if err != nil {
		t.Fatal(err)
	}
	shaky, err := s.CreditScore(ctx, "co-shaky", now)
	if err != nil {
		t.Fatal(err)
	}
	if steady.Score != 1000 || steady.Band != "A" {
		t.Fatalf("steady company = %d %s, want 1000 A: %+v", steady.Score, steady.Band, steady.Reasons)
	}
	if shaky.Band != "E" || shaky.Score >= steady.Score {
		t.Fatalf("shaky company = %d %s, want band E below the steady one: %+v", shaky.Score, shaky.Band, shaky.Reasons)
	}
	want := CreditHistory{MonthsActive: 4, MonthsCounted: 5, FundedMonths: 1, SpendMonths: 1, RefundsAndDisputes: 2, MissedPayments: 2, Defaults: 1}
	if shaky.History != want {
		t.Fatalf("shaky history = %+v, want %+v", shaky.History, want)
	}
	if len(shaky.Reasons) != 5 || shaky.Reasons[0].Factor != "missed_payments" || shaky.Reasons[0].Points != 0 || shaky.Reasons[0].Text == "" {
		t.Fatalf("shaky reasons = %+v, want all five, worst first: missed payments at 0", shaky.Reasons)
	}
	if _, err := s.CreditScore(ctx, "person", now); !errors.Is(err, ErrCreditScoreCompaniesOnly) {
		t.Fatalf("a private user's score = %v, want refused", err)
	}

	review, err := s.RequestCreditScoreReview(ctx, "co-shaky", "owner-shaky", "the default was our bank's error", now)
	if err != nil || review.Status != "open" || review.Score != shaky.Score || review.Band != "E" || len(review.Reasons) != 5 {
		t.Fatalf("review = %+v, %v", review, err)
	}
	again, err := s.RequestCreditScoreReview(ctx, "co-shaky", "owner-shaky", "again", now)
	if err != nil || again.ID != review.ID {
		t.Fatalf("asking again while open = %+v, %v, want the same task %s", again, err, review.ID)
	}
	tasks, err := s.OpenCreditScoreReviews(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != review.ID || tasks[0].Note != "the default was our bank's error" {
		t.Fatalf("operator's tasks = %+v, %v, want the one review", tasks, err)
	}
	closed, err := s.CloseCreditScoreReview(ctx, review.ID, "operator-cli:ops", "checked with the lender: the default stands")
	if err != nil || closed.Status != "closed" || closed.ClosedAt == nil {
		t.Fatalf("close = %+v, %v", closed, err)
	}
	if tasks, _ := s.OpenCreditScoreReviews(ctx); len(tasks) != 0 {
		t.Fatalf("operator's tasks after closing = %+v, want none", tasks)
	}
	if _, err := s.CloseCreditScoreReview(ctx, review.ID, "operator-cli:ops", "twice"); !errors.Is(err, ErrNoCreditScoreReview) {
		t.Fatalf("closing a closed review = %v, want ErrNoCreditScoreReview", err)
	}
	seen, err := s.CreditScore(ctx, "co-shaky", now)
	if err != nil || seen.Review == nil || seen.Review.Outcome != "checked with the lender: the default stands" {
		t.Fatalf("owner's view after the review = %+v, %v, want the outcome", seen.Review, err)
	}
}
