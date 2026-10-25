package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// money_bills.go — B41.101: BILLS A WORKSPACE RECEIVES.
//
// An invoice the workspace uploads or photographs is read into a bill: ReadBill finds the supplier, amount, currency,
// due date, invoice number and the bank details printed on it, in the text the document converter (distill) recovered.
// The bill is matched to a saved payee (B30.16) — by its bank details, else by the supplier's name — or marked a new
// payee. The invoice's bank details are kept to show and to propose a payee from, never paid on their own: a payment
// pays a saved payee, whose details passed the payee check. One invoice is one bill: the same document, or the same
// supplier's same invoice number, finds the bill already there.

// Bill sources: an uploaded file, a photographed invoice (its transcription), or mail to the bills address.
const (
	BillSourceUpload = "upload"
	BillSourcePhoto  = "photo"
	BillSourceEmail  = "email"
)

// How a bill's supplier relates to the workspace's saved payees.
const (
	BillPayeeMatched     = "matched"             // a saved payee with these bank details, or this name when none are printed
	BillPayeeDetailsDiff = "bank_details_differ" // a saved payee by this name whose saved details differ from the invoice's
	BillPayeeNew         = "new_payee"           // no saved payee: propose one from the invoice, through the payee check
)

var (
	// ErrBill: a bill with no document or an unknown source.
	ErrBill = errors.New("economy: invalid bill")
	// ErrBillNotFound: no such bill in this workspace.
	ErrBillNotFound = errors.New("economy: no such bill")
)

// BillFields is what reading an invoice found; a field it could not read is empty (AmountMinor 0).
type BillFields struct {
	Supplier      string `json:"supplier"`
	InvoiceNumber string `json:"invoice_number"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	DueDate       string `json:"due_date,omitempty"` // YYYY-MM-DD
	SortCode      string `json:"sort_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	IBAN          string `json:"iban,omitempty"`
	BIC           string `json:"bic,omitempty"`
	RoutingNumber string `json:"routing_number,omitempty"`
}

// Bill is a bill the workspace received.
type Bill struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Source      string `json:"source"`
	Filename    string `json:"filename,omitempty"`
	BillFields
	PayeeID    string `json:"payee_id,omitempty"`
	PayeeMatch string `json:"payee_match"`
	Status     string `json:"status"`
	// Missing names the fields reading the invoice could not find, for the owner to fill in.
	Missing   []string  `json:"missing"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	billSupplierLabel = regexp.MustCompile(`(?i)^(?:from|supplier|vendor|seller|bill(?:ed)?\s+from|invoice\s+from)\s*[:\-]\s*(.+)$`)
	billHeading       = regexp.MustCompile(`(?i)^(?:tax\s+|vat\s+)?(?:invoice|bill|receipt|statement)\b`)
	billInvoiceNumber = regexp.MustCompile(`(?i)\binvoice\s*(?:no\.?|number|num|#|ref(?:erence)?)\s*[:#.]?\s*([A-Z0-9][A-Z0-9\-/._]*)`)
	billAmountDue     = regexp.MustCompile(`(?i)\b(?:amount|total|balance)\s+(?:due|payable|to\s+pay)\b`)
	billTotal         = regexp.MustCompile(`(?i)^(?:grand\s+)?total\b`)
	billCurrencyMoney = regexp.MustCompile(`(?i)(GBP|EUR|USD|£|€|\$)\s*(\d[\d,.]*)|(\d[\d,.]*)\s*(GBP|EUR|USD|£|€)`)
	billNumber        = regexp.MustCompile(`\d[\d,.]*`)
	billCurrencyAny   = regexp.MustCompile(`(?i)\b(GBP|EUR|USD)\b|[£€$]`)
	billDueLabel      = regexp.MustCompile(`(?i)\b(?:due\s+date|payment\s+due|due\s+by|due\s+on|pay\s+by|due)\b`)
	billDateISO       = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	billDateNumeric   = regexp.MustCompile(`\b(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4})\b`)
	billDateDayMonth  = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+([a-z]{3,9})\.?,?\s+(\d{4})\b`)
	billDateMonthDay  = regexp.MustCompile(`(?i)\b([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	billIBAN          = regexp.MustCompile(`(?i)\bIBAN\b\s*[:.]?\s*([A-Z]{2}\d{2}(?:\s?[A-Z0-9]){11,30})`)
	billBIC           = regexp.MustCompile(`(?i)\b(?:BIC|SWIFT)(?:\s*code)?\b\s*[:.]?\s*([A-Z]{6}[A-Z0-9]{2}(?:[A-Z0-9]{3})?)\b`)
	billSortCode      = regexp.MustCompile(`(?i)\bsort\s*code\b\s*[:.]?\s*(\d{2})[\s\-]?(\d{2})[\s\-]?(\d{2})\b`)
	billAccountNumber = regexp.MustCompile(`(?i)\b(?:account|acct|a/c)\s*(?:no\.?|number|num|#)\s*[:.]?\s*(\d{4,17})\b`)
	billRouting       = regexp.MustCompile(`(?i)\b(?:routing|ABA)\s*(?:no\.?|number|#)?\s*[:.]?\s*(\d{9})\b`)
	billCurrencyCode  = map[string]string{"£": CurrencyGBP, "€": CurrencyEUR, "$": CurrencyUSD, "GBP": CurrencyGBP, "EUR": CurrencyEUR, "USD": CurrencyUSD}
)

// ReadBill reads an invoice's text (the Markdown distill recovered) into its fields. It looks for labelled lines —
// "Invoice number", "Amount due" or "Total", "Due date", "Sort code", "IBAN" — as invoices print them; what it cannot
// find it leaves empty.
func ReadBill(text string) BillFields {
	var f BillFields
	clean := strings.NewReplacer("|", " ", "*", "", "#", " ", "_", " ").Replace
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.Join(strings.Fields(clean(l)), " "); l != "" {
			lines = append(lines, l)
		}
	}
	amountRank := 0 // 2: an "amount due" line, 1: a "total" line
	for _, l := range lines {
		if m := billSupplierLabel.FindStringSubmatch(l); m != nil && f.Supplier == "" {
			f.Supplier = m[1]
		}
		if m := billInvoiceNumber.FindStringSubmatch(l); m != nil && f.InvoiceNumber == "" && strings.ContainsAny(m[1], "0123456789") {
			f.InvoiceNumber = strings.ToUpper(strings.TrimRight(m[1], ".-/"))
		}
		rank := 0
		switch {
		case billAmountDue.MatchString(l):
			rank = 2
		case billTotal.MatchString(l):
			rank = 1
		}
		// The last line of the best rank wins: a grand total follows the lines it adds up.
		if rank > 0 && rank >= amountRank {
			if amount, currency, ok := billMoney(l); ok {
				f.AmountMinor, f.Currency, amountRank = amount, currency, rank
			}
		}
		if f.DueDate == "" && billDueLabel.MatchString(l) {
			f.DueDate = billDate(l)
		}
		if m := billIBAN.FindStringSubmatch(l); m != nil && f.IBAN == "" {
			if iban := strings.ToUpper(strings.ReplaceAll(m[1], " ", "")); ibanPattern.MatchString(iban) && ibanChecks(iban) {
				f.IBAN = iban
			}
		}
		if m := billBIC.FindStringSubmatch(l); m != nil && f.BIC == "" {
			f.BIC = strings.ToUpper(m[1])
		}
		if m := billSortCode.FindStringSubmatch(l); m != nil && f.SortCode == "" {
			f.SortCode = m[1] + "-" + m[2] + "-" + m[3]
		}
		if m := billAccountNumber.FindStringSubmatch(l); m != nil && f.AccountNumber == "" {
			f.AccountNumber = m[1]
		}
		if m := billRouting.FindStringSubmatch(l); m != nil && f.RoutingNumber == "" {
			f.RoutingNumber = m[1]
		}
	}
	if f.Supplier == "" {
		// An invoice with no "From:" line names its supplier first, under or above the word "Invoice".
		for _, l := range lines {
			if !billHeading.MatchString(l) && !strings.Contains(l, ":") && strings.IndexFunc(l, unicode.IsLetter) >= 0 {
				f.Supplier = l
				break
			}
		}
	}
	if len(f.Supplier) > 140 {
		f.Supplier = f.Supplier[:140]
	}
	if f.AmountMinor > 0 && f.Currency == "" {
		if m := billCurrencyAny.FindString(text); m != "" {
			f.Currency = billCurrencyCode[strings.ToUpper(m)]
		}
	}
	return f
}

// billMoney is the amount on a line, in minor units, and its currency when the line prints one: the figure next to a
// currency sign or code, else the line's last figure.
func billMoney(line string) (int64, string, bool) {
	figure, currency := "", ""
	if m := billCurrencyMoney.FindStringSubmatch(line); m != nil {
		if m[1] != "" {
			figure, currency = m[2], billCurrencyCode[strings.ToUpper(m[1])]
		} else {
			figure, currency = m[3], billCurrencyCode[strings.ToUpper(m[4])]
		}
	} else if all := billNumber.FindAllString(line, -1); len(all) > 0 {
		figure = all[len(all)-1]
	}
	amount, ok := billMinor(figure)
	return amount, currency, ok
}

var commaDecimal = regexp.MustCompile(`^\d{1,3}(\.\d{3})*,\d{2}$`)

// billMinor parses a printed amount — 1,234.56, 1.234,56 or 1234 — into minor units of a two-decimal currency.
func billMinor(s string) (int64, bool) {
	s = strings.TrimRight(s, ".,")
	if commaDecimal.MatchString(s) {
		s = strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), ",", ".")
	} else {
		s = strings.ReplaceAll(s, ",", "")
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(whole) > 13 || len(frac) > 2 || strings.Contains(frac, ".") {
		return 0, false
	}
	frac += strings.Repeat("0", 2-len(frac))
	w, err1 := strconv.ParseInt(whole, 10, 64)
	c, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w*100+c == 0 {
		return 0, false
	}
	return w*100 + c, true
}

// billDate is the first date on a line, as YYYY-MM-DD. shortcut: a numeric date is read day first (01/11/2026 is
// 1 November) unless that is impossible; read the supplier's country once bills carry it.
func billDate(line string) string {
	try := func(layout, value string) string {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("2006-01-02")
		}
		return ""
	}
	if m := billDateISO.FindStringSubmatch(line); m != nil {
		return try("2006-01-02", m[0])
	}
	if m := billDateNumeric.FindStringSubmatch(line); m != nil {
		if d := try("2/1/2006", m[1]+"/"+m[2]+"/"+m[3]); d != "" {
			return d
		}
		return try("1/2/2006", m[1]+"/"+m[2]+"/"+m[3])
	}
	month := func(name string) string {
		if len(name) < 3 {
			return ""
		}
		return strings.ToUpper(name[:1]) + strings.ToLower(name[1:3])
	}
	if m := billDateDayMonth.FindStringSubmatch(line); m != nil {
		if d := try("2 Jan 2006", m[1]+" "+month(m[2])+" "+m[3]); d != "" {
			return d
		}
	}
	if m := billDateMonthDay.FindStringSubmatch(line); m != nil {
		return try("2 Jan 2006", m[2]+" "+month(m[1])+" "+m[3])
	}
	return ""
}

// billPayee matches a bill to the workspace's saved payees: by bank details first, then by the supplier's name.
func billPayee(f BillFields, payees []OutsidePayee) (payeeID, match string) {
	hasDetails := f.IBAN != "" || f.AccountNumber != ""
	sameDetails := func(p OutsidePayee) bool {
		switch {
		case f.IBAN != "":
			return p.IBAN == f.IBAN
		case f.SortCode != "":
			return p.SortCode == f.SortCode && p.AccountNumber == f.AccountNumber
		case f.RoutingNumber != "":
			return p.RoutingNumber == f.RoutingNumber && p.AccountNumber == f.AccountNumber
		}
		return false
	}
	for _, p := range payees {
		if hasDetails && sameDetails(p) {
			return p.ID, BillPayeeMatched
		}
	}
	for _, p := range payees {
		if f.Supplier != "" && strings.EqualFold(p.Name, f.Supplier) {
			if hasDetails {
				return p.ID, BillPayeeDetailsDiff
			}
			return p.ID, BillPayeeMatched
		}
	}
	return "", BillPayeeNew
}

const billCols = `id, workspace_id, source, filename, supplier, invoice_number, amount_minor, currency,
	COALESCE(to_char(due_date, 'YYYY-MM-DD'), ''), sort_code, account_number, iban, bic, routing_number, payee_id, payee_match,
	status, created_at`

func scanBill(row pgx.Row) (Bill, error) {
	var b Bill
	err := row.Scan(&b.ID, &b.WorkspaceID, &b.Source, &b.Filename, &b.Supplier, &b.InvoiceNumber, &b.AmountMinor, &b.Currency,
		&b.DueDate, &b.SortCode, &b.AccountNumber, &b.IBAN, &b.BIC, &b.RoutingNumber, &b.PayeeID, &b.PayeeMatch, &b.Status, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrBillNotFound
	}
	b.Missing = []string{}
	for _, m := range []struct {
		name  string
		empty bool
	}{{"supplier", b.Supplier == ""}, {"amount", b.AmountMinor == 0}, {"currency", b.Currency == ""},
		{"due_date", b.DueDate == ""}, {"invoice_number", b.InvoiceNumber == ""}} {
		if m.empty {
			b.Missing = append(b.Missing, m.name)
		}
	}
	return b, err
}

// AddBill reads document's text into a bill for workspaceID and matches it to a saved payee. created is false when
// the invoice was already a bill — the same document, or the same supplier's same invoice number — and that bill is
// returned unchanged.
func (s *DualTokenStore) AddBill(ctx context.Context, workspaceID, source, filename string, document []byte, text string) (b Bill, created bool, err error) {
	switch {
	case len(document) == 0:
		return Bill{}, false, fmt.Errorf("%w: the bill has no document", ErrBill)
	case source != BillSourceUpload && source != BillSourcePhoto && source != BillSourceEmail:
		return Bill{}, false, fmt.Errorf("%w: source is upload, photo or email", ErrBill)
	}
	sum := sha256.Sum256(document)
	digest := hex.EncodeToString(sum[:])
	f := ReadBill(text)
	payees, err := s.OutsidePayees(ctx, workspaceID)
	if err != nil {
		return Bill{}, false, err
	}
	payeeID, match := billPayee(f, payees)
	b, err = scanBill(s.pool.QueryRow(ctx, `INSERT INTO money_bills (id, workspace_id, source, document_sha256, filename, supplier,
		invoice_number, amount_minor, currency, due_date, sort_code, account_number, iban, bic, routing_number, payee_id, payee_match)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, '')::date, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT DO NOTHING RETURNING `+billCols, "bill_"+uuid.NewString(), workspaceID, source, digest, filename, f.Supplier,
		f.InvoiceNumber, f.AmountMinor, f.Currency, f.DueDate, f.SortCode, f.AccountNumber, f.IBAN, f.BIC, f.RoutingNumber, payeeID, match))
	if !errors.Is(err, ErrBillNotFound) {
		return b, err == nil, err
	}
	b, err = scanBill(s.pool.QueryRow(ctx, `SELECT `+billCols+` FROM money_bills WHERE workspace_id = $1 AND (document_sha256 = $2
		OR (invoice_number <> '' AND invoice_number = $3 AND lower(supplier) = lower($4))) ORDER BY created_at LIMIT 1`,
		workspaceID, digest, f.InvoiceNumber, f.Supplier))
	return b, false, err
}

// Bills is workspaceID's bills: those due first, soonest due first, then those paid.
func (s *DualTokenStore) Bills(ctx context.Context, workspaceID string) ([]Bill, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+billCols+` FROM money_bills WHERE workspace_id = $1
		ORDER BY status = 'paid', due_date NULLS LAST, created_at, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: bills: %w", err)
	}
	defer rows.Close()
	out := []Bill{}
	for rows.Next() {
		b, err := scanBill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BillByID is one of workspaceID's bills.
func (s *DualTokenStore) BillByID(ctx context.Context, workspaceID, billID string) (Bill, error) {
	return scanBill(s.pool.QueryRow(ctx, `SELECT `+billCols+` FROM money_bills WHERE id = $1 AND workspace_id = $2`, billID, workspaceID))
}
