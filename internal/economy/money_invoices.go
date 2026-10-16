package economy

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// money_invoices.go — B30.20: INVOICES AND PAYMENT LINKS.
//
// An agent — or its owner for it — issues an invoice from one of the agent's accounts in GBP, EUR or USD: lines, a
// due date, optional VAT (a rate per line, the seller's and the customer's VAT numbers), and a pay link. The pay link
// takes three ways to pay, and each posts into the issuing account (migration 0248):
//
//   - a card, through Stripe test mode: PayInvoiceByCard, from the Checkout webhook, posts payments_in from the
//     workspace's card partner account;
//   - a transfer to the company's account details quoting the agent's payment reference and the invoice's: ReceivePayment
//     routes it to the agent (B30.14), and SweepInvoices matches it to the invoice by the reference it quoted;
//   - a Talyvor agent's payment: PayInvoiceByAgent moves currency_accounts from the payer agent's account in the same
//     currency straight into the issuer's, one entry on both statements.
//
// Each class is RED: test money only until a clearance, asked by PostMoney. An invoice is draft until sent, sent until
// its payments cover the total — then paid — and overdue while sent past its due date (UTC); a draft or sent one may be
// voided. SweepInvoices sends a reminder before the due date when the issuer asked for one, and one once overdue.

var (
	// ErrInvoice: an invoice asked for with lines, a customer or a due date it cannot have.
	ErrInvoice = errors.New("economy: invalid invoice")
	// ErrInvoiceNotFound: no such invoice, or not one this caller may see.
	ErrInvoiceNotFound = errors.New("economy: no such invoice")
	// ErrInvoiceState: the invoice is not in a state that allows it — sending a sent one, paying a paid or void one.
	ErrInvoiceState = errors.New("economy: the invoice's state does not allow this")
	// ErrInvoiceShort: the paying agent's account does not hold what the invoice asks.
	ErrInvoiceShort = errors.New("economy: the account does not hold enough to pay this invoice")
	// ErrInvoicePayerPaused: the paying agent is paused or archived, so it pays nothing.
	ErrInvoicePayerPaused = errors.New("economy: the agent is paused or archived, so it pays nothing")
)

// The states of an invoice. Overdue is read, never stored: a sent invoice past its due date.
const (
	InvoiceDraft   = "draft"
	InvoiceSent    = "sent"
	InvoiceOverdue = "overdue"
	InvoicePaid    = "paid"
	InvoiceVoid    = "void"
)

// cardPartnerRef is the partner account card payments come in through: Stripe, one per workspace and currency.
const cardPartnerRef = "card:stripe"

// InvoiceLine is one line: Quantity of UnitAmountMinor, and VAT at VATRateBPS (2000 is 20%) on its net.
type InvoiceLine struct {
	Description     string `json:"description"`
	Quantity        int64  `json:"quantity"`
	UnitAmountMinor int64  `json:"unit_amount_minor"`
	VATRateBPS      int64  `json:"vat_rate_bps"`
	NetMinor        int64  `json:"net_minor"`
	VATMinor        int64  `json:"vat_minor"`
}

// InvoiceRequest is an invoice asked for: paid into AccountID — or, for an agent, its open account in Currency.
type InvoiceRequest struct {
	AccountID         string        `json:"account_id"`
	Currency          string        `json:"currency"`
	CustomerName      string        `json:"customer_name"`
	CustomerEmail     string        `json:"customer_email"`
	CustomerAddress   string        `json:"customer_address"`
	CustomerVATNumber string        `json:"customer_vat_number"`
	SellerVATNumber   string        `json:"seller_vat_number"`
	Lines             []InvoiceLine `json:"lines"`
	DueDate           string        `json:"due_date"`           // YYYY-MM-DD
	RemindDaysBefore  int           `json:"remind_days_before"` // a reminder this many days before the due date; 0 sends none
	Memo              string        `json:"memo"`
	Send              bool          `json:"send"` // sent at once, rather than kept as a draft
}

// Invoice is an invoice, what has been paid on it and the reminders sent.
type Invoice struct {
	ID                string            `json:"id"`
	WorkspaceID       string            `json:"workspace_id,omitempty"`
	AgentID           string            `json:"agent_id,omitempty"`
	AccountID         string            `json:"account_id,omitempty"`
	Issuer            string            `json:"issuer"`
	Number            string            `json:"number"`    // INV-000001, the workspace's own
	Reference         string            `json:"reference"` // quoted with a transfer, it marks the invoice paid
	PayToken          string            `json:"pay_token,omitempty"`
	PayURL            string            `json:"pay_url,omitempty"`
	Currency          string            `json:"currency"`
	CustomerName      string            `json:"customer_name"`
	CustomerEmail     string            `json:"customer_email,omitempty"`
	CustomerAddress   string            `json:"customer_address,omitempty"`
	CustomerVATNumber string            `json:"customer_vat_number,omitempty"`
	SellerVATNumber   string            `json:"seller_vat_number,omitempty"`
	Lines             []InvoiceLine     `json:"lines"`
	SubtotalMinor     int64             `json:"subtotal_minor"`
	VATMinor          int64             `json:"vat_minor"`
	TotalMinor        int64             `json:"total_minor"`
	PaidMinor         int64             `json:"paid_minor"`
	DueMinor          int64             `json:"due_minor"`
	DueDate           string            `json:"due_date"`
	RemindDaysBefore  int               `json:"remind_days_before"`
	Memo              string            `json:"memo,omitempty"`
	Status            string            `json:"status"`
	SentAt            *time.Time        `json:"sent_at,omitempty"`
	PaidAt            *time.Time        `json:"paid_at,omitempty"`
	VoidedAt          *time.Time        `json:"voided_at,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	Payments          []InvoicePayment  `json:"payments,omitempty"`
	Reminders         []InvoiceReminder `json:"reminders,omitempty"`
}

// InvoicePayment is one payment on an invoice: the money entry that posted it.
type InvoicePayment struct {
	EntryID     string    `json:"entry_id"`
	Method      string    `json:"method"` // card, transfer or agent
	AmountMinor int64     `json:"amount_minor"`
	PaidAt      time.Time `json:"paid_at"`
}

// InvoiceReminder is a reminder sent: before the due date, or once overdue.
type InvoiceReminder struct {
	InvoiceID string    `json:"invoice_id,omitempty"`
	Kind      string    `json:"kind"` // before_due or overdue
	SentTo    string    `json:"sent_to,omitempty"`
	Message   string    `json:"message"`
	SentAt    time.Time `json:"sent_at"`
}

// PaidInvoice is an agent's payment of an invoice: the entry, and the invoice it paid.
type PaidInvoice struct {
	Invoice Invoice    `json:"invoice"`
	Entry   MoneyEntry `json:"entry"`
}

// InvoiceCardPayment is a card payment Stripe reports for an invoice.
type InvoiceCardPayment struct {
	InvoiceID   string
	SessionID   string // Stripe's Checkout Session: the payment posts once
	Payer       string // the cardholder, screened first
	AmountMinor int64
	Currency    string
	Funding     string // test ("" too) or live
}

// SetInvoicePayBase is where an invoice's pay link points: the web app's origin, to which /pay/<token> is added.
func (s *DualTokenStore) SetInvoicePayBase(base string) {
	s.invoicePayBase = strings.TrimRight(base, "/")
}

const invoiceColumns = `i.id, i.workspace_id, i.agent_id, i.account_id, COALESCE((SELECT w.name FROM workspaces w WHERE w.id = i.workspace_id), ''),
	i.number, i.reference, i.pay_token, i.currency, i.customer_name, i.customer_email, i.customer_address, i.customer_vat_number,
	i.seller_vat_number, i.lines, i.subtotal_minor, i.vat_minor, i.total_minor, i.due_date::text, i.remind_days_before, i.memo, i.status,
	i.sent_at, i.paid_at, i.voided_at, i.created_at,
	COALESCE((SELECT jsonb_agg(jsonb_build_object('entry_id', p.entry_id, 'method', p.method, 'amount_minor', p.amount_minor,
		'paid_at', p.paid_at) ORDER BY p.paid_at, p.entry_id) FROM money_invoice_payments p WHERE p.invoice_id = i.id), '[]'),
	COALESCE((SELECT jsonb_agg(jsonb_build_object('kind', r.kind, 'sent_to', r.sent_to, 'message', r.message, 'sent_at', r.sent_at)
		ORDER BY r.sent_at) FROM money_invoice_reminders r WHERE r.invoice_id = i.id), '[]')`

func (s *DualTokenStore) scanInvoice(row pgx.Row) (Invoice, error) {
	var inv Invoice
	var number int
	var lines, payments, reminders []byte
	if err := row.Scan(&inv.ID, &inv.WorkspaceID, &inv.AgentID, &inv.AccountID, &inv.Issuer, &number, &inv.Reference, &inv.PayToken,
		&inv.Currency, &inv.CustomerName, &inv.CustomerEmail, &inv.CustomerAddress, &inv.CustomerVATNumber, &inv.SellerVATNumber, &lines,
		&inv.SubtotalMinor, &inv.VATMinor, &inv.TotalMinor, &inv.DueDate, &inv.RemindDaysBefore, &inv.Memo, &inv.Status,
		&inv.SentAt, &inv.PaidAt, &inv.VoidedAt, &inv.CreatedAt, &payments, &reminders); err != nil {
		return Invoice{}, err
	}
	for _, j := range []struct {
		raw []byte
		v   any
	}{{lines, &inv.Lines}, {payments, &inv.Payments}, {reminders, &inv.Reminders}} {
		if err := json.Unmarshal(j.raw, j.v); err != nil {
			return Invoice{}, fmt.Errorf("economy: read invoice %s: %w", inv.ID, err)
		}
	}
	inv.Number = fmt.Sprintf("INV-%06d", number)
	inv.PayURL = s.invoicePayBase + "/pay/" + inv.PayToken
	for _, p := range inv.Payments {
		inv.PaidMinor += p.AmountMinor
	}
	inv.DueMinor = max(inv.TotalMinor-inv.PaidMinor, 0)
	if inv.Status == InvoiceSent && time.Now().UTC().Format(time.DateOnly) > inv.DueDate {
		inv.Status = InvoiceOverdue
	}
	return inv, nil
}

func (s *DualTokenStore) readInvoice(ctx context.Context, q pgxDB, where string, args ...any) (Invoice, error) {
	inv, err := s.scanInvoice(q.QueryRow(ctx, `SELECT `+invoiceColumns+` FROM money_invoices i WHERE `+where, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrInvoiceNotFound
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("economy: read invoice: %w", err)
	}
	return inv, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateInvoice issues r from one of workspaceID's agent accounts — with agentID, only that agent's — as a draft, or
// sent when r.Send.
func (s *DualTokenStore) CreateInvoice(ctx context.Context, workspaceID, agentID string, r InvoiceRequest) (Invoice, error) {
	r.CustomerName, r.CustomerEmail = strings.TrimSpace(r.CustomerName), strings.TrimSpace(r.CustomerEmail)
	r.CustomerAddress, r.Memo = strings.TrimSpace(r.CustomerAddress), strings.TrimSpace(r.Memo)
	r.CustomerVATNumber, r.SellerVATNumber = strings.TrimSpace(r.CustomerVATNumber), strings.TrimSpace(r.SellerVATNumber)
	due, derr := time.Parse(time.DateOnly, strings.TrimSpace(r.DueDate))
	switch {
	case r.CustomerName == "" || len(r.CustomerName) > 200 || len(r.CustomerEmail) > 320 || len(r.CustomerAddress) > 1000:
		return Invoice{}, fmt.Errorf("%w: customer_name is 1 to 200 characters, customer_email at most 320, customer_address at most 1000", ErrInvoice)
	case len(r.CustomerVATNumber) > 50 || len(r.SellerVATNumber) > 50 || len(r.Memo) > 2000:
		return Invoice{}, fmt.Errorf("%w: a VAT number is at most 50 characters, and memo at most 2000", ErrInvoice)
	case len(r.Lines) == 0 || len(r.Lines) > 100:
		return Invoice{}, fmt.Errorf("%w: an invoice has 1 to 100 lines", ErrInvoice)
	case derr != nil:
		return Invoice{}, fmt.Errorf("%w: due_date is a date, YYYY-MM-DD", ErrInvoice)
	case due.Format(time.DateOnly) < time.Now().UTC().Format(time.DateOnly):
		return Invoice{}, fmt.Errorf("%w: due_date is today or later", ErrInvoice)
	case r.RemindDaysBefore < 0 || r.RemindDaysBefore > 365:
		return Invoice{}, fmt.Errorf("%w: remind_days_before is 0 to 365", ErrInvoice)
	}
	var subtotal, vat int64
	for i := range r.Lines {
		l := &r.Lines[i]
		l.Description = strings.TrimSpace(l.Description)
		switch {
		case l.Description == "" || len(l.Description) > 500:
			return Invoice{}, fmt.Errorf("%w: line %d's description is 1 to 500 characters", ErrInvoice, i+1)
		case l.Quantity <= 0 || l.UnitAmountMinor <= 0 || l.UnitAmountMinor > math.MaxInt64/l.Quantity:
			return Invoice{}, fmt.Errorf("%w: line %d's quantity and unit_amount_minor are above zero", ErrInvoice, i+1)
		case l.VATRateBPS < 0 || l.VATRateBPS > 10000:
			return Invoice{}, fmt.Errorf("%w: line %d's vat_rate_bps is 0 to 10000", ErrInvoice, i+1)
		}
		l.NetMinor = l.Quantity * l.UnitAmountMinor
		if l.NetMinor > math.MaxInt64/10000 || subtotal > math.MaxInt64/2-l.NetMinor {
			return Invoice{}, fmt.Errorf("%w: line %d is too large", ErrInvoice, i+1)
		}
		l.VATMinor = (l.NetMinor*l.VATRateBPS + 5000) / 10000 // to the nearest minor unit, a half up
		subtotal, vat = subtotal+l.NetMinor, vat+l.VATMinor
	}

	var accountID, owner, currency, status string
	err := s.pool.QueryRow(ctx, `SELECT id, agent_id, currency, status FROM money_accounts
		WHERE workspace_id = $1 AND purpose = 'agent' AND ($2 = '' OR agent_id = $2)
		  AND (id = $3 OR ($3 = '' AND $2 <> '' AND currency = upper($4) AND status = 'open'))`,
		workspaceID, agentID, strings.TrimSpace(r.AccountID), strings.TrimSpace(r.Currency)).Scan(&accountID, &owner, &currency, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, fmt.Errorf("%w: no agent account %s to be paid into", ErrMoneyAccountNotFound, cmp.Or(r.AccountID, r.Currency))
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	switch {
	case status != MoneyOpen:
		return Invoice{}, fmt.Errorf("%w: %s is %s", ErrMoneyAccountNotOpen, accountID, status)
	case currency == CurrencyUSDC:
		return Invoice{}, fmt.Errorf("%w: an invoice is in GBP, EUR or USD", ErrInvoice)
	}
	lines, err := json.Marshal(r.Lines)
	if err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	ref, err := randomHex(6)
	if err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	token, err := randomHex(24)
	if err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	id := "inv_" + uuid.NewString()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Numbers run from 1 in each workspace with no gaps: two invoices issued at once take turns.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('money_invoices:' || $1::text))`, workspaceID); err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO money_invoices (id, workspace_id, agent_id, account_id, currency, number, reference, pay_token,
		customer_name, customer_email, customer_address, customer_vat_number, seller_vat_number, lines, subtotal_minor, vat_minor, total_minor,
		due_date, remind_days_before, memo, status, sent_at)
		VALUES ($1, $2, $3, $4, $5, (SELECT COALESCE(max(number), 0) + 1 FROM money_invoices WHERE workspace_id = $2), $6, $7, $8, $9, $10, $11, $12,
		$13, $14, $15, $16, $17, $18, $19, CASE WHEN $20 THEN 'sent' ELSE 'draft' END, CASE WHEN $20 THEN now() END)`,
		id, workspaceID, owner, accountID, currency, "INV"+strings.ToUpper(ref), "pay_"+token, r.CustomerName, r.CustomerEmail,
		r.CustomerAddress, r.CustomerVATNumber, r.SellerVATNumber, string(lines), subtotal, vat, subtotal+vat, due, r.RemindDaysBefore, r.Memo,
		r.Send); err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Invoice{}, fmt.Errorf("economy: create invoice: %w", err)
	}
	return s.readInvoice(ctx, s.pool, `i.id = $1`, id)
}

// Invoices is what workspaceID has issued — with agentID, only that agent's — the newest first.
func (s *DualTokenStore) Invoices(ctx context.Context, workspaceID, agentID string) ([]Invoice, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+invoiceColumns+` FROM money_invoices i
		WHERE i.workspace_id = $1 AND ($2 = '' OR i.agent_id = $2) ORDER BY i.created_at DESC, i.id`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: invoices: %w", err)
	}
	defer rows.Close()
	out := []Invoice{}
	for rows.Next() {
		inv, err := s.scanInvoice(rows)
		if err != nil {
			return nil, fmt.Errorf("economy: invoices: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// SendInvoice sends workspaceID's draft invoice id — with agentID, only that agent's: its pay link opens.
func (s *DualTokenStore) SendInvoice(ctx context.Context, workspaceID, agentID, id string) (Invoice, error) {
	return s.moveInvoice(ctx, workspaceID, agentID, id, `status = 'sent', sent_at = now()`, `status = 'draft'`)
}

// VoidInvoice voids workspaceID's draft or sent invoice id — with agentID, only that agent's: it can no longer be paid.
func (s *DualTokenStore) VoidInvoice(ctx context.Context, workspaceID, agentID, id string) (Invoice, error) {
	return s.moveInvoice(ctx, workspaceID, agentID, id, `status = 'void', voided_at = now()`, `status IN ('draft', 'sent')`)
}

func (s *DualTokenStore) moveInvoice(ctx context.Context, workspaceID, agentID, id, set, from string) (Invoice, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE money_invoices SET `+set+` WHERE id = $1 AND workspace_id = $2 AND ($3 = '' OR agent_id = $3) AND `+from,
		id, workspaceID, agentID)
	if err != nil {
		return Invoice{}, fmt.Errorf("economy: invoice: %w", err)
	}
	inv, err := s.readInvoice(ctx, s.pool, `i.id = $1 AND i.workspace_id = $2 AND ($3 = '' OR i.agent_id = $3)`, id, workspaceID, agentID)
	if err == nil && tag.RowsAffected() == 0 {
		return Invoice{}, fmt.Errorf("%w: it is %s", ErrInvoiceState, inv.Status)
	}
	return inv, err
}

// InvoiceByToken is the invoice behind a pay link: a draft's link opens nothing.
func (s *DualTokenStore) InvoiceByToken(ctx context.Context, token string) (Invoice, error) {
	return s.readInvoice(ctx, s.pool, `i.pay_token = $1 AND i.status <> 'draft'`, token)
}

// recordInvoicePayment keeps entryID as a payment of amountMinor on invoiceID, once, and marks the invoice paid when
// its payments cover the total.
func recordInvoicePayment(ctx context.Context, q pgxDB, invoiceID, entryID, method string, amountMinor int64) error {
	if _, err := q.Exec(ctx, `INSERT INTO money_invoice_payments (invoice_id, entry_id, method, amount_minor) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, invoiceID, entryID, method, amountMinor); err != nil {
		return fmt.Errorf("economy: record invoice payment: %w", err)
	}
	return markInvoicesPaid(ctx, q, `i.id = $1`, invoiceID)
}

func markInvoicesPaid(ctx context.Context, q pgxDB, where string, args ...any) error {
	if _, err := q.Exec(ctx, `UPDATE money_invoices i SET status = 'paid', paid_at = now() WHERE `+where+` AND i.status = 'sent'
		AND (SELECT COALESCE(sum(p.amount_minor), 0) FROM money_invoice_payments p WHERE p.invoice_id = i.id) >= i.total_minor`, args...); err != nil {
		return fmt.Errorf("economy: mark invoice paid: %w", err)
	}
	return nil
}

// PayInvoiceByAgent pays what is due on the invoice behind token from agentID's open account in its currency, in
// workspaceID, as funding (test, or "" for test; or live). It is once per paying account: a retry answers the payment
// made, and moves no money twice.
func (s *DualTokenStore) PayInvoiceByAgent(ctx context.Context, token, workspaceID, agentID, funding string) (PaidInvoice, error) {
	if funding == "" {
		funding = FundingTest
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaidInvoice{}, fmt.Errorf("economy: pay invoice: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The invoice's row is held until the payment commits, so two payers at once cannot both pay it.
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM money_invoices WHERE pay_token = $1 AND status <> 'draft' FOR UPDATE`, token).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaidInvoice{}, ErrInvoiceNotFound
	}
	if err != nil {
		return PaidInvoice{}, fmt.Errorf("economy: pay invoice: %w", err)
	}
	inv, err := s.readInvoice(ctx, tx, `i.id = $1`, id)
	if err != nil {
		return PaidInvoice{}, err
	}
	var from string
	var paused bool
	err = tx.QueryRow(ctx, `SELECT m.id, a.paused_at IS NOT NULL OR a.archived_at IS NOT NULL FROM money_accounts m JOIN agent_accounts a ON a.id = m.agent_id
		WHERE m.workspace_id = $1 AND m.agent_id = $2 AND m.purpose = 'agent' AND m.currency = $3 AND m.status = 'open'`,
		workspaceID, agentID, inv.Currency).Scan(&from, &paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaidInvoice{}, fmt.Errorf("%w: the agent has no open %s account to pay from", ErrMoneyAccountNotFound, inv.Currency)
	}
	if err != nil {
		return PaidInvoice{}, fmt.Errorf("economy: pay invoice: %w", err)
	}
	key := "invoice_agent:" + inv.ID + ":" + from
	if paid, err := readMoneyEntry(ctx, tx, `e.workspace_id = $1 AND e.idempotency_key = $2`, workspaceID, key); err == nil {
		return PaidInvoice{Invoice: inv, Entry: paid}, nil
	}
	switch {
	case inv.Status != InvoiceSent && inv.Status != InvoiceOverdue:
		return PaidInvoice{}, fmt.Errorf("%w: it is %s", ErrInvoiceState, inv.Status)
	case from == inv.AccountID:
		return PaidInvoice{}, fmt.Errorf("%w: an agent does not pay its own invoice", ErrInvoice)
	case paused:
		return PaidInvoice{}, ErrInvoicePayerPaused
	}
	// B30.131: the payer's rules judge it as a payment to the issuer, at what it is worth in LXC, exactly as a payment out
	// (B30.17): its limits, its payee lists and its approval amount. The agent is locked first, then the account, so no
	// other movement spends the same money or the same limit while this one is judged.
	if s.accountPartners == nil {
		return PaidInvoice{}, fmt.Errorf("%w: no account partner is set", ErrAccountNotOpened)
	}
	value, err := s.paymentValue(ctx, OutsidePayment{WorkspaceID: workspaceID, IdempotencyKey: key, AmountMinor: inv.DueMinor, Currency: inv.Currency})
	if err != nil {
		return PaidInvoice{}, err
	}
	payee := Payee{Kind: "company", ID: inv.WorkspaceID, Name: inv.Issuer}
	if inv.AgentID != "" {
		payee = Payee{Kind: "agent", ID: inv.AgentID, Name: inv.Issuer}
	}
	fp := sha256.Sum256([]byte(key + "\x00" + strconv.FormatInt(inv.DueMinor, 10) + "\x00" + inv.Currency + "\x00" + funding))
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]), Payee: payee,
		Memo: minorString(inv.DueMinor, inv.Currency) + " for invoice " + inv.Number + " from " + cmp.Or(inv.Issuer, inv.WorkspaceID) + " (" + funding + " money)"})
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return PaidInvoice{}, err
	}
	var held int64
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, from); err != nil {
		return PaidInvoice{}, fmt.Errorf("economy: pay invoice: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance_minor FROM money_account_balances WHERE account_id = $1 AND funding = $2), 0)`,
		from, funding).Scan(&held); err != nil {
		return PaidInvoice{}, fmt.Errorf("economy: pay invoice: %w", err)
	}
	if held < inv.DueMinor {
		return PaidInvoice{}, fmt.Errorf("%w: it holds %s of %s money, and %s is due", ErrInvoiceShort, FormatMinor(held, inv.Currency), funding,
			FormatMinor(inv.DueMinor, inv.Currency))
	}
	if err := enforceAgentRules(ctx, tx, workspaceID, agentID, value, key); err != nil {
		return PaidInvoice{}, s.refusedMovement(ctx, tx, err)
	}
	e := MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityCurrencyAccounts, Kind: "invoice_payment", IdempotencyKey: key,
		Funding: funding, Memo: "Invoice " + inv.Number + " from " + cmp.Or(inv.Issuer, inv.WorkspaceID),
		Postings: []MoneyPosting{{AccountID: from, AmountMinor: -inv.DueMinor}, {AccountID: inv.AccountID, AmountMinor: inv.DueMinor}}}
	if inv.WorkspaceID != workspaceID {
		e.payeeWorkspaceID = inv.WorkspaceID
	}
	out := PaidInvoice{}
	if out.Entry, err = postMoneyTx(ctx, tx, e, s.screener); err != nil {
		return PaidInvoice{}, err
	}
	if err := recordInvoicePayment(ctx, tx, inv.ID, out.Entry.ID, "agent", inv.DueMinor); err != nil {
		return PaidInvoice{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return PaidInvoice{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return PaidInvoice{}, fmt.Errorf("economy: pay invoice: %w", err)
	}
	out.Invoice, err = s.readInvoice(ctx, s.pool, `i.id = $1`, inv.ID)
	return out, err
}

// PayInvoiceByCard posts a card payment Stripe took for an invoice into the issuing account: payments_in from the
// workspace's card partner account, screened against the cardholder first, once per Checkout Session. Money taken is
// always posted — onto a paid or void invoice too — and marks a sent one paid once its payments cover the total.
func (s *DualTokenStore) PayInvoiceByCard(ctx context.Context, c InvoiceCardPayment) (Invoice, error) {
	if c.Funding == "" {
		c.Funding = FundingTest
	}
	if c.SessionID == "" || c.AmountMinor <= 0 {
		return Invoice{}, fmt.Errorf("%w: a card payment names its session and brings money", ErrInvoice)
	}
	inv, err := s.readInvoice(ctx, s.pool, `i.id = $1`, c.InvoiceID)
	if err != nil {
		return Invoice{}, err
	}
	if !strings.EqualFold(c.Currency, inv.Currency) {
		return Invoice{}, fmt.Errorf("%w: the invoice is in %s, not %s", ErrInvoice, inv.Currency, c.Currency)
	}
	var partner string
	if err := s.pool.QueryRow(ctx, `WITH made AS (INSERT INTO money_accounts (id, workspace_id, currency, purpose, name, partner_account_ref)
		VALUES ($1, $2, $3, 'partner', 'Stripe (card payments)', $4)
		ON CONFLICT (workspace_id, currency) WHERE purpose = 'partner' AND partner_account_ref = 'card:stripe' DO NOTHING RETURNING id)
		SELECT id FROM made UNION ALL SELECT id FROM money_accounts WHERE workspace_id = $2 AND currency = $3 AND purpose = 'partner'
		AND partner_account_ref = $4`, "macc_"+uuid.NewString(), inv.WorkspaceID, inv.Currency, cardPartnerRef).Scan(&partner); err != nil {
		return Invoice{}, fmt.Errorf("economy: card partner account: %w", err)
	}
	e, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: inv.WorkspaceID, Capability: CapabilityPaymentsIn, Kind: "invoice_card",
		IdempotencyKey: "invoice_card:" + c.SessionID, Funding: c.Funding, Memo: "Card payment of invoice " + inv.Number,
		Counterparty: c.Payer, PartnerRef: c.SessionID,
		Postings: []MoneyPosting{{AccountID: partner, AmountMinor: -c.AmountMinor}, {AccountID: inv.AccountID, AmountMinor: c.AmountMinor}}})
	if err != nil {
		return Invoice{}, err
	}
	if err := recordInvoicePayment(ctx, s.pool, inv.ID, e.ID, "card", c.AmountMinor); err != nil {
		return Invoice{}, err
	}
	return s.readInvoice(ctx, s.pool, `i.id = $1`, inv.ID)
}

// SweepInvoices matches transfers to the invoices they quote, then sends the reminders due at now: one before the due
// date when the issuer asked for one, and one once overdue. It answers the reminders it sent.
//
// shortcut: Lens has no mailer, so a reminder is kept on the invoice — its list, and its pay page — addressed to the
// customer's email; send it by email too once Lens has a mailer.
func (s *DualTokenStore) SweepInvoices(ctx context.Context, now time.Time) ([]InvoiceReminder, error) {
	// A payment in to the invoice's account whose reference, as typed, holds the invoice's.
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO money_invoice_payments (invoice_id, entry_id, method, amount_minor)
			SELECT i.id, e.id, 'transfer', p.amount_minor FROM money_invoices i
			JOIN money_postings p ON p.account_id = i.account_id AND p.amount_minor > 0
			JOIN money_entries e ON e.id = p.entry_id AND e.kind = 'payment_in' AND e.created_at >= i.created_at
			WHERE i.status = 'sent' AND strpos(regexp_replace(upper(e.memo), '[^A-Z0-9]', '', 'g'), i.reference) > 0
			ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		return markInvoicesPaid(ctx, tx, `EXISTS (SELECT 1 FROM money_invoice_payments p WHERE p.invoice_id = i.id)`)
	}); err != nil {
		return nil, fmt.Errorf("economy: match invoice transfers: %w", err)
	}

	today := now.UTC().Format(time.DateOnly)
	rows, err := s.pool.Query(ctx, `SELECT `+invoiceColumns+` FROM money_invoices i WHERE i.status = 'sent' AND (
		($1::date > i.due_date AND NOT EXISTS (SELECT 1 FROM money_invoice_reminders r WHERE r.invoice_id = i.id AND r.kind = 'overdue'))
		OR (i.remind_days_before > 0 AND $1::date BETWEEN i.due_date - i.remind_days_before AND i.due_date
			AND NOT EXISTS (SELECT 1 FROM money_invoice_reminders r WHERE r.invoice_id = i.id AND r.kind = 'before_due')))`, today)
	if err != nil {
		return nil, fmt.Errorf("economy: invoice reminders: %w", err)
	}
	var due []Invoice
	for rows.Next() {
		inv, err := s.scanInvoice(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("economy: invoice reminders: %w", err)
		}
		due = append(due, inv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: invoice reminders: %w", err)
	}
	sent := []InvoiceReminder{}
	for _, inv := range due {
		r := InvoiceReminder{InvoiceID: inv.ID, Kind: "before_due", SentTo: inv.CustomerEmail,
			Message: fmt.Sprintf("Invoice %s from %s for %s is due on %s. Pay it at %s", inv.Number, cmp.Or(inv.Issuer, inv.WorkspaceID),
				FormatMinor(inv.DueMinor, inv.Currency), inv.DueDate, inv.PayURL)}
		if today > inv.DueDate {
			r.Kind = "overdue"
			r.Message = fmt.Sprintf("Invoice %s from %s for %s was due on %s and is overdue. Pay it at %s", inv.Number,
				cmp.Or(inv.Issuer, inv.WorkspaceID), FormatMinor(inv.DueMinor, inv.Currency), inv.DueDate, inv.PayURL)
		}
		err := s.pool.QueryRow(ctx, `INSERT INTO money_invoice_reminders (invoice_id, kind, sent_to, message, sent_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT DO NOTHING RETURNING sent_at`, r.InvoiceID, r.Kind, r.SentTo, r.Message, now).Scan(&r.SentAt)
		switch {
		case err == nil:
			sent = append(sent, r)
		case !errors.Is(err, pgx.ErrNoRows):
			return sent, fmt.Errorf("economy: invoice reminders: %w", err)
		}
	}
	return sent, nil
}
