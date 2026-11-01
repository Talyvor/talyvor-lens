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

// money_allowances.go — B30.27: A MONTHLY ALLOWANCE IN MONEY FOR EACH AGENT.
//
// The owner sets, for one agent and one currency, an amount and a day of the month (1–28, so every month has it). On
// that day, from midnight UTC, the company's account in the currency pays the amount into the agent's account in it.
// With sweep_back, what the agent's account still holds of that funding is first paid back to the company's, so each
// month starts from the allowance alone. Both are money entries (currency_accounts) under keys named by the allowance
// and the month, so they are lines on both accounts; each month is one money_allowance_runs row (migration 0298), and
// a month that has a row is never run again. A company account holding less than the allowance funds nothing that
// month, and its run says why; so does any refusal of the entry, such as live money before currency_accounts is
// cleared. A month missed while Lens was down is not made up: the latest one due runs.

var (
	// ErrMoneyAllowance: an allowance asked for with an amount, day, currency or funding it cannot have.
	ErrMoneyAllowance = errors.New("economy: invalid allowance")
	// ErrMoneyAllowanceNotFound: no allowance by that id in this workspace.
	ErrMoneyAllowanceNotFound = errors.New("economy: no such allowance")
)

// MoneyAllowance is what one agent is paid each month in one currency.
type MoneyAllowance struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	AgentID     string     `json:"agent_id"`
	Currency    string     `json:"currency"`
	AmountMinor int64      `json:"amount_minor"`
	DayOfMonth  int        `json:"day_of_month"`
	SweepBack   bool       `json:"sweep_back"`
	Funding     string     `json:"funding"`
	NextRunAt   time.Time  `json:"next_run_at"`
	StoppedAt   *time.Time `json:"stopped_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// MoneyAllowanceRun is one month of an allowance: what was swept back, what was paid in, and the entries that did it.
type MoneyAllowanceRun struct {
	AllowanceID  string    `json:"allowance_id"`
	Period       string    `json:"period"` // YYYY-MM
	SweptMinor   int64     `json:"swept_minor"`
	FundedMinor  int64     `json:"funded_minor"`
	SweepEntryID string    `json:"sweep_entry_id,omitempty"`
	FundEntryID  string    `json:"fund_entry_id,omitempty"`
	Status       string    `json:"status"` // funded | refused
	Detail       string    `json:"detail,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

const allowanceColumns = `id, workspace_id, agent_id, currency, amount_minor, day_of_month, sweep_back, funding, next_run_at, stopped_at, created_at`

func scanAllowance(row pgx.Row) (MoneyAllowance, error) {
	var a MoneyAllowance
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.AgentID, &a.Currency, &a.AmountMinor, &a.DayOfMonth, &a.SweepBack, &a.Funding,
		&a.NextRunAt, &a.StoppedAt, &a.CreatedAt)
	return a, err
}

// nextAllowanceDay is the first midnight UTC on day of the month that is not before today.
func nextAllowanceDay(now time.Time, day int) time.Time {
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, time.UTC)
	if now.Day() > day {
		t = t.AddDate(0, 1, 0)
	}
	return t
}

// SetMoneyAllowance sets a's agent's allowance in a's currency, or changes it, and starts it again if it was stopped.
// The company's and the agent's accounts in that currency must be open. A changed day of the month moves the next
// run; a month that already ran is not run again.
func (s *DualTokenStore) SetMoneyAllowance(ctx context.Context, a MoneyAllowance) (MoneyAllowance, error) {
	a.Currency = strings.ToUpper(strings.TrimSpace(a.Currency))
	if a.Funding == "" {
		a.Funding = FundingTest
	}
	switch {
	case a.Currency != CurrencyGBP && a.Currency != CurrencyEUR && a.Currency != CurrencyUSD:
		return MoneyAllowance{}, fmt.Errorf("%w: an allowance is in GBP, EUR or USD, not %q", ErrMoneyAllowance, a.Currency)
	case a.AmountMinor <= 0:
		return MoneyAllowance{}, fmt.Errorf("%w: the amount must be above zero", ErrMoneyAllowance)
	case a.DayOfMonth < 1 || a.DayOfMonth > 28:
		return MoneyAllowance{}, fmt.Errorf("%w: the day of the month is 1 to 28, not %d", ErrMoneyAllowance, a.DayOfMonth)
	case a.Funding != FundingTest && a.Funding != FundingLive:
		return MoneyAllowance{}, fmt.Errorf("%w: funding is test or live, not %q", ErrMoneyAllowance, a.Funding)
	}
	if a.Funding == FundingLive {
		c, _ := CapabilityByKey(CapabilityCurrencyAccounts)
		refusal, err := capabilityLive(ctx, s.pool, a.WorkspaceID, c)
		if err != nil {
			return MoneyAllowance{}, err
		}
		if refusal != nil {
			return MoneyAllowance{}, refusal
		}
	}
	var company, agent bool
	if err := s.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM money_accounts WHERE workspace_id = $1 AND currency = $3 AND purpose = 'company' AND status = 'open'),
		EXISTS (SELECT 1 FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2 AND currency = $3 AND purpose = 'agent' AND status = 'open')`,
		a.WorkspaceID, a.AgentID, a.Currency).Scan(&company, &agent); err != nil {
		return MoneyAllowance{}, fmt.Errorf("economy: set allowance: %w", err)
	}
	if !company || !agent {
		return MoneyAllowance{}, fmt.Errorf("%w: open the company's and the agent's %s accounts first", ErrMoneyAccountNotFound, a.Currency)
	}
	out, err := scanAllowance(s.pool.QueryRow(ctx, `INSERT INTO money_allowances
		(id, workspace_id, agent_id, currency, amount_minor, day_of_month, sweep_back, funding, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (agent_id, currency) DO UPDATE SET amount_minor = EXCLUDED.amount_minor, sweep_back = EXCLUDED.sweep_back,
			funding = EXCLUDED.funding, day_of_month = EXCLUDED.day_of_month, stopped_at = NULL, updated_at = now(),
			next_run_at = CASE WHEN money_allowances.day_of_month = EXCLUDED.day_of_month AND money_allowances.stopped_at IS NULL
				THEN money_allowances.next_run_at ELSE EXCLUDED.next_run_at END
		WHERE money_allowances.workspace_id = EXCLUDED.workspace_id
		RETURNING `+allowanceColumns,
		"mal_"+uuid.NewString(), a.WorkspaceID, a.AgentID, a.Currency, a.AmountMinor, a.DayOfMonth, a.SweepBack, a.Funding,
		nextAllowanceDay(time.Now(), a.DayOfMonth)))
	if err != nil {
		return MoneyAllowance{}, fmt.Errorf("economy: set allowance: %w", err)
	}
	return out, nil
}

// MoneyAllowances lists workspaceID's allowances, or with agentID that agent's.
func (s *DualTokenStore) MoneyAllowances(ctx context.Context, workspaceID, agentID string) ([]MoneyAllowance, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+allowanceColumns+` FROM money_allowances
		WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2) ORDER BY created_at, id`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: allowances: %w", err)
	}
	defer rows.Close()
	out := []MoneyAllowance{}
	for rows.Next() {
		a, err := scanAllowance(rows)
		if err != nil {
			return nil, fmt.Errorf("economy: allowances: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StopMoneyAllowance stops workspaceID's allowance id: no month after this one is paid. Its runs stay.
func (s *DualTokenStore) StopMoneyAllowance(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE money_allowances SET stopped_at = COALESCE(stopped_at, now()), updated_at = now()
		WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: stop allowance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMoneyAllowanceNotFound
	}
	return nil
}

// MoneyAllowanceRuns is every month workspaceID's allowance id ran, the latest first.
func (s *DualTokenStore) MoneyAllowanceRuns(ctx context.Context, workspaceID, id string) ([]MoneyAllowanceRun, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_allowances WHERE id = $1 AND workspace_id = $2)`,
		id, workspaceID).Scan(&ok); err != nil {
		return nil, fmt.Errorf("economy: allowance runs: %w", err)
	}
	if !ok {
		return nil, ErrMoneyAllowanceNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT allowance_id, period, swept_minor, funded_minor, COALESCE(sweep_entry_id, ''),
		COALESCE(fund_entry_id, ''), status, detail, created_at FROM money_allowance_runs WHERE allowance_id = $1 ORDER BY period DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("economy: allowance runs: %w", err)
	}
	defer rows.Close()
	out := []MoneyAllowanceRun{}
	for rows.Next() {
		var r MoneyAllowanceRun
		if err := rows.Scan(&r.AllowanceID, &r.Period, &r.SweptMinor, &r.FundedMinor, &r.SweepEntryID, &r.FundEntryID,
			&r.Status, &r.Detail, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("economy: allowance runs: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunMoneyAllowances runs every allowance due at now, each in its own transaction with its row held, up to 200 a run.
func (s *DualTokenStore) RunMoneyAllowances(ctx context.Context, now time.Time) (funded, refused int, err error) {
	for i := 0; i < 200; i++ {
		ran, ok, err := s.runOneMoneyAllowance(ctx, now)
		if err != nil || !ok {
			return funded, refused, err
		}
		switch ran {
		case "funded":
			funded++
		case "refused":
			refused++
		}
	}
	return funded, refused, nil
}

// runOneMoneyAllowance runs the allowance due longest, if one is due: its status, "" when the month had already run.
func (s *DualTokenStore) runOneMoneyAllowance(ctx context.Context, now time.Time) (status string, ok bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("economy: run allowances: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanAllowance(tx.QueryRow(ctx, `SELECT `+allowanceColumns+` FROM money_allowances
		WHERE next_run_at <= $1 AND stopped_at IS NULL ORDER BY next_run_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("economy: run allowances: %w", err)
	}
	due := a.NextRunAt
	for !due.AddDate(0, 1, 0).After(now) {
		due = due.AddDate(0, 1, 0)
	}
	run := MoneyAllowanceRun{AllowanceID: a.ID, Period: due.UTC().Format("2006-01")}
	var ran bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_allowance_runs WHERE allowance_id = $1 AND period = $2)`,
		a.ID, run.Period).Scan(&ran); err != nil {
		return "", false, fmt.Errorf("economy: run allowances: %w", err)
	}
	if !ran {
		if err := s.payMoneyAllowance(ctx, tx, a, &run); err != nil {
			return "", false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO money_allowance_runs (allowance_id, period, swept_minor, funded_minor, sweep_entry_id,
			fund_entry_id, status, detail) VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8)`,
			run.AllowanceID, run.Period, run.SweptMinor, run.FundedMinor, run.SweepEntryID, run.FundEntryID, run.Status, run.Detail); err != nil {
			return "", false, fmt.Errorf("economy: run allowances: %w", err)
		}
		status = run.Status
	}
	if _, err := tx.Exec(ctx, `UPDATE money_allowances SET next_run_at = $2 WHERE id = $1`, a.ID, due.AddDate(0, 1, 0)); err != nil {
		return "", false, fmt.Errorf("economy: run allowances: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("economy: run allowances: %w", err)
	}
	return status, true, nil
}

// payMoneyAllowance sweeps back and pays in a's month into run, in a savepoint of tx: a refusal moves no money, and
// is the run's detail. Only a failure to reach the database is returned.
func (s *DualTokenStore) payMoneyAllowance(ctx context.Context, tx pgx.Tx, a MoneyAllowance, run *MoneyAllowanceRun) error {
	refuse := func(detail string) error {
		run.Status, run.Detail, run.SweptMinor, run.FundedMinor, run.SweepEntryID, run.FundEntryID = "refused", detail, 0, 0, "", ""
		return nil
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: run allowances: %w", err)
	}
	defer func() { _ = sp.Rollback(ctx) }()
	var company, agent string
	err = sp.QueryRow(ctx, `SELECT
		COALESCE((SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $3 AND purpose = 'company' AND status = 'open'), ''),
		COALESCE((SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2 AND currency = $3 AND purpose = 'agent' AND status = 'open'), '')`,
		a.WorkspaceID, a.AgentID, a.Currency).Scan(&company, &agent)
	if err != nil {
		return fmt.Errorf("economy: run allowances: %w", err)
	}
	if company == "" || agent == "" {
		return refuse(fmt.Sprintf("the company's or the agent's %s account is not open", a.Currency))
	}
	held := func(account string) (int64, error) {
		var n int64
		err := sp.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
			account, a.Funding).Scan(&n)
		return n, err
	}
	key := "allowance:" + a.ID + ":" + run.Period
	if a.SweepBack {
		left, err := held(agent)
		if err != nil {
			return fmt.Errorf("economy: run allowances: %w", err)
		}
		if left > 0 {
			e, err := postMoneyTx(ctx, sp, MoneyEntry{WorkspaceID: a.WorkspaceID, Capability: CapabilityCurrencyAccounts, Kind: "allowance_sweep",
				IdempotencyKey: key + ":sweep", Funding: a.Funding, Memo: "Unspent allowance back to the company, " + run.Period,
				Postings: []MoneyPosting{{AccountID: agent, AmountMinor: -left}, {AccountID: company, AmountMinor: left}}}, s.screener)
			if err != nil {
				return refuse(err.Error())
			}
			run.SweptMinor, run.SweepEntryID = left, e.ID
		}
	}
	has, err := held(company)
	if err != nil {
		return fmt.Errorf("economy: run allowances: %w", err)
	}
	if has < a.AmountMinor {
		// The sweep stands: it is the agent's unspent money going home, whether or not this month is paid.
		if err := sp.Commit(ctx); err != nil {
			return fmt.Errorf("economy: run allowances: %w", err)
		}
		run.Status, run.Detail = "refused", fmt.Sprintf("the company's %s account holds %s, less than the allowance of %s",
			a.Currency, minorString(has, a.Currency), minorString(a.AmountMinor, a.Currency))
		return nil
	}
	e, err := postMoneyTx(ctx, sp, MoneyEntry{WorkspaceID: a.WorkspaceID, Capability: CapabilityCurrencyAccounts, Kind: "allowance",
		IdempotencyKey: key + ":fund", Funding: a.Funding, Memo: "Monthly allowance, " + run.Period,
		Postings: []MoneyPosting{{AccountID: company, AmountMinor: -a.AmountMinor}, {AccountID: agent, AmountMinor: a.AmountMinor}}}, s.screener)
	if err != nil {
		return refuse(err.Error())
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("economy: run allowances: %w", err)
	}
	run.Status, run.FundedMinor, run.FundEntryID = "funded", a.AmountMinor, e.ID
	return nil
}
