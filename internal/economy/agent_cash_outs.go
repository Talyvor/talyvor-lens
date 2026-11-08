package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_cash_outs.go — B22.9: CASH-OUT TO MONEY, BEHIND A LICENSED PARTNER.
//
// An owner asks to turn an agent's credits back into money in their bank account (RequestCashOut). The credits
// leave the agent at once and are held: one agent_postings entry agent:<id> −amount / cashout:<id> +amount, and
// the LXC leaves the workspace's lxc_balances (an lxc_ledger row of type LXCTypeCashOut), so nothing can spend
// it while the partner pays. RunCashOuts, on the schedules' tick, hands each held request to the partner and
// asks after each submitted one: paid, the held credits are gone for good (cashout:<id> → 'cashed_out'); failed,
// they go back to the agent, test-funded as they left.
//
// Lens knows one partner-neutral interface, CashOutPartner, and has one implementation: TestCashOutPartner,
// which pays nothing. The real one waits for a licensed partner. Cash-out is class RED: until the operator
// records a clearance it takes test-funded credits only, and a request of live money is refused naming the
// class. The owner must be verified (B19.11). Marketplace seller payouts (B20.5) are not this.

// CapabilityCashOut is the wallet capability cash-out moves money as.
const CapabilityCashOut = "cash_out"

// LXCTypeCashOut marks credits leaving a workspace to be cashed out, or coming back when a cash-out fails.
const LXCTypeCashOut = "agent_cash_out"

// ErrNoCashOutPartner: no partner can pay a cash-out out.
var ErrNoCashOutPartner = errors.New("economy: cash-out needs a partner, and none is configured")

// ErrCashOut: a request that is not one.
var ErrCashOut = errors.New("economy: cash-out")

// CashOut is one request to turn an agent's credits into money.
type CashOut struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	AgentID        string     `json:"agent_id"`
	AmountULXC     int64      `json:"amount_ulxc"`
	AmountUUSD     int64      `json:"amount_uusd"`
	TestFundedULXC int64      `json:"test_funded_ulxc"`
	Destination    string     `json:"destination"`
	Partner        string     `json:"partner"`
	PartnerRef     string     `json:"partner_ref,omitempty"`
	Status         string     `json:"status"` // held | submitted | paid | failed
	Detail         string     `json:"detail,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
}

// CashOutPartner is what pays a cash-out's money out. A licensed partner's implementation is the only one
// that may move real money.
type CashOutPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Pay asks the partner to pay c.AmountUUSD to c.Destination and returns its reference for the payment. It
	// must be idempotent on c.ID: asked again for the same cash-out (a tick after a lost commit), it pays once and
	// returns the same reference.
	Pay(ctx context.Context, c CashOut) (ref string, err error)
	// Status says where the payment ref stands: "submitted" while under way, "paid", or "failed" with why.
	Status(ctx context.Context, ref string) (status, detail string, err error)
}

// TestCashOutPartner is test mode: it pays nothing and reports every payment paid — except to a destination
// starting "test-fail", which it reports failed, so the way back can be seen too.
type TestCashOutPartner struct{}

// Name is "test".
func (TestCashOutPartner) Name() string { return "test" }

// Pay pays nothing.
func (TestCashOutPartner) Pay(_ context.Context, c CashOut) (string, error) {
	if strings.HasPrefix(c.Destination, "test-fail") {
		return "test_fail_" + c.ID, nil
	}
	return "test_" + c.ID, nil
}

// Status reports the payment paid, or failed for a "test-fail" destination.
func (TestCashOutPartner) Status(_ context.Context, ref string) (string, string, error) {
	if strings.HasPrefix(ref, "test_fail_") {
		return "failed", "test mode: the destination asked for a failure", nil
	}
	return "paid", "", nil
}

// SetCashOutPartner sets what pays cash-outs out.
func (s *DualTokenStore) SetCashOutPartner(p CashOutPartner) { s.cashOutPartner = p }

const cashOutColumns = `id, workspace_id, agent_id, amount_ulxc, amount_uusd, test_funded_ulxc, destination, partner, partner_ref, status,
	detail, created_at, decided_at`

func scanCashOut(row pgx.Row) (CashOut, error) {
	var c CashOut
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.AgentID, &c.AmountULXC, &c.AmountUUSD, &c.TestFundedULXC, &c.Destination, &c.Partner,
		&c.PartnerRef, &c.Status, &c.Detail, &c.CreatedAt, &c.DecidedAt)
	return c, err
}

// RequestCashOut holds amount µLXC of workspaceID's agent to be paid out as money to destination, and hands it
// to the partner.
func (s *DualTokenStore) RequestCashOut(ctx context.Context, workspaceID, agentID string, amount int64, destination, requestedBy string) (CashOut, error) {
	destination = strings.TrimSpace(destination)
	switch {
	case amount <= 0:
		return CashOut{}, fmt.Errorf("%w: the amount must be positive", ErrCashOut)
	case destination == "" || len(destination) > 64:
		return CashOut{}, fmt.Errorf("%w: a destination is a label of up to 64 characters for the account; the partner holds its details", ErrCashOut)
	case s.cashOutPartner == nil:
		return CashOut{}, ErrNoCashOutPartner
	}
	c := CashOut{ID: "cash_" + uuid.NewString(), WorkspaceID: workspaceID, AgentID: agentID, AmountULXC: amount,
		AmountUUSD: amount / ULXCPerUSDMicro, Destination: destination, Partner: s.cashOutPartner.Name(), Status: "held"}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CashOut{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return CashOut{}, err
	}
	if err := refuseIfPaused(ctx, tx, agentID); err != nil {
		return CashOut{}, err
	}
	verified := false
	if s.ownerVerifier != nil {
		if verified, err = s.ownerVerifier.MayEarn(ctx, tx, workspaceID); err != nil {
			return CashOut{}, fmt.Errorf("economy: owner verification: %w", err)
		}
	}
	if !verified {
		return CashOut{}, ErrOwnerUnverified
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(agentID))
	if err != nil {
		return CashOut{}, err
	}
	if bal < amount {
		return CashOut{}, agentHolds(ctx, tx, ErrAgentFunds, agentID, bal, 0)
	}
	if _, _, _, err := readLXCBalance(ctx, tx, workspaceID); err != nil {
		return CashOut{}, err
	}
	testBefore, err := testFundedULXC(ctx, tx, workspaceID)
	if err != nil {
		return CashOut{}, err
	}
	// B22.1: RED and uncleared, cash-out takes test-funded credits only, or is refused naming the class.
	if _, err := spendForCapability(ctx, tx, workspaceID, CapabilityCashOut, amount); err != nil {
		return CashOut{}, err
	}
	if err := moveCashOutLXC(ctx, tx, workspaceID, -amount, c, "credits held to be cashed out"); err != nil {
		return CashOut{}, err
	}
	testAfter, err := testFundedULXC(ctx, tx, workspaceID)
	if err != nil {
		return CashOut{}, err
	}
	c.TestFundedULXC = max(testBefore-testAfter, 0)
	if err := postEntry(ctx, tx, workspaceID, "cash_out", c.ID, leg{agentAccount(agentID), -amount}, leg{cashOutAccount(c.ID), amount}); err != nil {
		return CashOut{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_cash_outs (id, workspace_id, agent_id, amount_ulxc, amount_uusd, test_funded_ulxc, destination,
		partner, requested_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, c.ID, workspaceID, agentID, amount, c.AmountUUSD, c.TestFundedULXC,
		destination, c.Partner, requestedBy); err != nil {
		return CashOut{}, fmt.Errorf("economy: record cash-out: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CashOut{}, err
	}
	// Handed to the partner now; one that does not take it is tried again on the tick.
	_ = s.submitCashOut(ctx, c.ID)
	return s.getCashOut(ctx, workspaceID, c.ID)
}

func cashOutAccount(id string) string { return "cashout:" + id }

// moveCashOutLXC moves delta µLXC out of workspaceID's balance for cash-out c, or back into it, with its ledger
// row.
func moveCashOutLXC(ctx context.Context, tx pgx.Tx, workspaceID string, delta int64, c CashOut, desc string) error {
	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if err := insertLXCLedger(ctx, tx, workspaceID, delta, bal+delta, LXCTypeCashOut, desc, map[string]interface{}{
		"cash_out_id": c.ID, "agent_id": c.AgentID, "partner": c.Partner, "class": string(ClassRed)}); err != nil {
		return err
	}
	return writeLXCBalance(ctx, tx, workspaceID, bal+delta, minted, spent)
}

// submitCashOut hands a held cash-out to the partner.
func (s *DualTokenStore) submitCashOut(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanCashOut(tx.QueryRow(ctx, `SELECT `+cashOutColumns+` FROM agent_cash_outs WHERE id = $1 AND status = 'held' FOR UPDATE SKIP LOCKED`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ref, err := s.cashOutPartner.Pay(ctx, c)
	if err != nil {
		return fmt.Errorf("economy: cash-out partner: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_cash_outs SET status = 'submitted', partner_ref = $2 WHERE id = $1`, id, ref); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// settleCashOut asks the partner after a submitted cash-out and records what it says: paid, the held credits
// are gone; failed, they go back to the agent. It says what it recorded ("" for still under way).
func (s *DualTokenStore) settleCashOut(ctx context.Context, id string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanCashOut(tx.QueryRow(ctx, `SELECT `+cashOutColumns+` FROM agent_cash_outs WHERE id = $1 AND status = 'submitted' FOR UPDATE SKIP LOCKED`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	status, detail, err := s.cashOutPartner.Status(ctx, c.PartnerRef)
	if err != nil {
		return "", fmt.Errorf("economy: cash-out partner: %w", err)
	}
	switch status {
	case "paid":
		if err := postEntry(ctx, tx, c.WorkspaceID, "cash_out", c.ID, leg{cashOutAccount(c.ID), -c.AmountULXC}, leg{"cashed_out", c.AmountULXC}); err != nil {
			return "", err
		}
	case "failed":
		if err := lockAgent(ctx, tx, c.WorkspaceID, c.AgentID); err != nil {
			return "", err
		}
		if err := moveCashOutLXC(ctx, tx, c.WorkspaceID, c.AmountULXC, c, "cash-out failed: credits returned"); err != nil {
			return "", err
		}
		if err := addTestFunded(ctx, tx, c.WorkspaceID, c.TestFundedULXC); err != nil {
			return "", err
		}
		if err := postEntry(ctx, tx, c.WorkspaceID, "cash_out", c.ID, leg{cashOutAccount(c.ID), -c.AmountULXC}, leg{agentAccount(c.AgentID), c.AmountULXC}); err != nil {
			return "", err
		}
	default:
		return "", nil
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_cash_outs SET status = $2, detail = $3, decided_at = now() WHERE id = $1`, c.ID, status, detail); err != nil {
		return "", err
	}
	return status, tx.Commit(ctx)
}

// CashOutRunResult is what one run of the cash-outs did.
type CashOutRunResult struct {
	Submitted, Paid, Failed int
}

// RunCashOuts hands every held cash-out to the partner and records what it says of every submitted one.
func (s *DualTokenStore) RunCashOuts(ctx context.Context) (CashOutRunResult, error) {
	var res CashOutRunResult
	if s.cashOutPartner == nil {
		return res, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, status FROM agent_cash_outs WHERE status IN ('held', 'submitted') ORDER BY created_at, id LIMIT $1`, maxTicksPerRun)
	if err != nil {
		return res, fmt.Errorf("economy: cash-outs: %w", err)
	}
	type pending struct{ id, status string }
	var due []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.status); err != nil {
			rows.Close()
			return res, err
		}
		due = append(due, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, p := range due {
		if p.status == "held" {
			if err := s.submitCashOut(ctx, p.id); err != nil {
				return res, err
			}
			res.Submitted++
			continue
		}
		switch got, err := s.settleCashOut(ctx, p.id); {
		case err != nil:
			return res, err
		case got == "paid":
			res.Paid++
		case got == "failed":
			res.Failed++
		}
	}
	return res, nil
}

func (s *DualTokenStore) getCashOut(ctx context.Context, workspaceID, id string) (CashOut, error) {
	return scanCashOut(s.pool.QueryRow(ctx, `SELECT `+cashOutColumns+` FROM agent_cash_outs WHERE id = $1 AND workspace_id = $2`, id, workspaceID))
}

// ListCashOuts reads workspaceID's cash-outs, newest first.
func (s *DualTokenStore) ListCashOuts(ctx context.Context, workspaceID string) ([]CashOut, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cashOutColumns+` FROM agent_cash_outs WHERE workspace_id = $1 ORDER BY created_at DESC, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: cash-outs: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (CashOut, error) { return scanCashOut(row) })
}
