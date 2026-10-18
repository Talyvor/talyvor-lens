package market

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// invoices.go — B28.140 (B17.189): A BUYER'S MARKETPLACE BILL BY STRIPE INVOICE.
//
// A paid marketplace invoice is recorded with the billing period it charged for, its number and Stripe's PDF
// (market_invoices, 0254). What it charged and refunded is read off the uses it cleared, so the bill read for an invoice
// holds exactly those uses — a refunded one reading refunded — and totals the invoice less its refunds. The period in
// progress is listed first as "upcoming": the billed uses made since the last invoice's period ended, not yet paid.

// UpcomingInvoice is the id, and the status, of the period in progress.
const UpcomingInvoice = "upcoming"

// ErrNoInvoice: the buyer has no marketplace invoice of that id.
var ErrNoInvoice = errors.New("market: this workspace has no marketplace invoice of that id")

// Invoice is one invoice of a buyer's marketplace bill: what it charged, price and tax, and what of that was refunded
// since, in µUSD.
type Invoice struct {
	ID                string    `json:"id"`
	Number            string    `json:"number,omitempty"`
	PeriodStart       time.Time `json:"period_start"`
	PeriodEnd         time.Time `json:"period_end"`
	Status            string    `json:"status"` // paid, or upcoming
	InvoicePDF        string    `json:"invoice_pdf,omitempty"`
	GrossUSDMicros    int64     `json:"gross_usd_micros"`
	RefundedUSDMicros int64     `json:"refunded_usd_micros"`
}

// RecordMarketInvoice records a paid marketplace invoice of buyerWorkspaceID (billing.MarketInvoiceRecorder). A replay
// changes nothing.
func (s *Store) RecordMarketInvoice(ctx context.Context, buyerWorkspaceID, invoiceID, number, pdf string, periodStart, periodEnd,
	paidAt time.Time, livemode bool) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO market_invoices (id, buyer_workspace_id, number, period_start, period_end, invoice_pdf, livemode, paid_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (id) DO NOTHING`,
		invoiceID, buyerWorkspaceID, number, periodStart, periodEnd, pdf, livemode, paidAt); err != nil {
		return fmt.Errorf("market: record invoice %s: %w", invoiceID, err)
	}
	return nil
}

// Invoices lists buyerWorkspaceID's marketplace invoices newest first, the period in progress before them.
func (s *Store) Invoices(ctx context.Context, buyerWorkspaceID string) ([]Invoice, error) {
	up, err := s.InvoiceBill(ctx, buyerWorkspaceID, UpcomingInvoice)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT i.id, i.number, i.period_start, i.period_end, i.invoice_pdf,
		       COALESCE(sum(u.price_ulxc / $2 + COALESCE(t.tax_usd_micros, 0)), 0)::bigint,
		       COALESCE(sum(u.price_ulxc / $2 + COALESCE(t.tax_usd_micros, 0)) FILTER (WHERE r.use_id IS NOT NULL), 0)::bigint
		FROM market_invoices i
		LEFT JOIN market_uses u ON u.cleared_invoice_id = i.id AND u.buyer_workspace_id = i.buyer_workspace_id AND u.charge = 'billed'
		LEFT JOIN market_refunds r ON r.use_id = u.id
		LEFT JOIN market_tax_lines t ON t.use_id = u.id
		WHERE i.buyer_workspace_id = $1
		GROUP BY i.id ORDER BY i.period_end DESC, i.created_at DESC`, buyerWorkspaceID, ulxcPerUSDMicro)
	if err != nil {
		return nil, fmt.Errorf("market: invoices: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invoice, error) {
		x := Invoice{Status: "paid"}
		return x, row.Scan(&x.ID, &x.Number, &x.PeriodStart, &x.PeriodEnd, &x.InvoicePDF, &x.GrossUSDMicros, &x.RefundedUSDMicros)
	})
	if err != nil {
		return nil, fmt.Errorf("market: invoices: %w", err)
	}
	return append([]Invoice{*up.Invoice}, list...), nil
}

// InvoiceBill reads the bill for one of buyerWorkspaceID's invoices: the uses it carried, totalled as the invoice less its
// refunds. UpcomingInvoice reads the period in progress.
func (s *Store) InvoiceBill(ctx context.Context, buyerWorkspaceID, invoiceID string) (Bill, error) {
	inv := Invoice{ID: invoiceID, Status: "paid"}
	var b Bill
	var err error
	if invoiceID == UpcomingInvoice {
		// It began where the last invoice's period ended, or with the buyer's first use since none was paid.
		var last *time.Time
		if err := s.pool.QueryRow(ctx, `SELECT max(period_end) FROM market_invoices WHERE buyer_workspace_id = $1`, buyerWorkspaceID).
			Scan(&last); err != nil {
			return b, fmt.Errorf("market: the period in progress: %w", err)
		}
		since := time.Unix(0, 0).UTC()
		if last != nil {
			since = *last
		}
		b, err = s.bill(ctx, Bill{}, `u.ran_at IS NOT NULL AND u.cleared_at IS NULL AND u.used_at >= $2`, buyerWorkspaceID, since)
		inv.Status, inv.PeriodStart, inv.PeriodEnd = UpcomingInvoice, since, s.now().UTC()
		if last == nil {
			inv.PeriodStart = inv.PeriodEnd
			if len(b.Lines) > 0 {
				inv.PeriodStart = b.Lines[0].UsedAt
			}
		}
	} else {
		err = s.pool.QueryRow(ctx, `SELECT number, period_start, period_end, invoice_pdf FROM market_invoices WHERE id = $1 AND buyer_workspace_id = $2`,
			invoiceID, buyerWorkspaceID).Scan(&inv.Number, &inv.PeriodStart, &inv.PeriodEnd, &inv.InvoicePDF)
		if errors.Is(err, pgx.ErrNoRows) {
			return b, ErrNoInvoice
		}
		if err != nil {
			return b, fmt.Errorf("market: invoice %s: %w", invoiceID, err)
		}
		b, err = s.bill(ctx, Bill{}, `u.cleared_invoice_id = $2`, buyerWorkspaceID, invoiceID)
	}
	if err != nil {
		return b, err
	}
	b.Month = inv.PeriodStart.Format("2006-01")
	for _, l := range b.Lines {
		g := l.PriceULXC/ulxcPerUSDMicro + l.TaxUSDMicros
		inv.GrossUSDMicros += g
		if l.Refunded != nil {
			inv.RefundedUSDMicros += g
		}
	}
	b.Invoice = &inv
	return b, nil
}
