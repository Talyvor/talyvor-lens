package market

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// statement.go — B32.42: EVERY WEEKLY PAYOUT HAS A STATEMENT OF ITS WEEK.
//
// A seller's statement for a week (an ISO week, UTC, such as 2026-W41) is read from the marketplace journal: what
// moved through their available balance that week, and where it came from. It opens with what was brought forward
// from earlier weeks; adds the sales released to them from the holdback that week, less Talyvor's fee and the
// royalties those sales paid to the originals they remix, and the royalties and split shares released to them from
// others' sales; takes off refunds and chargebacks, earnings taken as credits and what is carried forward to the next
// week; and ends with Stripe's fees. Its lines sum to the net its payout paid — 0 in a week without one, when it is
// all carried forward. The VAT Talyvor collected from the buyers of those sales is shown for information only: it
// was never the seller's, and is not a line.
//
// To a seller who agreed to self-billing, the week's payout is also a self-billed invoice from them to Talyvor (B32.43,
// selfbill.go): the statement carries it, and the VAT the seller charged on it is a line, paid with the payout.

// The lines of a statement, in order.
const (
	LineBroughtForward    = "brought_forward"
	LineSales             = "sales"
	LineTalyvorFee        = "talyvor_fee"
	LineRoyaltiesPaid     = "royalties_paid"
	LineRoyaltiesReceived = "royalties_received"
	LineRefunds           = "refunds"
	LineCredits           = "credits"
	LineSupplyVAT         = "supply_vat"
	LineOther             = "other"
	LineCarriedForward    = "carried_forward"
	LineStripeFees        = "stripe_fees"
)

// StatementLine is one line of a statement: what it adds to the net, or (negative) takes from it.
type StatementLine struct {
	Kind            string `json:"kind"`
	Label           string `json:"label"`
	AmountUSDMicros int64  `json:"amount_usd_micros"`
}

// Statement is a seller's statement for one week.
type Statement struct {
	Period string    `json:"period"` // the ISO week, such as 2026-W41
	From   time.Time `json:"from"`   // its Monday, 00:00 UTC
	To     time.Time `json:"to"`     // the next Monday, 00:00 UTC
	// Payout is the week's payout in money, if the seller was paid: under the minimum, they are carried forward.
	Payout *Payout         `json:"payout"`
	Sales  int64           `json:"sales"` // how many of the seller's sales were released to them this week
	Lines  []StatementLine `json:"lines"`
	// NetUSDMicros is the sum of the lines: what the week's payout paid.
	NetUSDMicros int64 `json:"net_usd_micros"`
	// VATCollectedUSDMicros is, for information, the VAT Talyvor collected from the buyers of the sales released this
	// week, owed to the tax authorities and not part of the net.
	VATCollectedUSDMicros int64 `json:"vat_collected_usd_micros"`
	// SelfBilledInvoice is the week's payout as a self-billed invoice from the seller to Talyvor: nil when the seller
	// has not agreed to self-billing, or was not paid (B32.43).
	SelfBilledInvoice *SelfBill `json:"self_billed_invoice"`
}

// StatementSummary is one week a seller was paid in, for the list of their statements.
type StatementSummary struct {
	Period       string     `json:"period"`
	PayoutID     string     `json:"payout_id"`
	NetUSDMicros int64      `json:"net_usd_micros"`
	PaidAt       *time.Time `json:"paid_at"`
}

var isoWeek = regexp.MustCompile(`^(\d{4})-W(\d{2})$`)

// WeekBounds is the Monday 00:00 UTC that starts the ISO week period (such as 2026-W41) and the next one.
func WeekBounds(period string) (from, to time.Time, err error) {
	m := isoWeek.FindStringSubmatch(period)
	if m == nil {
		return from, to, invalid("period must be an ISO week, such as 2026-W41")
	}
	year, _ := strconv.Atoi(m[1])
	week, _ := strconv.Atoi(m[2])
	// 4 January is always in week 1.
	jan4 := time.Date(year, time.January, 4, 0, 0, 0, 0, time.UTC)
	from = jan4.AddDate(0, 0, -(int(jan4.Weekday())+6)%7+(week-1)*7)
	if week < 1 || weekOf(from) != period {
		return time.Time{}, time.Time{}, invalid("%d has no week %d", year, week)
	}
	return from, from.AddDate(0, 0, 7), nil
}

// SellerStatement reads workspaceID's statement for period from the journal.
func (s *Store) SellerStatement(ctx context.Context, workspaceID, period string) (Statement, error) {
	from, to, err := WeekBounds(period)
	if err != nil {
		return Statement{}, err
	}
	st := Statement{Period: period, From: from, To: to}
	available := SellerAvailable(workspaceID)
	// Every posting on the seller's available balance: before the week, and in it by the kind of entry it is in. A
	// debit is positive, so what the seller has is the negative of the postings' sum.
	var before, released, reversed, credits, paid, selfBilled, other, fees int64
	if err := s.pool.QueryRow(ctx, `SELECT
		COALESCE(sum(p.amount_usd_micros) FILTER (WHERE j.created_at < $2), 0)::bigint,
		COALESCE(sum(p.amount_usd_micros) FILTER (WHERE j.created_at >= $2 AND j.kind = 'release'), 0)::bigint,
		COALESCE(sum(p.amount_usd_micros) FILTER (WHERE j.created_at >= $2 AND j.kind = 'reversal'), 0)::bigint,
		COALESCE(sum(p.amount_usd_micros) FILTER (WHERE j.created_at >= $2 AND j.kind = 'credits'), 0)::bigint,
		COALESCE(sum(p.amount_usd_micros) FILTER (WHERE j.created_at >= $2 AND j.kind = 'payout'), 0)::bigint,
		COALESCE(sum(p.amount_usd_micros) FILTER (WHERE j.created_at >= $2 AND j.kind = 'self_bill'), 0)::bigint,
		COALESCE(sum(p.amount_usd_micros) FILTER (WHERE j.created_at >= $2 AND j.kind NOT IN ('release', 'reversal', 'credits', 'payout', 'self_bill')), 0)::bigint
		FROM market_journal_postings p JOIN market_journal_entries j ON j.id = p.entry_id
		WHERE p.account = $1 AND j.created_at < $3`, available, from, to).
		Scan(&before, &released, &reversed, &credits, &paid, &selfBilled, &other); err != nil {
		return st, fmt.Errorf("market: statement %s: %w", period, err)
	}
	// Stripe's fees, from the payout's own entry.
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(-p.amount_usd_micros), 0)::bigint
		FROM market_journal_postings p JOIN market_journal_entries j ON j.id = p.entry_id
		WHERE j.kind = 'payout' AND j.created_at >= $2 AND j.created_at < $3 AND p.account = $4
		  AND EXISTS (SELECT 1 FROM market_journal_postings a WHERE a.entry_id = j.id AND a.account = $1)`,
		available, from, to, AccountConnectFees).Scan(&fees); err != nil {
		return st, fmt.Errorf("market: statement %s: %w", period, err)
	}
	// The seller's own sales released this week, from their earnings: the price before tax, Talyvor's fee, and what
	// the seller kept after the royalties up its family tree; and the VAT the buyer paid on it, from its clear entry.
	var gross, fee, kept, vat int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(e.gross_usd_micros), 0)::bigint, COALESCE(sum(e.fee_usd_micros), 0)::bigint,
		COALESCE(sum(e.share_usd_micros), 0)::bigint,
		COALESCE(sum((SELECT -sum(t.amount_usd_micros) FROM market_journal_postings t JOIN market_journal_entries c ON c.id = t.entry_id
			WHERE c.kind = 'clear' AND c.ref = e.use_id AND t.account LIKE 'tax:%')), 0)::bigint
		FROM market_earnings e JOIN market_journal_entries j ON j.kind = 'release' AND j.ref = e.use_id
		WHERE e.seller_workspace_id = $1 AND e.kind = 'sale' AND j.created_at >= $2 AND j.created_at < $3`,
		workspaceID, from, to).Scan(&st.Sales, &gross, &fee, &kept, &vat); err != nil {
		return st, fmt.Errorf("market: statement %s: %w", period, err)
	}
	st.VATCollectedUSDMicros = vat
	st.Lines = []StatementLine{
		{LineBroughtForward, "Brought forward from earlier weeks", -before},
		{LineSales, "Sales", gross},
		{LineTalyvorFee, "Talyvor's fee", -fee},
		{LineRoyaltiesPaid, "Royalties paid to the originals your listings build on", -(gross - fee - kept)},
		{LineRoyaltiesReceived, "Royalties and split shares received", -released - kept},
		{LineRefunds, "Refunds and chargebacks", -reversed},
		{LineCredits, "Taken as Talyvor credits", -credits},
	}
	var p Payout
	err = s.pool.QueryRow(ctx, `SELECT id, method, month, period, gross_usd_micros, vat_usd_micros, account_fee_usd_micros, payout_fee_usd_micros,
		       net_usd_micros, credits_ulxc, COALESCE(stripe_transfer_id, ''), paid_at, last_error, created_at
		FROM market_payouts WHERE workspace_id = $1 AND method IN ('stripe', 'money') AND period = $2`, workspaceID, period).
		Scan(&p.ID, &p.Method, &p.Month, &p.Period, &p.GrossUSDMicros, &p.VATUSDMicros, &p.AccountFeeUSDMicros, &p.PayoutFeeUSDMicros,
			&p.NetUSDMicros, &p.CreditsULXC, &p.StripeTransferID, &p.PaidAt, &p.LastError, &p.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return st, fmt.Errorf("market: statement %s: %w", period, err)
	default:
		st.Payout = &p
		bill, err := s.SelfBillOf(ctx, workspaceID, p.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return st, fmt.Errorf("market: statement %s: %w", period, err)
		default:
			st.SelfBilledInvoice = &bill
		}
	}
	// The VAT on the seller's supply, paid with the payout (B32.43).
	if st.SelfBilledInvoice != nil || selfBilled != 0 {
		label := "VAT on your supply"
		if st.SelfBilledInvoice != nil && !st.SelfBilledInvoice.VATEnabled {
			label = VATUnderReview
		}
		st.Lines = append(st.Lines, StatementLine{LineSupplyVAT, label, -selfBilled})
	}
	if other != 0 {
		st.Lines = append(st.Lines, StatementLine{LineOther, "Other adjustments", -other})
	}
	st.Lines = append(st.Lines,
		StatementLine{LineCarriedForward, "Carried forward to next week", before + released + reversed + credits + paid + selfBilled + other},
		StatementLine{LineStripeFees, "Stripe's fees, at cost", -fees})
	for _, l := range st.Lines {
		st.NetUSDMicros += l.AmountUSDMicros
	}
	return st, nil
}

// SellerStatements lists the weeks workspaceID was paid in money, newest first.
func (s *Store) SellerStatements(ctx context.Context, workspaceID string) ([]StatementSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT period, id, net_usd_micros, paid_at FROM market_payouts
		WHERE workspace_id = $1 AND method IN ('stripe', 'money') AND period <> '' ORDER BY period DESC LIMIT 104`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("market: statements: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatementSummary, error) {
		var x StatementSummary
		return x, row.Scan(&x.Period, &x.PayoutID, &x.NetUSDMicros, &x.PaidAt)
	})
	if err != nil {
		return nil, fmt.Errorf("market: statements: %w", err)
	}
	return list, nil
}
