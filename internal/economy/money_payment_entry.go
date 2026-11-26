package economy

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// money_payment_entry.go — B30.147: A PAYMENT'S LEDGER ENTRY, READABLE.
//
// A payment out's entry is the one that took its money out when it completed (movements): hold → the partner, the fee
// → Talyvor's revenue account, and an international payment's postings in the payee's currency through the route's
// conversion and payments accounts. It is read with every posting, each with its account's purpose, and what the
// postings sum to in each currency — zero in every one, or the entry would not have posted.

// ErrPaymentNotPosted: the payment has no entry yet — it is pending or sent, its money held — or never will: it failed.
var ErrPaymentNotPosted = errors.New("economy: the payment's entry is not posted")

// PaymentEntryPosting is one posting of a payment's entry.
type PaymentEntryPosting struct {
	AccountID   string `json:"account_id"`
	Purpose     string `json:"purpose"` // the account's: company, agent, hold, partner or revenue
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// PaymentEntrySum is what an entry's postings sum to in one currency.
type PaymentEntrySum struct {
	Currency string `json:"currency"`
	SumMinor int64  `json:"sum_minor"`
}

// PaymentEntry is a payment out's entry: its postings in order, and their sums per currency in the order the
// currencies first post.
type PaymentEntry struct {
	PaymentID string                `json:"payment_id"`
	EntryID   string                `json:"entry_id"`
	Kind      string                `json:"kind"`
	Funding   string                `json:"funding"`
	CreatedAt time.Time             `json:"created_at"`
	Postings  []PaymentEntryPosting `json:"postings"`
	Sums      []PaymentEntrySum     `json:"sums"`
}

// OutsidePaymentEntry reads workspaceID's payment id's entry — with agentID set, only that agent's payment's. A payment
// it does not find is ErrOutsidePaymentNotFound; one not yet completed, ErrPaymentNotPosted.
func (s *DualTokenStore) OutsidePaymentEntry(ctx context.Context, workspaceID, agentID, id string) (PaymentEntry, error) {
	p, err := scanOutsidePayment(s.pool.QueryRow(ctx, `SELECT `+outsidePaymentCols+` FROM money_payments
		WHERE id = $1 AND workspace_id = $2 AND ($3 = '' OR agent_id = $3)`, id, workspaceID, agentID))
	if err != nil {
		return PaymentEntry{}, err
	}
	if p.EntryID == "" {
		return PaymentEntry{}, fmt.Errorf("%w: payment %s is %s, and its entry is posted only once it completes", ErrPaymentNotPosted, p.ID, p.Status)
	}
	e := PaymentEntry{PaymentID: p.ID, EntryID: p.EntryID, Funding: p.Funding, Postings: []PaymentEntryPosting{}, Sums: []PaymentEntrySum{}}
	rows, err := s.pool.Query(ctx, `SELECT e.kind, e.created_at, p.account_id, a.purpose, p.amount_minor, p.currency
		FROM money_entries e JOIN money_postings p ON p.entry_id = e.id JOIN money_accounts a ON a.id = p.account_id
		WHERE e.id = $1 AND e.workspace_id = $2 ORDER BY p.line`, p.EntryID, workspaceID)
	if err != nil {
		return PaymentEntry{}, fmt.Errorf("economy: payment entry: %w", err)
	}
	defer rows.Close()
	sum := map[string]int{} // currency → its index in e.Sums
	for rows.Next() {
		var lp PaymentEntryPosting
		if err := rows.Scan(&e.Kind, &e.CreatedAt, &lp.AccountID, &lp.Purpose, &lp.AmountMinor, &lp.Currency); err != nil {
			return PaymentEntry{}, fmt.Errorf("economy: payment entry: %w", err)
		}
		e.Postings = append(e.Postings, lp)
		i, ok := sum[lp.Currency]
		if !ok {
			i, sum[lp.Currency] = len(e.Sums), len(e.Sums)
			e.Sums = append(e.Sums, PaymentEntrySum{Currency: lp.Currency})
		}
		e.Sums[i].SumMinor += lp.AmountMinor
	}
	return e, rows.Err()
}
