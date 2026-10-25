package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// payment_risk.go — B41.103: INVOICE RISK CHECKS ON EVERY BILL AND PAYMENT.
//
// Before approval, each item paying a saved payee — a bill or a payment — is checked, and what is found goes on the
// approval card as the item's flags: the same invoice number or amount already paid to this payee; bank details on
// the bill that differ from the saved payee's; the first payment to this payee; an amount far above this payee's
// usual (transaction monitoring's thresholds, B30.7). A duplicate or a changed bank detail is never paid under a
// mandate: the person approves it with a passkey and a reason, and the reason is part of the statement they sign.

// The flags an item can carry.
const (
	FlagDuplicate          = "duplicate"
	FlagBankDetailsChanged = "bank_details_changed"
	FlagFirstPayment       = "first_payment"
	FlagUnusualAmount      = "unusual_amount"
)

// ItemFlag is one risk found on an item, as the approval card shows it.
type ItemFlag struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// maxApprovalReason is how long a reason for approving a flagged item may be.
const maxApprovalReason = 500

// reasonFlags is the codes among flags that a person must give a reason to approve.
func reasonFlags(flags []ItemFlag) []string {
	var out []string
	for _, f := range flags {
		if f.Code == FlagDuplicate || f.Code == FlagBankDetailsChanged {
			out = append(out, f.Code)
		}
	}
	return out
}

// unusualAmount is when an amount is far above a payee's usual; zero until set, and then nothing is flagged unusual.
type unusualAmount struct{ timesUsual, fromPayments, historyDays int }

// SetUnusualAmount sets when an item's amount is flagged far above its payee's usual: at least timesUsual times the
// median of the payments to the payee in its currency over the last historyDays, once there are fromPayments of them.
// main sets it from transaction monitoring's settings (B30.7), so both judge "usual" alike.
func (s *DualTokenStore) SetUnusualAmount(timesUsual, fromPayments, historyDays int) {
	s.unusual = unusualAmount{timesUsual, fromPayments, historyDays}
}

// riskFlags adds the checks' flags to it, an item paying a saved payee: invoice is the invoice number it pays ("" for
// none), newPayee whether the payee was never paid before.
func (s *DualTokenStore) riskFlags(ctx context.Context, workspaceID string, it *PaymentRequestItem, invoice string, newPayee bool) error {
	if newPayee {
		it.Flags = append(it.Flags, ItemFlag{FlagFirstPayment, "the first payment to " + it.PayeeName})
	}
	// A payment that failed or came back did not pay the invoice.
	var ref, currency, on string
	var amount int64
	var sameInvoice bool
	err := s.pool.QueryRow(ctx, `SELECT reference, amount_minor, currency, to_char(created_at, 'YYYY-MM-DD'),
		$3 <> '' AND lower(reference) = lower($3) FROM money_payments
		WHERE workspace_id = $1 AND payee_id = $2 AND status NOT IN ('failed', 'returned')
		AND (($3 <> '' AND lower(reference) = lower($3)) OR (amount_minor = $4 AND currency = $5))
		ORDER BY 5 DESC, created_at DESC LIMIT 1`, workspaceID, it.PayeeID, invoice, it.AmountMinor, it.Currency).
		Scan(&ref, &amount, &currency, &on, &sameInvoice)
	switch {
	case err == nil && sameInvoice:
		it.Flags = append(it.Flags, ItemFlag{FlagDuplicate, fmt.Sprintf("invoice %s was paid to %s already, %s on %s",
			ref, it.PayeeName, formatMinor(currency, amount), on)})
	case err == nil:
		it.Flags = append(it.Flags, ItemFlag{FlagDuplicate, fmt.Sprintf("%s was paid the same amount, %s, already on %s (reference %q)",
			it.PayeeName, formatMinor(currency, amount), on, ref)})
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("economy: duplicate check: %w", err)
	}

	u := s.unusual
	if u.timesUsual <= 0 || newPayee {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT amount_minor FROM money_payments WHERE workspace_id = $1 AND payee_id = $2 AND currency = $3
		AND status NOT IN ('failed', 'returned') AND created_at >= now() - make_interval(days => $4)`,
		workspaceID, it.PayeeID, it.Currency, u.historyDays)
	if err != nil {
		return fmt.Errorf("economy: payee's usual: %w", err)
	}
	before, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("economy: payee's usual: %w", err)
	}
	if len(before) < max(u.fromPayments, 1) {
		return nil
	}
	// The median, as transaction monitoring takes it.
	slices.Sort(before)
	usual := before[len(before)/2]
	if len(before)%2 == 0 {
		usual = (before[len(before)/2-1] + usual) / 2
	}
	if usual > 0 && it.AmountMinor >= usual*int64(u.timesUsual) {
		it.Flags = append(it.Flags, ItemFlag{FlagUnusualAmount, fmt.Sprintf("at least %d times the usual payment to %s of %s (the median of its last %d)",
			u.timesUsual, it.PayeeName, formatMinor(it.Currency, usual), len(before))})
	}
	return nil
}

// approvalReason checks the reason a statement gives for the flagged items it approves.
func approvalReason(st ApprovalStatement) error {
	if len(st.Reason) > maxApprovalReason {
		return fmt.Errorf("%w: a reason is up to %d characters", ErrPaymentRequest, maxApprovalReason)
	}
	for _, it := range st.Items {
		if len(it.Overrides) > 0 && strings.TrimSpace(st.Reason) == "" {
			return fmt.Errorf("%w: item %d is flagged %s: approving it needs a reason, signed with the approval",
				ErrPaymentRequest, it.Position, strings.Join(it.Overrides, " and "))
		}
	}
	return nil
}
