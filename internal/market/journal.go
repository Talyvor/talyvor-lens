package market

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// journal.go — B32.16: THE MARKETPLACE JOURNAL.
//
// Every clearing and refund of a marketplace sale is an entry of double-entry postings (migration 0199), in
// micro-dollars with the currency on every posting, because a use can cost less than a cent. Debits are
// positive and credits negative; the database refuses, when the transaction commits, an entry whose postings
// do not sum to zero per currency, and refuses any change to a posting. market_journal_balances keeps each
// account's total in the same transaction.
//
//   - clear: a paid invoice clears a use — +gross+tax stripe:clearing, −fee revenue:market_fee, −share
//     seller:<ws>:holdback for each payee: the seller and every ancestor its royalties pay (B32.26), and −tax
//     tax:<jurisdiction> (B32.39). ClearInvoice posts it beside the earnings.
//   - reversal: a refund or chargeback of a cleared use posts the exact mirror of its clear entry. A trigger on
//     market_refunds posts it in the transaction that writes the refund, so every writer of one —
//     ReverseInvoice, refundUses, a test-money crossing's reversal — is journalled without being touched. Once
//     the earning was released, the seller's share comes back out of available instead (migration 0200).
//   - release, payout, credits: the holdback's escrow and what leaves it (B32.17, escrow.go and payout.go).
//   - self_bill: the VAT a seller charges Talyvor on a payout's self-billed invoice — −VAT seller:<ws>:available,
//     +VAT tax:<jurisdiction>:input (B32.43, selfbill.go).

// The journal's accounts. A seller has two: SellerHoldback and SellerAvailable.
const (
	AccountStripeClearing = "stripe:clearing"
	AccountMarketFee      = "revenue:market_fee"
	AccountRoundingStripe = "rounding:stripe"
	AccountConnectFees    = "stripe:connect_fees"
	AccountCreditsIssued  = "credits:issued"
	AccountMoneyClearing  = "money:clearing" // payouts made through the money layer (B46.22)
)

// The kinds of journal entry.
const (
	JournalClear    = "clear"
	JournalReversal = "reversal"
	JournalRelease  = "release"
	JournalPayout   = "payout"
	JournalCredits  = "credits"
	JournalSelfBill = "self_bill"
)

// SellerHoldback is the seller's account for earnings still inside the 14-day holdback.
func SellerHoldback(workspaceID string) string { return "seller:" + workspaceID + ":holdback" }

// SellerAvailable is the seller's account for earnings past the holdback and not yet paid out.
func SellerAvailable(workspaceID string) string { return "seller:" + workspaceID + ":available" }

// Posting is one line of a journal entry.
type Posting struct {
	Account         string `json:"account"`
	AmountUSDMicros int64  `json:"amount_usd_micros"` // a debit is positive, a credit negative
	Currency        string `json:"currency"`          // "" is USD
	Funding         string `json:"funding"`           // test or live; "" when posting: the entry's
}

// funding is test or live as the money was (B22.1): live only when a live-mode invoice paid a real workspace.
func funding(livemode, test bool) string {
	if livemode && !test {
		return "live"
	}
	return "test"
}

// PostJournalTx writes one entry of kind for ref, and its postings, on tx — the caller's transaction, so the
// entry commits or rolls back with the rows it explains. Each posting is funded by fundedBy unless it names its
// own funding. A posting of zero is left out, and an entry of none is not written: it answers "" then, and the new
// entry's id otherwise. An entry that does not balance is refused when tx commits.
func PostJournalTx(ctx context.Context, tx pgx.Tx, kind, ref, fundedBy string, at time.Time, postings ...Posting) (string, error) {
	var sql strings.Builder
	args := []any{}
	for _, p := range postings {
		if p.AmountUSDMicros == 0 {
			continue
		}
		if p.Currency == "" {
			p.Currency = "USD"
		}
		if p.Funding == "" {
			p.Funding = fundedBy
		}
		if len(args) == 0 {
			args = append(args, "mje_"+uuid.NewString())
			sql.WriteString(`INSERT INTO market_journal_postings (entry_id, line, account, amount_usd_micros, currency, funding) VALUES `)
		} else {
			sql.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&sql, "($1, %d, $%d, $%d, $%d, $%d)", (n-1)/4+1, n+1, n+2, n+3, n+4)
		args = append(args, p.Account, p.AmountUSDMicros, p.Currency, p.Funding)
	}
	if len(args) == 0 {
		return "", nil
	}
	id := args[0].(string)
	if _, err := tx.Exec(ctx, `INSERT INTO market_journal_entries (id, kind, ref, created_at) VALUES ($1, $2, $3, $4)`, id, kind, ref, at); err != nil {
		return "", fmt.Errorf("market: journal %s %s: %w", kind, ref, err)
	}
	if _, err := tx.Exec(ctx, sql.String(), args...); err != nil {
		return "", fmt.Errorf("market: journal %s %s: %w", kind, ref, err)
	}
	return id, nil
}

// postClearTx journals the clearing of one use: the buyer paid collected (the price and its tax), Talyvor keeps fee,
// and the rest waits in the holdback of each of its payees — the seller, and the ancestors its lineage royalties pay
// (B32.26) — or is owed to the tax authority (B32.39).
func postClearTx(ctx context.Context, tx pgx.Tx, useID string, collected, fee int64, fundedBy string, at time.Time, payees ...Posting) error {
	_, err := PostJournalTx(ctx, tx, JournalClear, useID, fundedBy, at, append([]Posting{
		{Account: AccountStripeClearing, AmountUSDMicros: collected},
		{Account: AccountMarketFee, AmountUSDMicros: -fee}}, payees...)...)
	return err
}

// JournalBalance reads an account's balance in currency ("" is USD), test and live money together: the sum of its
// postings, kept by market_journal_balances as each is written.
func (s *Store) JournalBalance(ctx context.Context, account, currency string) (int64, error) {
	if currency == "" {
		currency = "USD"
	}
	var b int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_usd_micros), 0)::bigint FROM market_journal_balances
		WHERE account = $1 AND currency = $2`, account, currency).Scan(&b); err != nil {
		return 0, fmt.Errorf("market: journal balance of %s: %w", account, err)
	}
	return b, nil
}

// JournalEntry is one entry of the marketplace journal and its postings.
type JournalEntry struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Ref       string    `json:"ref"`
	Funding   string    `json:"funding"` // test, live, or "test and live" (earnings taken as credits can be both)
	CreatedAt time.Time `json:"created_at"`
	Postings  []Posting `json:"postings"`
}

// JournalFor reads the journal entries that explain ref (a use: its clear, and its reversal if it was refunded),
// oldest first.
func (s *Store) JournalFor(ctx context.Context, ref string) ([]JournalEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.id, e.kind, e.ref, e.created_at, p.funding, p.account, p.amount_usd_micros, p.currency
		FROM market_journal_entries e JOIN market_journal_postings p ON p.entry_id = e.id
		WHERE e.ref = $1 ORDER BY e.created_at, e.id, p.line`, ref)
	if err != nil {
		return nil, fmt.Errorf("market: journal of %s: %w", ref, err)
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var p Posting
		if err := rows.Scan(&e.ID, &e.Kind, &e.Ref, &e.CreatedAt, &p.Funding, &p.Account, &p.AmountUSDMicros, &p.Currency); err != nil {
			return nil, fmt.Errorf("market: journal of %s: %w", ref, err)
		}
		e.Funding = p.Funding
		if len(out) == 0 || out[len(out)-1].ID != e.ID {
			out = append(out, e)
		} else if out[len(out)-1].Funding != p.Funding {
			out[len(out)-1].Funding = "test and live"
		}
		out[len(out)-1].Postings = append(out[len(out)-1].Postings, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("market: journal of %s: %w", ref, err)
	}
	return out, nil
}
