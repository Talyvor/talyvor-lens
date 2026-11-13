package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// credit_line.go — B22.4: TALYVOR'S CREDIT LINE FOR COMPANIES: SPEND NOW, PAY MONTHLY.
//
// The operator marks a workspace a company and sets its credit line (`lens credit-lines`). When one of its
// agents spends on Talyvor services — a proxy debit or hold (agentMovement) — past what the agent holds, the
// line lends the shortfall: credits of funding FundingCreditLine land in the workspace (an lxc_ledger row of
// type LXCTypeCreditLineDraw), a fund entry moves them to the agent, credit_line_draws records the draw, and
// the spend goes through as usual, judged by the agent's rules. Marketplace listings need no draw: they are
// already on the company's monthly marketplace bill (B20.2), never taken from credits.
//
// A draw is refused past the limit, while the operator has paused the line, and while an invoice of it is
// unpaid past its due date: a missed payment pauses the line until that invoice is paid. Each month billing
// puts the draws of the months before on one Stripe invoice (billing.InvoiceCreditLines); once it is paid its
// draws stop counting against the limit.
//
// Companies only: credit involving a private user is class RED, and a private user's workspace is refused a
// line. For a company the line is GREEN (company_credit_line).

// LXCTypeCreditLineDraw marks credits the company's credit line lent to one of its agents.
const LXCTypeCreditLineDraw = "credit_line_draw"

// FundingCreditLine is the funding of credits lent by a credit line: Talyvor's, invoiced to the company.
const FundingCreditLine = "credit_line"

// ErrCreditLineCompaniesOnly: a private user's workspace cannot have a credit line.
var ErrCreditLineCompaniesOnly = errors.New("economy: Any loan or credit involving a private user is class RED. A credit line is for company workspaces only")

// ErrNoCreditLine: the workspace has no credit line.
var ErrNoCreditLine = errors.New("economy: the workspace has no credit line")

// CreditLineInvoice is one month's invoice of a credit line's draws.
type CreditLineInvoice struct {
	ID              string     `json:"id"`
	PeriodEnd       time.Time  `json:"period_end"`
	AmountULXC      int64      `json:"amount_ulxc"`
	AmountCents     int64      `json:"amount_cents"`
	StripeInvoiceID string     `json:"stripe_invoice_id"`
	DueAt           time.Time  `json:"due_at"`
	PaidAt          *time.Time `json:"paid_at,omitempty"`
	Late            bool       `json:"late"`
}

// CreditLine is a company's credit line as its owner sees it.
type CreditLine struct {
	WorkspaceID   string              `json:"workspace_id"`
	LimitULXC     int64               `json:"limit_ulxc"`
	UsedULXC      int64               `json:"used_ulxc"` // drawn and not yet paid for
	AvailableULXC int64               `json:"available_ulxc"`
	Paused        bool                `json:"paused"`
	PausedReason  string              `json:"paused_reason,omitempty"` // the operator's, or the late invoice
	Invoices      []CreditLineInvoice `json:"invoices"`
}

// creditLineUsedSQL is what a line has lent and not been paid for: its draws on no invoice or an unpaid one.
const creditLineUsedSQL = `SELECT COALESCE(sum(d.amount_ulxc), 0)::bigint FROM credit_line_draws d
  LEFT JOIN credit_line_invoices i ON i.id = d.invoice_id
  WHERE d.workspace_id = $1 AND i.paid_at IS NULL`

// creditLineLateSQL is whether an invoice of the line is unpaid past its due date.
const creditLineLateSQL = `SELECT EXISTS (SELECT 1 FROM credit_line_invoices
  WHERE workspace_id = $1 AND paid_at IS NULL AND due_at < now())`

// SetCompany records whether a workspace is a company.
func (s *DualTokenStore) SetCompany(ctx context.Context, workspaceID string, company bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE workspaces SET company = $2 WHERE id = $1`, workspaceID, company)
	if err != nil {
		return fmt.Errorf("economy: mark company: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("economy: no workspace %q", workspaceID)
	}
	return nil
}

// SetCreditLine gives a company workspace a credit line of limit µLXC, or changes its limit.
func (s *DualTokenStore) SetCreditLine(ctx context.Context, workspaceID string, limit int64, by string) (CreditLine, error) {
	if limit <= 0 {
		return CreditLine{}, errors.New("economy: a credit line's limit must be positive")
	}
	var company bool
	err := s.pool.QueryRow(ctx, `SELECT company FROM workspaces WHERE id = $1`, workspaceID).Scan(&company)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditLine{}, fmt.Errorf("economy: no workspace %q", workspaceID)
	}
	if err != nil {
		return CreditLine{}, err
	}
	if !company {
		return CreditLine{}, ErrCreditLineCompaniesOnly
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO company_credit_lines (workspace_id, limit_ulxc, set_by) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET limit_ulxc = $2, set_by = $3, updated_at = now()`, workspaceID, limit, by); err != nil {
		return CreditLine{}, fmt.Errorf("economy: set credit line: %w", err)
	}
	return s.CreditLine(ctx, workspaceID)
}

// PauseCreditLine stops the line lending until it is resumed.
func (s *DualTokenStore) PauseCreditLine(ctx context.Context, workspaceID, why string) error {
	return s.setLinePause(ctx, workspaceID, `paused_at = now(), paused_reason = $2`, why)
}

// ResumeCreditLine undoes the operator's pause. A late invoice still pauses the line until it is paid.
func (s *DualTokenStore) ResumeCreditLine(ctx context.Context, workspaceID string) error {
	return s.setLinePause(ctx, workspaceID, `paused_at = NULL, paused_reason = $2`, "")
}

func (s *DualTokenStore) setLinePause(ctx context.Context, workspaceID, set, why string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE company_credit_lines SET `+set+`, updated_at = now() WHERE workspace_id = $1`, workspaceID, why)
	if err != nil {
		return fmt.Errorf("economy: pause credit line: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoCreditLine
	}
	return nil
}

// CreditLine reads a workspace's credit line, what it has lent, and its invoices, newest first.
func (s *DualTokenStore) CreditLine(ctx context.Context, workspaceID string) (CreditLine, error) {
	l := CreditLine{WorkspaceID: workspaceID, Invoices: []CreditLineInvoice{}}
	var pausedAt *time.Time
	var company bool
	err := s.pool.QueryRow(ctx, `SELECT l.limit_ulxc, l.paused_at, l.paused_reason, COALESCE(w.company, false)
		FROM company_credit_lines l LEFT JOIN workspaces w ON w.id = l.workspace_id WHERE l.workspace_id = $1`,
		workspaceID).Scan(&l.LimitULXC, &pausedAt, &l.PausedReason, &company)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditLine{}, ErrNoCreditLine
	}
	if err != nil {
		return CreditLine{}, err
	}
	if err := s.pool.QueryRow(ctx, creditLineUsedSQL, workspaceID).Scan(&l.UsedULXC); err != nil {
		return CreditLine{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, period_end, amount_ulxc, amount_cents, stripe_invoice_id, due_at, paid_at,
		paid_at IS NULL AND due_at < now() FROM credit_line_invoices WHERE workspace_id = $1 ORDER BY period_end DESC`, workspaceID)
	if err != nil {
		return CreditLine{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var inv CreditLineInvoice
		if err := rows.Scan(&inv.ID, &inv.PeriodEnd, &inv.AmountULXC, &inv.AmountCents, &inv.StripeInvoiceID, &inv.DueAt, &inv.PaidAt, &inv.Late); err != nil {
			return CreditLine{}, err
		}
		if inv.Late && pausedAt == nil {
			l.PausedReason = fmt.Sprintf("the invoice due %s is unpaid", inv.DueAt.UTC().Format("2 January 2006"))
		}
		l.Paused = l.Paused || inv.Late
		l.Invoices = append(l.Invoices, inv)
	}
	if !company && pausedAt == nil {
		l.PausedReason = "the workspace is no longer recorded as a company"
	}
	l.Paused = l.Paused || pausedAt != nil || !company
	if !l.Paused {
		l.AvailableULXC = max(l.LimitULXC-l.UsedULXC, 0)
	}
	return l, rows.Err()
}

// CreditLineAvailableLXC is what the workspace's credit line can lend now: 0 with no line, or a paused one.
// The proxy's balance gate counts it for an agent's request (B22.4).
func (s *DualTokenStore) CreditLineAvailableLXC(ctx context.Context, workspaceID string) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	l, err := s.CreditLine(ctx, workspaceID)
	if errors.Is(err, ErrNoCreditLine) {
		return 0, nil
	}
	return l.AvailableULXC, err
}

// ListCreditLines reads every credit line.
func (s *DualTokenStore) ListCreditLines(ctx context.Context) ([]CreditLine, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id FROM company_credit_lines ORDER BY workspace_id`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]CreditLine, 0, len(ids))
	for _, id := range ids {
		l, err := s.CreditLine(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// drawCreditLine lends need µLXC from the workspace's credit line to agentID, inside the caller's spend
// transaction, and reports whether it did. It draws nothing — and the caller refuses the spend as before —
// when the workspace has no line or is no longer a company, and when the line is paused, late, or would pass
// its limit. The caller holds the agent's lock; the line's row is locked here, before the workspace balance.
func drawCreditLine(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, need int64, ref string) (bool, error) {
	var limit int64
	var paused, company bool
	err := tx.QueryRow(ctx, `SELECT l.limit_ulxc, l.paused_at IS NOT NULL, COALESCE(w.company, false)
		FROM company_credit_lines l LEFT JOIN workspaces w ON w.id = l.workspace_id
		WHERE l.workspace_id = $1 FOR UPDATE OF l`, workspaceID).Scan(&limit, &paused, &company)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("economy: credit line: %w", err)
	}
	if paused || !company {
		return false, nil
	}
	var late bool
	var used int64
	if err := tx.QueryRow(ctx, creditLineLateSQL, workspaceID).Scan(&late); err != nil {
		return false, fmt.Errorf("economy: credit line invoices: %w", err)
	}
	if err := tx.QueryRow(ctx, creditLineUsedSQL, workspaceID).Scan(&used); err != nil {
		return false, fmt.Errorf("economy: credit line used: %w", err)
	}
	if late || used+need > limit {
		return false, nil
	}
	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return false, err
	}
	meta := map[string]interface{}{"funding": FundingCreditLine, "agent_id": agentID, "ref": ref}
	if err := insertLXCLedger(ctx, tx, workspaceID, need, bal+need, LXCTypeCreditLineDraw, "credit line draw", meta); err != nil {
		return false, err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, bal+need, minted+need, spent); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO credit_line_draws (id, workspace_id, agent_id, amount_ulxc, ref) VALUES ($1, $2, $3, $4, $5)`,
		"cld_"+uuid.NewString(), workspaceID, agentID, need, ref); err != nil {
		return false, fmt.Errorf("economy: record credit line draw: %w", err)
	}
	return true, postEntry(ctx, tx, workspaceID, "credit_line", ref, leg{"workspace", -need}, leg{agentAccount(agentID), need})
}
