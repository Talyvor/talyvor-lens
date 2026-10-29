package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// test_bill.go — B25.7: A TEST BUYER'S BILL PAID, ITS EARNINGS PAST THE HOLDBACK, AND REFUNDED, INSIDE ONE TESTER RUN.
//
// A buyer's marketplace uses clear when their monthly bill is paid, and the seller's earnings wait the 14-day
// holdback after that; a paid bill is refunded when Stripe says so (charge.refunded). A tester run lasts
// minutes, so for a TEST (synthetic) buyer only, PayTestBill pays the bill now — recorded as paid one holdback
// ago, since an earning is append-only and its payable_at is fixed when it clears — and RefundTestBill refunds
// a paid one as charge.refunded does (ReverseInvoice). A real buyer is never touched.

// ErrNotTestBuyer: the buyer is not a test workspace; nothing was paid or refunded.
var ErrNotTestBuyer = errors.New("market: only a test (synthetic) workspace's bill can be paid or refunded here")

// ErrNoTestBill: no paid bill of this test buyer has that id.
var ErrNoTestBill = errors.New("market: no paid bill of this test workspace has that id")

// PayTestBill pays a test buyer's marketplace bill now: every billed use metered and not yet cleared clears on
// one invoice (in_synthetic_…), paid Holdback before now, so its sellers' earnings are payable at now — the
// payout run or take-as-credits then pays them. It answers the invoice and how many uses it cleared.
func (s *Store) PayTestBill(ctx context.Context, buyerWorkspaceID string, now time.Time) (string, int, error) {
	var test bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, buyerWorkspaceID).Scan(&test); err != nil {
		return "", 0, fmt.Errorf("market: pay a test bill: %w", err)
	}
	if !test {
		return "", 0, ErrNotTestBuyer
	}
	invoiceID := "in_synthetic_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	paidAt := now.Add(-Holdback)
	n, err := s.ClearInvoice(ctx, buyerWorkspaceID, invoiceID, time.Unix(0, 0).UTC(), now.Add(time.Hour), paidAt, false)
	if err != nil || n == 0 {
		return "", n, err
	}
	// B28.140: its billing period began where the buyer's last one ended, or with the first use it cleared, and ends now.
	var start time.Time
	if err := s.pool.QueryRow(ctx, `SELECT LEAST((SELECT max(period_end) FROM market_invoices WHERE buyer_workspace_id = $1),
		(SELECT min(used_at) FROM market_uses WHERE cleared_invoice_id = $2))`, buyerWorkspaceID, invoiceID).Scan(&start); err != nil {
		return invoiceID, n, fmt.Errorf("market: pay a test bill: %w", err)
	}
	// A bill Lens paid itself has no Stripe invoice: its PDF is Talyvor's receipt for it, as the app opens it.
	pdf := ""
	if s.receipts != nil { // B32.40: the paid bill's receipt, in the test series
		r, err := s.IssueReceipt(ctx, buyerWorkspaceID, invoiceID, paidAt, false, nil)
		if err != nil {
			return invoiceID, n, err
		}
		if s.receipts.AppURL != "" {
			pdf = s.receipts.AppURL + "/api/marketplace/receipts/" + r.ID + "?format=pdf"
		}
	}
	return invoiceID, n, s.RecordMarketInvoice(ctx, buyerWorkspaceID, invoiceID, "", pdf, start, now, paidAt, false)
}

// RefundTestBill refunds a test buyer's paid bill in full, as Stripe's charge.refunded refunds one: a
// buyer_refund market_refunds row per use it cleared, each seller's share reversed. It answers how many uses
// were refunded now (a bill refunded before refunds nothing more).
func (s *Store) RefundTestBill(ctx context.Context, buyerWorkspaceID, invoiceID string) (int, error) {
	var mine, all int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE buyer_workspace_id = $2 AND test), count(*)
		FROM market_uses WHERE cleared_invoice_id = $1`, invoiceID, buyerWorkspaceID).Scan(&mine, &all); err != nil {
		return 0, fmt.Errorf("market: refund a test bill: %w", err)
	}
	if all == 0 || mine != all {
		return 0, ErrNoTestBill
	}
	n, _, err := s.ReverseInvoice(ctx, invoiceID, "buyer_refund", "ch_synthetic_"+strings.ReplaceAll(uuid.NewString(), "-", ""), 0, true)
	return n, err
}

// PayOutTestSeller is the weekly payout run for one test (synthetic) seller now, as on the payout weekday (B32.99):
// their earnings past the holdback are released onto the journal; a seller withheld for their tax details is answered
// held and nothing is paid; otherwise what is available is paid exactly as the run pays it — the minimum, once an
// ISO week, Stripe's fees as PayoutFees takes them, journalled the same — and its transfer is recorded as
// tr_synthetic_…, with no Stripe call, on test money as PayTestBill pays a test bill. It answers the payout made
// now, or nil (held, under the minimum, or paid this week already). A real seller is never touched.
func (s *Store) PayOutTestSeller(ctx context.Context, workspaceID string, now time.Time) (held bool, made *Payout, err error) {
	var test bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&test); err != nil {
		return false, nil, fmt.Errorf("market: pay out a test seller: %w", err)
	}
	if !test {
		return false, nil, ErrNotTestBuyer
	}
	if _, err := s.ReleaseDue(ctx, now); err != nil {
		return false, nil, err
	}
	// A test seller connected to Stripe test mode keeps its account on the row; one never connected has none to name.
	account := "acct_synthetic"
	if err := s.pool.QueryRow(ctx, `SELECT `+taxHeldSQL+`, COALESCE((SELECT stripe_account_id FROM market_sellers WHERE workspace_id = $1), $2)
		FROM (SELECT $1::text AS workspace_id) s`, workspaceID, account).Scan(&held, &account); err != nil {
		return false, nil, fmt.Errorf("market: pay out a test seller: %w", err)
	}
	if held {
		return true, nil, nil
	}
	payable := func(q queryRower, ws string) (int64, error) {
		released, _, paid, err := sellerBalance(ctx, q, ws, now)
		return released - paid, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		id, err := s.payOutTx(ctx, tx, workspaceID, account, "", false, payable, now)
		if err != nil || id == "" {
			return err
		}
		var p Payout
		if err := tx.QueryRow(ctx, `UPDATE market_payouts SET stripe_transfer_id = $2, paid_at = $3 WHERE id = $1
			RETURNING id, method, month, period, gross_usd_micros, vat_usd_micros, account_fee_usd_micros, payout_fee_usd_micros, net_usd_micros,
			          credits_ulxc, stripe_transfer_id, paid_at, last_error, created_at`,
			id, "tr_synthetic_"+strings.ReplaceAll(uuid.NewString(), "-", ""), now).Scan(&p.ID, &p.Method, &p.Month, &p.Period, &p.GrossUSDMicros,
			&p.VATUSDMicros, &p.AccountFeeUSDMicros, &p.PayoutFeeUSDMicros, &p.NetUSDMicros, &p.CreditsULXC, &p.StripeTransferID, &p.PaidAt,
			&p.LastError, &p.CreatedAt); err != nil {
			return err
		}
		made = &p
		return nil
	})
	if err != nil {
		return false, nil, fmt.Errorf("market: pay out test seller %s: %w", workspaceID, err)
	}
	return false, made, nil
}
