package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/workspace"
)

// company_loans.go — B22.5: LOANS BETWEEN COMPANIES: OFFER, BORROW, REPAY.
//
// An agent of one company offers an agent of another a loan in credits (OfferLoan): the principal, the
// interest on it over the whole term, how many equal instalments repay it and how often, and the fee a late
// instalment adds. The borrower's side accepts or declines (AnswerLoan); accepting pays the principal out as
// a transfer (B22.3) judged by the lender's rules. RunLoanRepayments, on the schedules' tick, then takes each
// instalment from the borrower's agent when it is due and pays it to the lender's. One that cannot be taken is
// missed: the loan is late and the instalment is tried again one period later with the late fee added; missed
// again, the loan is in default and nothing more is taken.
//
// Both sides must be companies (workspaces.company, B22.4) whose owners are verified (B19.11): credit
// involving a private user is class RED. A loan is class AMBER — test-funded credits only until the operator
// clears loans_between_companies; with live money and no clearance the offer itself is refused naming the
// class. Every movement is an agent_transfers row naming the loan, so both statements carry it; the loan and
// its events (agent_loan_events) are on both statements too.
//
// B56.22: Talyvor keeps the lending fee (fees.LendingFeeBPS, wired by SetLendingFee) of the principal. The offer
// records the rate and the fee, so the borrower sees it before answering; accepting takes that fee from what
// the borrower received, as its own lxc_ledger row and agent posting (kind lending_fee), and the payout event
// names it. Unwired, the fee is 0.

// CapabilityCompanyLoans is the wallet capability a loan moves money as.
const CapabilityCompanyLoans = "loans_between_companies"

// LXCTypeLendingFee marks the lending fee's lxc_ledger row and agent postings — Talyvor's fee on a loan arranged.
const LXCTypeLendingFee = "lending_fee"

// SetLendingFee wires the lending fee's rate, in basis points of the principal. nil, the fee is 0.
func (s *DualTokenStore) SetLendingFee(bps func() int64) { s.lendingFee = bps }

// LendingFeeLabel is how a statement names the fee: "Lending fee 1%".
func LendingFeeLabel(bps int64) string { return "Lending fee " + percent(bps) }

// ErrLoanCompaniesOnly: a private user's workspace cannot lend or borrow.
var ErrLoanCompaniesOnly = errors.New("economy: Any loan or credit involving a private user is class RED — both sides of a loan must be companies")

// ErrLoanNotFound: no such loan this workspace can act on in its state.
var ErrLoanNotFound = errors.New("economy: no such loan")

// ErrLoanTerms: terms that are not a loan.
var ErrLoanTerms = errors.New("economy: loan terms")

// LoanTerms are what a lender offers.
type LoanTerms struct {
	PrincipalULXC int64  `json:"principal_ulxc"`
	InterestBPS   int    `json:"interest_bps"` // on the principal, over the whole term: 1000 is 10%
	Instalments   int    `json:"instalments"`
	Every         string `json:"every"` // day | week | month
	LateFeeULXC   int64  `json:"late_fee_ulxc"`
	Memo          string `json:"memo,omitempty"`
}

// LoanEvent is one movement of a loan: its payout, an instalment taken, one missed, late, defaulted.
type LoanEvent struct {
	Kind          string    `json:"kind"`
	Instalment    int       `json:"instalment,omitempty"`
	PrincipalULXC int64     `json:"principal_ulxc,omitempty"`
	InterestULXC  int64     `json:"interest_ulxc,omitempty"`
	LateFeeULXC   int64     `json:"late_fee_ulxc,omitempty"`
	FeeULXC       int64     `json:"fee_ulxc,omitempty"` // on the payout: the lending fee taken from what was paid out
	TransferID    string    `json:"transfer_id,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	At            time.Time `json:"at"`
}

// Loan is a loan between two companies' agents, as both sides see it.
type Loan struct {
	ID                  string `json:"id"`
	LenderWorkspaceID   string `json:"lender_workspace_id"`
	LenderAgentID       string `json:"lender_agent_id"`
	BorrowerWorkspaceID string `json:"borrower_workspace_id"`
	BorrowerAgentID     string `json:"borrower_agent_id"`
	LoanTerms
	// FeeBPS and FeeULXC are the lending fee, set when it is offered: accepting takes FeeULXC from the payout.
	FeeBPS          int64       `json:"fee_bps"`
	FeeULXC         int64       `json:"fee_ulxc"`
	Status          string      `json:"status"` // offered | declined | withdrawn | active | late | defaulted | repaid
	PaidInstalments int         `json:"paid_instalments"`
	NextDueAt       *time.Time  `json:"next_due_at,omitempty"`
	OfferedAt       time.Time   `json:"offered_at"`
	DecidedAt       *time.Time  `json:"decided_at,omitempty"`
	Events          []LoanEvent `json:"events"`
}

// instalment is the principal and interest of instalment k (from 1): equal parts, the last one taking what
// the division leaves.
func (t LoanTerms) instalment(k int) (principal, interest int64) {
	n := int64(t.Instalments)
	total := t.PrincipalULXC * int64(t.InterestBPS) / 10_000
	principal, interest = t.PrincipalULXC/n, total/n
	if int64(k) == n {
		principal, interest = t.PrincipalULXC-principal*(n-1), total-interest*(n-1)
	}
	return principal, interest
}

// dueOf is when instalment k of a loan decided at decided falls due.
func dueOf(decided time.Time, every string, k int) time.Time {
	switch every {
	case "day":
		return decided.AddDate(0, 0, k)
	case "week":
		return decided.AddDate(0, 0, 7*k)
	default: // month
		return decided.AddDate(0, k, 0)
	}
}

func (t LoanTerms) validate() error {
	switch {
	case t.PrincipalULXC <= 0:
		return fmt.Errorf("%w: the principal must be positive", ErrLoanTerms)
	case t.InterestBPS < 0 || t.InterestBPS > 100_000:
		return fmt.Errorf("%w: interest_bps must be 0 to 100000", ErrLoanTerms)
	case t.Instalments < 1 || t.Instalments > 120:
		return fmt.Errorf("%w: 1 to 120 instalments", ErrLoanTerms)
	case t.Every != "day" && t.Every != "week" && t.Every != "month":
		return fmt.Errorf("%w: every must be day, week or month", ErrLoanTerms)
	case t.LateFeeULXC < 0:
		return fmt.Errorf("%w: the late fee cannot be negative", ErrLoanTerms)
	}
	return nil
}

// requireCompanies refuses a loan unless both workspaces are companies. A test workspace is a test company
// (B25.3): test users lend to each other, and the wall keeps it among test workspaces.
func requireCompanies(ctx context.Context, q pgxDB, workspaceIDs ...string) error {
	var companies int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE id = ANY($1) AND (company OR synthetic)`, workspaceIDs).Scan(&companies); err != nil {
		return fmt.Errorf("economy: companies: %w", err)
	}
	if companies != len(workspaceIDs) {
		return ErrLoanCompaniesOnly
	}
	return nil
}

const loanColumns = `id, lender_workspace_id, lender_agent_id, borrower_workspace_id, borrower_agent_id, principal_ulxc, interest_bps,
	instalments, every, late_fee_ulxc, memo, fee_bps, fee_ulxc, status, paid_instalments, next_due_at, offered_at, decided_at`

func scanLoan(row pgx.Row) (Loan, error) {
	var l Loan
	err := row.Scan(&l.ID, &l.LenderWorkspaceID, &l.LenderAgentID, &l.BorrowerWorkspaceID, &l.BorrowerAgentID, &l.PrincipalULXC,
		&l.InterestBPS, &l.Instalments, &l.Every, &l.LateFeeULXC, &l.Memo, &l.FeeBPS, &l.FeeULXC, &l.Status, &l.PaidInstalments, &l.NextDueAt, &l.OfferedAt, &l.DecidedAt)
	l.Events = []LoanEvent{}
	return l, err
}

// OfferLoan offers, from workspaceID's agent lenderAgentID, a loan on terms to the agent at borrowerAddress.
func (s *DualTokenStore) OfferLoan(ctx context.Context, workspaceID, lenderAgentID, borrowerAddress string, terms LoanTerms) (Loan, error) {
	if err := terms.validate(); err != nil {
		return Loan{}, err
	}
	to, err := s.ResolveWallet(ctx, borrowerAddress)
	if err != nil {
		return Loan{}, err
	}
	if to.WorkspaceID == workspaceID {
		return Loan{}, fmt.Errorf("%w: a company cannot lend to itself", ErrLoanTerms)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Loan{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, lenderAgentID); err != nil {
		return Loan{}, err
	}
	if err := requireCompanies(ctx, tx, workspaceID, to.WorkspaceID); err != nil {
		return Loan{}, err
	}
	if err := workspace.CheckMoneyWall(ctx, tx, workspaceID, to.WorkspaceID); err != nil {
		return Loan{}, err
	}
	for _, side := range []struct{ ws, agent string }{{workspaceID, lenderAgentID}, {to.WorkspaceID, to.WalletID}} {
		var owner string
		if err := tx.QueryRow(ctx, `SELECT owner_user_id FROM agent_accounts WHERE id = $1`, side.agent).Scan(&owner); err != nil {
			return Loan{}, fmt.Errorf("economy: agent owner: %w", err)
		}
		verified := false
		if owner != "" && s.ownerVerifier != nil {
			if verified, err = s.ownerVerifier.MayEarn(ctx, tx, side.ws); err != nil {
				return Loan{}, fmt.Errorf("economy: owner verification: %w", err)
			}
		}
		if !verified {
			return Loan{}, ErrOwnerUnverified
		}
	}
	// B22.1: uncleared, an AMBER loan is made of test-funded credits only — refused now, naming the class,
	// when the lender's are not enough.
	if c, _ := CapabilityByKey(CapabilityCompanyLoans); c.Class != ClassGreen {
		// B30.8: a frozen lender lends nothing, test-funded credits included.
		if err := refuseFrozen(ctx, tx, workspaceID, c); err != nil {
			return Loan{}, err
		}
		refusal, err := capabilityLive(ctx, tx, workspaceID, c)
		if err != nil {
			return Loan{}, err
		}
		have, err := testFundedULXC(ctx, tx, workspaceID)
		if err != nil {
			return Loan{}, err
		}
		if refusal != nil && have < terms.PrincipalULXC {
			// B25.3: a test lender's credits are test money, whatever funded them.
			test, err := testWorkspace(ctx, tx, workspaceID)
			if err != nil {
				return Loan{}, err
			}
			if !test {
				return Loan{}, refusal
			}
		}
	}
	var feeBPS int64
	if s.lendingFee != nil {
		feeBPS = s.lendingFee()
	}
	l, err := scanLoan(tx.QueryRow(ctx, `INSERT INTO agent_loans (id, lender_workspace_id, lender_agent_id, borrower_workspace_id,
		borrower_agent_id, principal_ulxc, interest_bps, instalments, every, late_fee_ulxc, memo, fee_bps, fee_ulxc)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+loanColumns,
		"loan_"+uuid.NewString(), workspaceID, lenderAgentID, to.WorkspaceID, to.WalletID, terms.PrincipalULXC, terms.InterestBPS,
		terms.Instalments, terms.Every, terms.LateFeeULXC, terms.Memo, feeBPS, PlatformFee(terms.PrincipalULXC, feeBPS)))
	if err != nil {
		return Loan{}, fmt.Errorf("economy: offer loan: %w", err)
	}
	return l, tx.Commit(ctx)
}

// AnswerLoan accepts or declines a loan offered to one of workspaceID's agents. Accepting pays the principal
// out from the lender's agent, judged by its rules, takes the lending fee from it and starts the instalments.
func (s *DualTokenStore) AnswerLoan(ctx context.Context, workspaceID, loanID string, accept bool) (Loan, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Loan{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	l, err := scanLoan(tx.QueryRow(ctx, `SELECT `+loanColumns+` FROM agent_loans
		WHERE id = $1 AND borrower_workspace_id = $2 AND status = 'offered' FOR UPDATE`, loanID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Loan{}, ErrLoanNotFound
	}
	if err != nil {
		return Loan{}, err
	}
	if !accept {
		if _, err := tx.Exec(ctx, `UPDATE agent_loans SET status = 'declined', decided_at = now() WHERE id = $1`, loanID); err != nil {
			return Loan{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Loan{}, err
		}
		return s.GetLoan(ctx, workspaceID, loanID)
	}
	if err := requireCompanies(ctx, tx, l.LenderWorkspaceID, l.BorrowerWorkspaceID); err != nil {
		return Loan{}, err
	}
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: "loan:" + l.ID,
		Payee: Payee{Kind: "company", ID: l.BorrowerWorkspaceID}, Memo: l.Memo})
	t, err := s.transferTx(ctx, tx, AgentTransfer{FromWorkspaceID: l.LenderWorkspaceID, FromAgentID: l.LenderAgentID,
		ToWorkspaceID: l.BorrowerWorkspaceID, ToAgentID: l.BorrowerAgentID, AmountULXC: l.PrincipalULXC,
		Memo: "loan " + l.ID + ": payout", LoanID: l.ID, capability: CapabilityCompanyLoans}, true)
	if err != nil {
		return Loan{}, s.refusedMovement(ctx, tx, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_loans SET status = 'active', decided_at = $2, next_due_at = $3 WHERE id = $1`,
		loanID, t.CreatedAt, dueOf(t.CreatedAt, l.Every, 1)); err != nil {
		return Loan{}, err
	}
	if err := takeLendingFee(ctx, tx, l); err != nil {
		return Loan{}, err
	}
	if err := loanEvent(ctx, tx, l.ID, LoanEvent{Kind: "payout", PrincipalULXC: l.PrincipalULXC, FeeULXC: l.FeeULXC, TransferID: t.ID}); err != nil {
		return Loan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Loan{}, err
	}
	return s.GetLoan(ctx, workspaceID, loanID)
}

// WithdrawLoan takes back a loan workspaceID offered and that has not been answered.
func (s *DualTokenStore) WithdrawLoan(ctx context.Context, workspaceID, loanID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_loans SET status = 'withdrawn', decided_at = now()
		WHERE id = $1 AND lender_workspace_id = $2 AND status = 'offered'`, loanID, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLoanNotFound
	}
	return nil
}

// takeLendingFee takes a just-paid-out loan's fee from the borrower's agent, which has just received the principal:
// its workspace's balance and ledger, and an entry from the agent to the spend side naming the loan and the rate.
// It judges nothing — the fee is Talyvor's, agreed with the offer, and comes out of the payout.
func takeLendingFee(ctx context.Context, tx pgx.Tx, l Loan) error {
	if l.FeeULXC == 0 {
		return nil
	}
	bal, minted, spent, err := readLXCBalance(ctx, tx, l.BorrowerWorkspaceID)
	if err != nil {
		return err
	}
	if err := insertLXCLedger(ctx, tx, l.BorrowerWorkspaceID, -l.FeeULXC, bal-l.FeeULXC, LXCTypeLendingFee, LendingFeeLabel(l.FeeBPS)+" on loan "+l.ID,
		map[string]interface{}{"loan_id": l.ID, "lending_fee_bps": l.FeeBPS, "principal_ulxc": l.PrincipalULXC}); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, l.BorrowerWorkspaceID, bal-l.FeeULXC, minted, spent+l.FeeULXC); err != nil {
		return err
	}
	entry := uuid.New()
	for _, p := range []leg{{agentAccount(l.BorrowerAgentID), -l.FeeULXC}, {"spend", l.FeeULXC}} {
		if err := insertPosting(ctx, tx, `entry_id, workspace_id, account, amount_ulxc, kind, ref, fee_bps`,
			entry, l.BorrowerWorkspaceID, p.account, p.amount, LXCTypeLendingFee, l.ID, l.FeeBPS); err != nil {
			return fmt.Errorf("economy: post %s: %w", LXCTypeLendingFee, err)
		}
	}
	return nil
}

func loanEvent(ctx context.Context, tx pgx.Tx, loanID string, e LoanEvent) error {
	_, err := tx.Exec(ctx, `INSERT INTO agent_loan_events (loan_id, kind, instalment, principal_ulxc, interest_ulxc, late_fee_ulxc, fee_ulxc, transfer_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, loanID, e.Kind, e.Instalment, e.PrincipalULXC, e.InterestULXC, e.LateFeeULXC, e.FeeULXC, e.TransferID, e.Detail)
	if err != nil {
		return fmt.Errorf("economy: loan event: %w", err)
	}
	return nil
}

// GetLoan reads a loan workspaceID lends or borrows, with its events.
func (s *DualTokenStore) GetLoan(ctx context.Context, workspaceID, loanID string) (Loan, error) {
	loans, err := s.loans(ctx, `WHERE id = $2 AND (lender_workspace_id = $1 OR borrower_workspace_id = $1)`, workspaceID, loanID)
	if err != nil {
		return Loan{}, err
	}
	if len(loans) == 0 {
		return Loan{}, ErrLoanNotFound
	}
	return loans[0], nil
}

// BringTestLoanDue — B25.7: a TEST loan's next instalment made due at now, so that a tester run, which lasts
// minutes, sees what happens a period after acceptance: the minute tick (RunLoanRepayments) takes it, or misses
// it — and brought due again while late, the loan defaults. Only a loan marked test (migration 0173) that
// workspaceID lends or borrows, active or late, moves; any other is ErrLoanNotFound and nothing changes.
func (s *DualTokenStore) BringTestLoanDue(ctx context.Context, workspaceID, loanID string, now time.Time) (Loan, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_loans SET next_due_at = LEAST(next_due_at, $3)
		WHERE id = $2 AND (lender_workspace_id = $1 OR borrower_workspace_id = $1) AND test AND status IN ('active', 'late')`,
		workspaceID, loanID, now)
	if err != nil {
		return Loan{}, fmt.Errorf("economy: bring a test loan due: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Loan{}, ErrLoanNotFound
	}
	return s.GetLoan(ctx, workspaceID, loanID)
}

// ListLoans reads the loans workspaceID lends or borrows, newest first, with their events.
func (s *DualTokenStore) ListLoans(ctx context.Context, workspaceID string) ([]Loan, error) {
	return s.loans(ctx, `WHERE lender_workspace_id = $1 OR borrower_workspace_id = $1`, workspaceID)
}

func (s *DualTokenStore) loans(ctx context.Context, where string, args ...any) ([]Loan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+loanColumns+` FROM agent_loans `+where+` ORDER BY offered_at DESC, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("economy: loans: %w", err)
	}
	var out []Loan
	byID := map[string]int{}
	for rows.Next() {
		l, err := scanLoan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		byID[l.ID] = len(out)
		out = append(out, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	ids := make([]string, 0, len(out))
	for id := range byID {
		ids = append(ids, id)
	}
	rows, err = s.pool.Query(ctx, `SELECT loan_id, kind, instalment, principal_ulxc, interest_ulxc, late_fee_ulxc, fee_ulxc, transfer_id, detail, at
		FROM agent_loan_events WHERE loan_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, fmt.Errorf("economy: loan events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var e LoanEvent
		if err := rows.Scan(&id, &e.Kind, &e.Instalment, &e.PrincipalULXC, &e.InterestULXC, &e.LateFeeULXC, &e.FeeULXC, &e.TransferID, &e.Detail, &e.At); err != nil {
			return nil, err
		}
		out[byID[id]].Events = append(out[byID[id]].Events, e)
	}
	return out, rows.Err()
}

// LoanRunResult is what one run of the loans did.
type LoanRunResult struct {
	Paid, Missed, Defaulted int
}

// RunLoanRepayments takes every instalment due by now, each loan in its own transaction.
func (s *DualTokenStore) RunLoanRepayments(ctx context.Context, now time.Time) (LoanRunResult, error) {
	var res LoanRunResult
	for range maxTicksPerRun {
		kind, err := s.runLoanTick(ctx, now)
		if err != nil {
			return res, err
		}
		switch kind {
		case "":
			return res, nil
		case "instalment":
			res.Paid++
		case "late":
			res.Missed++
		case "defaulted":
			res.Missed++
			res.Defaulted++
		}
	}
	return res, nil
}

// runLoanTick takes the earliest due instalment of any loan, holding the loan's row, and returns what
// happened: instalment, late, defaulted — or "" when nothing is due.
func (s *DualTokenStore) runLoanTick(ctx context.Context, now time.Time) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	l, err := scanLoan(tx.QueryRow(ctx, `SELECT `+loanColumns+` FROM agent_loans
		WHERE status IN ('active', 'late') AND next_due_at <= $1 ORDER BY next_due_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("economy: due loan: %w", err)
	}
	k := l.PaidInstalments + 1
	principal, interest := l.instalment(k)
	var fee int64
	if l.Status == "late" {
		fee = l.LateFeeULXC
	}
	// The instalment in a savepoint: a refusal is undone and recorded as missed.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", err
	}
	t, perr := s.transferTx(ctx, sp, AgentTransfer{FromWorkspaceID: l.BorrowerWorkspaceID, FromAgentID: l.BorrowerAgentID,
		ToWorkspaceID: l.LenderWorkspaceID, ToAgentID: l.LenderAgentID, AmountULXC: principal + interest + fee,
		Memo: fmt.Sprintf("loan %s: instalment %d of %d", l.ID, k, l.Instalments), LoanID: l.ID, capability: CapabilityCompanyLoans}, false)
	var kind string
	switch {
	case perr == nil:
		if err := sp.Commit(ctx); err != nil {
			return "", err
		}
		kind = "instalment"
		status, next := "active", any(dueOf(*l.DecidedAt, l.Every, k+1))
		if k == l.Instalments {
			status, next = "repaid", nil
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_loans SET status = $2, paid_instalments = $3, next_due_at = $4 WHERE id = $1`,
			l.ID, status, k, next); err != nil {
			return "", err
		}
		if err := loanEvent(ctx, tx, l.ID, LoanEvent{Kind: "instalment", Instalment: k, PrincipalULXC: principal, InterestULXC: interest,
			LateFeeULXC: fee, TransferID: t.ID}); err != nil {
			return "", err
		}
	case errors.Is(perr, ErrAgentFunds), errors.Is(perr, ErrCapabilityNotCleared), errors.Is(perr, ErrOwnerUnverified),
		errors.Is(perr, ErrAgentNotFound), errors.Is(perr, workspace.ErrMoneyWall):
		if err := sp.Rollback(ctx); err != nil {
			return "", err
		}
		if err := loanEvent(ctx, tx, l.ID, LoanEvent{Kind: "missed", Instalment: k, PrincipalULXC: principal, InterestULXC: interest,
			LateFeeULXC: fee, Detail: perr.Error()}); err != nil {
			return "", err
		}
		// Missed once: late, and tried again one period on. Missed again: in default.
		status, next := "late", any(dueOf(*l.NextDueAt, l.Every, 1))
		kind = "late"
		if l.Status == "late" {
			kind, status, next = "defaulted", "defaulted", nil
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_loans SET status = $2, next_due_at = $3 WHERE id = $1`, l.ID, status, next); err != nil {
			return "", err
		}
		if err := loanEvent(ctx, tx, l.ID, LoanEvent{Kind: kind, Instalment: k}); err != nil {
			return "", err
		}
	default:
		return "", perr
	}
	return kind, tx.Commit(ctx)
}
