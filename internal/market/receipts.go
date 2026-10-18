package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/taxprofile"
)

// receipts.go — B32.40: A VAT RECEIPT FOR EVERY PAID MARKETPLACE BILL, ISSUED BY TALYVOR AS THE SUPPLIER.
//
// Under the deemed-supplier rules Talyvor is the supplier of every marketplace sale, so the receipt for a buyer's paid
// marketplace bill is Talyvor's. When the invoice is paid (invoice.paid, or a test bill paid here, B25.7) and its uses
// have cleared, IssueReceipt writes one receipt for it: the next number of its year from a gap-free sequence, the
// supplier (LENS_SUPPLIER_*) and the buyer (its tax profile, B32.38) as they are at that moment, one line per use the
// invoice cleared with its net, rate and tax (market_tax_lines, B32.39), the totals in US dollars, the tax also in the
// buyer's currency at its jurisdiction's rate source, and the reverse-charge note where it applies. A receipt is never
// changed. Until Talyvor's VAT number is set it reads "VAT registration pending" and is a Preview, as is every receipt
// for test money.

// ErrNoReceipt: no receipt of this workspace has that id.
var ErrNoReceipt = errors.New("market: no receipt of this workspace has that id")

// errNoReceiptLines: the invoice cleared no use, so there is nothing to give a receipt for.
var errNoReceiptLines = errors.New("market: the invoice cleared no use")

// VATRegistrationPending is what a receipt prints for Talyvor's VAT number until it is set.
const VATRegistrationPending = "VAT registration pending"

// Supplier is who a marketplace receipt is issued by: Talyvor, as the seller of record.
type Supplier struct {
	LegalName string `json:"legal_name"`
	Address   string `json:"address"`
	VATNumber string `json:"vat_number"` // "" until Talyvor's VAT registration is set: the receipt is a Preview
}

// ReceiptBuyers reads the buyer's legal name, address and VAT number: *taxprofile.Store.
type ReceiptBuyers interface {
	Get(ctx context.Context, workspaceID string) (taxprofile.Profile, error)
}

// ReceiptRates prices a receipt's tax in the buyer's currency: *ecbrate.Book.
type ReceiptRates interface {
	FromUSD(ctx context.Context, usdMicros int64, currency string, at time.Time) (ecbrate.Converted, error)
}

// Receipts is what the marketplace issues receipts with.
type Receipts struct {
	Supplier Supplier
	Buyers   ReceiptBuyers // nil: the buyer is printed by its workspace's name
	Rates    ReceiptRates  // nil: no tax in the buyer's currency
	AppURL   string        // the app's origin, where a test bill's invoice links its receipt's PDF (B28.140); "": it links none
}

// SetReceipts turns receipts on: every marketplace bill paid from now on gets one.
func (s *Store) SetReceipts(r Receipts) { s.receipts = &r }

// ReceiptBuyer is who a receipt is issued to, as they were when it was issued.
type ReceiptBuyer struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Address     string `json:"address,omitempty"`
	PostalCode  string `json:"postal_code,omitempty"`
	Country     string `json:"country,omitempty"`
	Business    bool   `json:"business"`
	VATNumber   string `json:"vat_number,omitempty"` // only a number the tax partner found valid
}

// ReceiptLine is one use the paid invoice cleared.
type ReceiptLine struct {
	UseID        string `json:"use_id"`
	Description  string `json:"description"`
	NetUSDMicros int64  `json:"net_usd_micros"`
	RateBps      int    `json:"rate_bps"`
	TaxUSDMicros int64  `json:"tax_usd_micros"`
	Treatment    string `json:"treatment,omitempty"`
	Jurisdiction string `json:"jurisdiction,omitempty"`
	Note         string `json:"note,omitempty"`
}

// Receipt is Talyvor's receipt for one paid marketplace bill.
type Receipt struct {
	ID               string    `json:"id"`
	Number           string    `json:"number"`   // as printed: 2026-000001, or TEST-2026-000001 for test money
	Sequence         int       `json:"sequence"` // its place in its year's run: 1 for the year's first
	Year             int       `json:"year"`
	Series           string    `json:"series"` // live or test
	InvoiceID        string    `json:"invoice_id"`
	BuyerWorkspaceID string    `json:"buyer_workspace_id"`
	IssuedAt         time.Time `json:"issued_at"`
	PaidAt           time.Time `json:"paid_at"`

	Supplier Supplier      `json:"supplier"`
	Buyer    ReceiptBuyer  `json:"buyer"`
	Lines    []ReceiptLine `json:"lines"`

	NetUSDMicros   int64  `json:"net_usd_micros"`
	TaxUSDMicros   int64  `json:"tax_usd_micros"`
	GrossUSDMicros int64  `json:"gross_usd_micros"`
	GrossCents     int64  `json:"gross_cents"`
	StripeTotal    *int64 `json:"stripe_total_cents"`             // the paid invoice's total, as Stripe reported it
	MatchesStripe  *bool  `json:"matches_stripe_total,omitempty"` // whether GrossCents is it, to the cent

	TaxLocal      *ecbrate.Converted `json:"tax_local"` // the tax in the buyer's currency; nil before a rate is published
	ReverseCharge bool               `json:"reverse_charge"`
	Notes         []string           `json:"notes"`

	Preview       bool   `json:"preview"`
	PreviewReason string `json:"preview_reason,omitempty"`
}

// ReceiptSummary is one receipt in a workspace's list.
type ReceiptSummary struct {
	ID             string    `json:"id"`
	Number         string    `json:"number"`
	InvoiceID      string    `json:"invoice_id"`
	IssuedAt       time.Time `json:"issued_at"`
	GrossUSDMicros int64     `json:"gross_usd_micros"`
	TaxUSDMicros   int64     `json:"tax_usd_micros"`
}

// receiptNumber is a receipt's number as printed.
func receiptNumber(series string, year, n int) string {
	if series == "test" {
		return fmt.Sprintf("TEST-%d-%06d", year, n)
	}
	return fmt.Sprintf("%d-%06d", year, n)
}

// useKindLabels says what a cleared use was, after its listing's title.
var useKindLabels = map[string]string{"buy": "purchase", "rent": "rental", "subscribe": "subscription", "renewal": "renewal",
	"prize": "prize"}

// IssueMarketReceipt issues the receipt for a paid marketplace invoice (billing.MarketReceipter), answering its id; ""
// when the invoice cleared no use, or receipts are not turned on. Issued once: a replay answers the receipt already
// issued.
func (s *Store) IssueMarketReceipt(ctx context.Context, buyerWorkspaceID, invoiceID string, paidAt time.Time, livemode bool,
	stripeTotalCents *int64) (string, error) {
	if s.receipts == nil {
		return "", nil
	}
	r, err := s.IssueReceipt(ctx, buyerWorkspaceID, invoiceID, paidAt, livemode, stripeTotalCents)
	if errors.Is(err, errNoReceiptLines) {
		return "", nil
	}
	return r.ID, err
}

// IssueReceipt issues the receipt for invoiceID, which buyerWorkspaceID paid at paidAt: one line per use it cleared.
// Its number is the next of its series and year, taken in the transaction that writes it, so the run has no gaps.
// An invoice already given a receipt answers that receipt and issues nothing.
func (s *Store) IssueReceipt(ctx context.Context, buyerWorkspaceID, invoiceID string, paidAt time.Time, livemode bool,
	stripeTotalCents *int64) (Receipt, error) {
	if s.receipts == nil {
		return Receipt{}, errors.New("market: receipts are not configured")
	}
	if id, err := s.receiptOfInvoice(ctx, invoiceID); err == nil {
		return s.Receipt(ctx, buyerWorkspaceID, id)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, err
	}
	lines, err := s.receiptLines(ctx, buyerWorkspaceID, invoiceID)
	if err != nil {
		return Receipt{}, err
	}
	if len(lines) == 0 {
		return Receipt{}, errNoReceiptLines
	}
	buyer, err := s.receiptBuyer(ctx, buyerWorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	var tax int64
	for _, l := range lines {
		tax += l.TaxUSDMicros
	}
	local, err := s.localTax(ctx, receiptJurisdiction(lines, buyer), tax, paidAt)
	if err != nil {
		return Receipt{}, err
	}
	supplier, err := json.Marshal(s.receipts.Supplier)
	if err != nil {
		return Receipt{}, err
	}
	buyerJSON, err := json.Marshal(buyer)
	if err != nil {
		return Receipt{}, err
	}
	series := "test"
	if livemode {
		series = "live"
	}
	issuedAt := s.now().UTC()
	id := "rcpt_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	errTaken := errors.New("taken")
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `INSERT INTO market_receipt_sequences (series, year, last_number) VALUES ($1, $2, 1)
			ON CONFLICT (series, year) DO UPDATE SET last_number = market_receipt_sequences.last_number + 1
			RETURNING last_number`, series, issuedAt.Year()).Scan(&n); err != nil {
			return err
		}
		var lc, lr, ls *string
		var lm *int64
		var ld *time.Time
		if local != nil {
			lc, lm, lr, ld, ls = &local.Currency, &local.AmountMinor, &local.Rate, local.RateDate, &local.Source
		}
		tag, err := tx.Exec(ctx, `INSERT INTO market_receipts (id, series, year, number, invoice_id, buyer_workspace_id, issued_at, paid_at,
			supplier, buyer, stripe_total_cents, tax_local_currency, tax_local_minor, tax_local_rate, tax_local_rate_date, tax_local_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) ON CONFLICT (invoice_id) DO NOTHING`,
			id, series, issuedAt.Year(), n, invoiceID, buyerWorkspaceID, issuedAt, paidAt, string(supplier), string(buyerJSON), stripeTotalCents,
			lc, lm, lr, ld, ls)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errTaken // issued meanwhile: this transaction gives its number back
		}
		for i, l := range lines {
			if _, err := tx.Exec(ctx, `INSERT INTO market_receipt_lines (receipt_id, position, use_id, description, net_usd_micros, rate_bps,
				tax_usd_micros, treatment, jurisdiction, note) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				id, i+1, l.UseID, l.Description, l.NetUSDMicros, l.RateBps, l.TaxUSDMicros, l.Treatment, l.Jurisdiction, l.Note); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errTaken) {
		if id, err = s.receiptOfInvoice(ctx, invoiceID); err != nil {
			return Receipt{}, err
		}
	} else if err != nil {
		return Receipt{}, fmt.Errorf("market: issue the receipt for %s: %w", invoiceID, err)
	}
	return s.Receipt(ctx, buyerWorkspaceID, id)
}

func (s *Store) receiptOfInvoice(ctx context.Context, invoiceID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM market_receipts WHERE invoice_id = $1`, invoiceID).Scan(&id)
	return id, err
}

// receiptLines are the uses invoiceID cleared, in the order they were used, each with its tax line.
func (s *Store) receiptLines(ctx context.Context, buyerWorkspaceID, invoiceID string) ([]ReceiptLine, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id, COALESCE(l.title, 'Payment to ' || a.name, 'Marketplace charge'), u.use_kind, u.price_ulxc,
		       COALESCE(t.rate_bps, 0), COALESCE(t.tax_usd_micros, 0), COALESCE(t.treatment, ''), COALESCE(t.jurisdiction, ''), COALESCE(t.note, '')
		FROM market_uses u LEFT JOIN market_listings l ON l.id = u.listing_id
		LEFT JOIN agent_accounts a ON a.id = u.payee_agent_id AND u.payee_agent_id <> ''
		LEFT JOIN market_tax_lines t ON t.use_id = u.id
		WHERE u.buyer_workspace_id = $1 AND u.cleared_invoice_id = $2
		ORDER BY u.used_at, u.id`, buyerWorkspaceID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("market: receipt lines: %w", err)
	}
	defer rows.Close()
	var lines []ReceiptLine
	for rows.Next() {
		var l ReceiptLine
		var kind string
		var ulxc int64
		if err := rows.Scan(&l.UseID, &l.Description, &kind, &ulxc, &l.RateBps, &l.TaxUSDMicros, &l.Treatment, &l.Jurisdiction, &l.Note); err != nil {
			return nil, err
		}
		if label, ok := useKindLabels[kind]; ok {
			l.Description += " — " + label
		}
		l.NetUSDMicros = ulxc / ulxcPerUSDMicro
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

// receiptBuyer is the buyer as its tax profile names it, or by its workspace's name without one.
func (s *Store) receiptBuyer(ctx context.Context, workspaceID string) (ReceiptBuyer, error) {
	b := ReceiptBuyer{WorkspaceID: workspaceID}
	if s.receipts.Buyers != nil {
		p, err := s.receipts.Buyers.Get(ctx, workspaceID)
		switch {
		case err == nil:
			b.Name, b.Address, b.PostalCode, b.Country, b.Business = p.LegalName, p.Address, p.PostalCode, p.Country, p.Business
			if p.TaxIDValid {
				b.VATNumber = p.TaxID
			}
			return b, nil
		case !errors.Is(err, taxprofile.ErrNotFound):
			return b, err
		}
	}
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), '')`, workspaceID).Scan(&b.Name)
	return b, err
}

// receiptJurisdiction is the buyer's tax jurisdiction: the first a line was taxed in, else its profile's country.
func receiptJurisdiction(lines []ReceiptLine, buyer ReceiptBuyer) string {
	for _, l := range lines {
		if l.Jurisdiction != "" {
			return l.Jurisdiction
		}
	}
	return buyer.Country
}

// localTax is taxUSDMicros in jurisdiction's currency at its rate source on the day the bill was paid; nil when the
// jurisdiction is unknown or loaded with no currency, its rate source is not the ECB's, or no rate is published yet.
func (s *Store) localTax(ctx context.Context, jurisdiction string, taxUSDMicros int64, paidAt time.Time) (*ecbrate.Converted, error) {
	if s.receipts.Rates == nil || jurisdiction == "" {
		return nil, nil
	}
	var currency, source string
	err := s.pool.QueryRow(ctx, `SELECT currency, rate_source FROM tax_jurisdictions WHERE code = $1`, jurisdiction).Scan(&currency, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if source != "ecb" && currency != "USD" {
		return nil, nil
	}
	c, err := s.receipts.Rates.FromUSD(ctx, taxUSDMicros, currency, paidAt)
	if errors.Is(err, ecbrate.ErrNoRate) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Receipt reads one of buyerWorkspaceID's receipts.
func (s *Store) Receipt(ctx context.Context, buyerWorkspaceID, id string) (Receipt, error) {
	r := Receipt{Lines: []ReceiptLine{}, Notes: []string{}}
	var supplier, buyer []byte
	var lc, lr, ls *string
	var lm *int64
	var ld *time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, series, year, number, invoice_id, buyer_workspace_id, issued_at, paid_at, supplier, buyer,
		       stripe_total_cents, tax_local_currency, tax_local_minor, tax_local_rate, tax_local_rate_date, tax_local_source
		FROM market_receipts WHERE id = $1 AND buyer_workspace_id = $2`, id, buyerWorkspaceID).
		Scan(&r.ID, &r.Series, &r.Year, &r.Sequence, &r.InvoiceID, &r.BuyerWorkspaceID, &r.IssuedAt, &r.PaidAt, &supplier, &buyer,
			&r.StripeTotal, &lc, &lm, &lr, &ld, &ls)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNoReceipt
	}
	if err != nil {
		return r, fmt.Errorf("market: receipt: %w", err)
	}
	if err := json.Unmarshal(supplier, &r.Supplier); err != nil {
		return r, err
	}
	if err := json.Unmarshal(buyer, &r.Buyer); err != nil {
		return r, err
	}
	r.Number = receiptNumber(r.Series, r.Year, r.Sequence)
	if lc != nil && lm != nil {
		r.TaxLocal = &ecbrate.Converted{Currency: *lc, AmountMinor: *lm, RateDate: ld}
		if lr != nil {
			r.TaxLocal.Rate = *lr
		}
		if ls != nil {
			r.TaxLocal.Source = *ls
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT use_id, description, net_usd_micros, rate_bps, tax_usd_micros, treatment, jurisdiction, note
		FROM market_receipt_lines WHERE receipt_id = $1 ORDER BY position`, id)
	if err != nil {
		return r, fmt.Errorf("market: receipt lines: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var l ReceiptLine
		if err := rows.Scan(&l.UseID, &l.Description, &l.NetUSDMicros, &l.RateBps, &l.TaxUSDMicros, &l.Treatment, &l.Jurisdiction, &l.Note); err != nil {
			return r, err
		}
		r.Lines = append(r.Lines, l)
		r.NetUSDMicros += l.NetUSDMicros
		r.TaxUSDMicros += l.TaxUSDMicros
		if l.Treatment == string(partners.TaxReverseCharge) {
			r.ReverseCharge = true
		}
		if l.Note != "" && !seen[l.Note] {
			seen[l.Note] = true
			r.Notes = append(r.Notes, l.Note)
		}
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	r.GrossUSDMicros = r.NetUSDMicros + r.TaxUSDMicros
	r.GrossCents = (r.GrossUSDMicros + 5_000) / 10_000
	if r.StripeTotal != nil {
		match := *r.StripeTotal == r.GrossCents
		r.MatchesStripe = &match
	}
	switch {
	case r.Series == "test":
		r.Preview, r.PreviewReason = true, "Preview — test money only"
	case r.Supplier.VATNumber == "":
		r.Preview, r.PreviewReason = true, "Preview — "+VATRegistrationPending
	}
	return r, nil
}

// ReceiptsOf lists buyerWorkspaceID's receipts, newest first.
func (s *Store) ReceiptsOf(ctx context.Context, buyerWorkspaceID string) ([]ReceiptSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id, r.series, r.year, r.number, r.invoice_id, r.issued_at,
		       COALESCE(SUM(l.net_usd_micros + l.tax_usd_micros), 0), COALESCE(SUM(l.tax_usd_micros), 0)
		FROM market_receipts r JOIN market_receipt_lines l ON l.receipt_id = r.id
		WHERE r.buyer_workspace_id = $1 GROUP BY r.id ORDER BY r.issued_at DESC, r.number DESC LIMIT 500`, buyerWorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("market: receipts: %w", err)
	}
	defer rows.Close()
	out := []ReceiptSummary{}
	for rows.Next() {
		var x ReceiptSummary
		var series string
		var year, n int
		if err := rows.Scan(&x.ID, &series, &year, &n, &x.InvoiceID, &x.IssuedAt, &x.GrossUSDMicros, &x.TaxUSDMicros); err != nil {
			return nil, err
		}
		x.Number = receiptNumber(series, year, n)
		out = append(out, x)
	}
	return out, rows.Err()
}
