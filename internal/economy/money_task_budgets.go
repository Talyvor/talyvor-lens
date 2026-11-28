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

// money_task_budgets.go — B28.310: A BUDGET FOR ONE TASK, AND WHAT IS LEFT OF IT GOES BACK WHEN IT ENDS.
//
// The owner gives one agent an amount in one currency for a task, in the owner's words, until a time. Setting it pays
// the amount from the company's account in the currency into the agent's at once (kind task_budget). At that time — or
// when the owner ends it first — the part not spent goes back to the company's account in one entry (kind
// task_budget_return) under a key named by the budget, so it is posted once however often it is run. What was spent is
// what left the agent's account in the budget's funding since it was funded, less card holds released back to it;
// money going home (a task budget's return, an allowance's sweep) is not spending. The return is the amount less that,
// never below zero and never more than the account then holds. The agent's words with a payment (agent_task.go) are
// never read: they are what the agent says, not which money it spent. A return that is refused (the account frozen or
// closed, live money not cleared) moves nothing, says why, and is tried again when the owner ends the budget.

var (
	// ErrMoneyTaskBudget: a task budget asked for with a task, amount, currency, funding or end it cannot have.
	ErrMoneyTaskBudget = errors.New("economy: invalid task budget")
	// ErrMoneyTaskBudgetFunds: the company's account holds less than the budget.
	ErrMoneyTaskBudgetFunds = errors.New("economy: the company's account holds less than the task budget")
	// ErrMoneyTaskBudgetNotFound: no task budget by that id in this workspace.
	ErrMoneyTaskBudgetNotFound = errors.New("economy: no such task budget")
)

// MoneyTaskBudget is one agent's budget for one task, and what came of it.
type MoneyTaskBudget struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	AgentID        string     `json:"agent_id"`
	Task           string     `json:"task"`
	Currency       string     `json:"currency"`
	AmountMinor    int64      `json:"amount_minor"`
	Funding        string     `json:"funding"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
	FundEntryID    string     `json:"fund_entry_id"`
	Status         string     `json:"status"` // active | returned | refused
	SpentMinor     int64      `json:"spent_minor"`
	ReturnedMinor  int64      `json:"returned_minor"`
	ReturnEntryID  string     `json:"return_entry_id,omitempty"`
	Detail         string     `json:"detail,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

const taskBudgetColumns = `id, workspace_id, agent_id, task, currency, amount_minor, funding, idempotency_key, expires_at, fund_entry_id,
	status, spent_minor, returned_minor, COALESCE(return_entry_id, ''), detail, ended_at, created_at`

func scanTaskBudget(row pgx.Row) (MoneyTaskBudget, error) {
	var b MoneyTaskBudget
	err := row.Scan(&b.ID, &b.WorkspaceID, &b.AgentID, &b.Task, &b.Currency, &b.AmountMinor, &b.Funding, &b.IdempotencyKey,
		&b.ExpiresAt, &b.FundEntryID, &b.Status, &b.SpentMinor, &b.ReturnedMinor, &b.ReturnEntryID, &b.Detail, &b.EndedAt, &b.CreatedAt)
	return b, err
}

// taskBudgetAccounts is workspaceID's open company and agent accounts in currency, "" for one that is not open.
func taskBudgetAccounts(ctx context.Context, q pgxDB, workspaceID, agentID, currency string) (company, agent string, err error) {
	err = q.QueryRow(ctx, `SELECT
		COALESCE((SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $3 AND purpose = 'company' AND status = 'open'), ''),
		COALESCE((SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2 AND currency = $3 AND purpose = 'agent' AND status = 'open'), '')`,
		workspaceID, agentID, currency).Scan(&company, &agent)
	return company, agent, err
}

// heldIn is what account holds in funding.
func heldIn(ctx context.Context, q pgxDB, account, funding string) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		account, funding).Scan(&n)
	return n, err
}

// CreateMoneyTaskBudget sets b's agent a budget of b.AmountMinor in b.Currency for b.Task until b.ExpiresAt, and pays it
// from the company's account into the agent's. The company's and the agent's accounts in the currency must be open,
// and the company's must hold the amount. A retry with the same idempotency key is the budget it set, paid once.
func (s *DualTokenStore) CreateMoneyTaskBudget(ctx context.Context, b MoneyTaskBudget, now time.Time) (MoneyTaskBudget, error) {
	b.Currency = strings.ToUpper(strings.TrimSpace(b.Currency))
	b.IdempotencyKey = strings.TrimSpace(b.IdempotencyKey)
	if b.Funding == "" {
		b.Funding = FundingTest
	}
	task, err := CleanTask(b.Task)
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("%w: %s", ErrMoneyTaskBudget, err.Error())
	}
	b.Task = task
	switch {
	case b.Task == "":
		return MoneyTaskBudget{}, fmt.Errorf("%w: say what the task is", ErrMoneyTaskBudget)
	case b.Currency != CurrencyGBP && b.Currency != CurrencyEUR && b.Currency != CurrencyUSD:
		return MoneyTaskBudget{}, fmt.Errorf("%w: a task budget is in GBP, EUR or USD, not %q", ErrMoneyTaskBudget, b.Currency)
	case b.AmountMinor <= 0:
		return MoneyTaskBudget{}, fmt.Errorf("%w: the amount must be above zero", ErrMoneyTaskBudget)
	case b.Funding != FundingTest && b.Funding != FundingLive:
		return MoneyTaskBudget{}, fmt.Errorf("%w: funding is test or live, not %q", ErrMoneyTaskBudget, b.Funding)
	}
	if b.IdempotencyKey != "" {
		got, err := scanTaskBudget(s.pool.QueryRow(ctx, `SELECT `+taskBudgetColumns+` FROM money_task_budgets
			WHERE workspace_id = $1 AND idempotency_key = $2`, b.WorkspaceID, b.IdempotencyKey))
		if err == nil {
			return got, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return MoneyTaskBudget{}, fmt.Errorf("economy: set task budget: %w", err)
		}
	}
	if !b.ExpiresAt.After(now) {
		return MoneyTaskBudget{}, fmt.Errorf("%w: it must end after now", ErrMoneyTaskBudget)
	}
	if b.Funding == FundingLive {
		c, _ := CapabilityByKey(CapabilityCurrencyAccounts)
		refusal, err := capabilityLive(ctx, s.pool, b.WorkspaceID, c)
		if err != nil {
			return MoneyTaskBudget{}, err
		}
		if refusal != nil {
			return MoneyTaskBudget{}, refusal
		}
	}
	b.ID = "mtb_" + uuid.NewString()
	if b.IdempotencyKey == "" {
		b.IdempotencyKey = b.ID
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: set task budget: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	company, agent, err := taskBudgetAccounts(ctx, tx, b.WorkspaceID, b.AgentID, b.Currency)
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: set task budget: %w", err)
	}
	if company == "" || agent == "" {
		return MoneyTaskBudget{}, fmt.Errorf("%w: open the company's and the agent's %s accounts first", ErrMoneyAccountNotFound, b.Currency)
	}
	// As a payment out does: the balance read below is the one posted against, with no payment between.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{company, agent}); err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: set task budget: %w", err)
	}
	has, err := heldIn(ctx, tx, company, b.Funding)
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: set task budget: %w", err)
	}
	if has < b.AmountMinor {
		return MoneyTaskBudget{}, fmt.Errorf("%w: the company's %s account holds %s, and the budget is %s", ErrMoneyTaskBudgetFunds,
			b.Currency, minorString(has, b.Currency), minorString(b.AmountMinor, b.Currency))
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: b.WorkspaceID, Capability: CapabilityCurrencyAccounts, Kind: "task_budget",
		IdempotencyKey: "task_budget:" + b.IdempotencyKey + ":fund", Funding: b.Funding, Memo: "Task budget: " + b.Task,
		Postings: []MoneyPosting{{AccountID: company, AmountMinor: -b.AmountMinor}, {AccountID: agent, AmountMinor: b.AmountMinor}}}, noPartnerPostings)
	if err != nil {
		return MoneyTaskBudget{}, err
	}
	out, err := scanTaskBudget(tx.QueryRow(ctx, `INSERT INTO money_task_budgets
		(id, workspace_id, agent_id, task, currency, amount_minor, funding, idempotency_key, expires_at, fund_entry_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+taskBudgetColumns,
		b.ID, b.WorkspaceID, b.AgentID, b.Task, b.Currency, b.AmountMinor, b.Funding, b.IdempotencyKey, b.ExpiresAt, e.ID))
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: set task budget: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: set task budget: %w", err)
	}
	return out, nil
}

// MoneyTaskBudgets lists workspaceID's task budgets, or with agentID that agent's, the latest first.
func (s *DualTokenStore) MoneyTaskBudgets(ctx context.Context, workspaceID, agentID string) ([]MoneyTaskBudget, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskBudgetColumns+` FROM money_task_budgets
		WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2) ORDER BY created_at DESC, id`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: task budgets: %w", err)
	}
	defer rows.Close()
	out := []MoneyTaskBudget{}
	for rows.Next() {
		b, err := scanTaskBudget(rows)
		if err != nil {
			return nil, fmt.Errorf("economy: task budgets: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MoneyTaskBudget is workspaceID's task budget id.
func (s *DualTokenStore) MoneyTaskBudget(ctx context.Context, workspaceID, id string) (MoneyTaskBudget, error) {
	b, err := scanTaskBudget(s.pool.QueryRow(ctx, `SELECT `+taskBudgetColumns+` FROM money_task_budgets WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyTaskBudget{}, ErrMoneyTaskBudgetNotFound
	}
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: task budget: %w", err)
	}
	return b, nil
}

// EndMoneyTaskBudget ends workspaceID's task budget id now, before its time, and returns what is left of it; one
// whose return was refused is tried again. One already returned is as it was.
func (s *DualTokenStore) EndMoneyTaskBudget(ctx context.Context, workspaceID, id string, now time.Time) (MoneyTaskBudget, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: end task budget: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := scanTaskBudget(tx.QueryRow(ctx, `SELECT `+taskBudgetColumns+` FROM money_task_budgets WHERE id = $1 AND workspace_id = $2 FOR UPDATE`,
		id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyTaskBudget{}, ErrMoneyTaskBudgetNotFound
	}
	if err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: end task budget: %w", err)
	}
	if b.Status == "returned" {
		return b, nil
	}
	if b, err = s.returnTaskBudget(ctx, tx, b, now); err != nil {
		return MoneyTaskBudget{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MoneyTaskBudget{}, fmt.Errorf("economy: end task budget: %w", err)
	}
	return b, nil
}

// RunMoneyTaskBudgets returns every active task budget whose time has come at now, each in its own transaction with
// its row held, up to 200 a run.
func (s *DualTokenStore) RunMoneyTaskBudgets(ctx context.Context, now time.Time) (returned, refused int, err error) {
	for i := 0; i < 200; i++ {
		status, ok, err := s.runOneTaskBudget(ctx, now)
		if err != nil || !ok {
			return returned, refused, err
		}
		if status == "returned" {
			returned++
		} else {
			refused++
		}
	}
	return returned, refused, nil
}

func (s *DualTokenStore) runOneTaskBudget(ctx context.Context, now time.Time) (status string, ok bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("economy: run task budgets: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := scanTaskBudget(tx.QueryRow(ctx, `SELECT `+taskBudgetColumns+` FROM money_task_budgets
		WHERE status = 'active' AND expires_at <= $1 ORDER BY expires_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("economy: run task budgets: %w", err)
	}
	if b, err = s.returnTaskBudget(ctx, tx, b, now); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("economy: run task budgets: %w", err)
	}
	return b.Status, true, nil
}

// returnTaskBudget pays what is left of b back to the company's account, in a savepoint of tx, and records it on b's
// row, which tx holds. A refusal moves no money and is b's detail; only a failure to reach the database is returned.
func (s *DualTokenStore) returnTaskBudget(ctx context.Context, tx pgx.Tx, b MoneyTaskBudget, now time.Time) (MoneyTaskBudget, error) {
	fail := func(err error) (MoneyTaskBudget, error) {
		return MoneyTaskBudget{}, fmt.Errorf("economy: return task budget: %w", err)
	}
	b.Status, b.Detail, b.SpentMinor, b.ReturnedMinor, b.ReturnEntryID = "returned", "", 0, 0, ""
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = sp.Rollback(ctx) }()
	company, agent, err := taskBudgetAccounts(ctx, sp, b.WorkspaceID, b.AgentID, b.Currency)
	if err != nil {
		return fail(err)
	}
	if company == "" || agent == "" {
		b.Status, b.Detail = "refused", fmt.Sprintf("the company's or the agent's %s account is not open", b.Currency)
	} else {
		if _, err := sp.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{company, agent}); err != nil {
			return fail(err)
		}
		left, err := heldIn(ctx, sp, agent, b.Funding)
		if err != nil {
			return fail(err)
		}
		// What left the agent's account since the budget was funded, less card holds that came back.
		var spent int64
		if err := sp.QueryRow(ctx, `SELECT COALESCE(sum(CASE
				WHEN p.amount_minor < 0 AND e.kind NOT IN ('task_budget_return', 'allowance_sweep') THEN -p.amount_minor
				WHEN p.amount_minor > 0 AND e.kind = 'card_release' THEN -p.amount_minor
				ELSE 0 END), 0)::bigint
			FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
			WHERE p.account_id = $1 AND p.funding = $2 AND e.id <> $3
			  AND e.created_at >= (SELECT created_at FROM money_entries WHERE id = $3)`,
			agent, b.Funding, b.FundEntryID).Scan(&spent); err != nil {
			return fail(err)
		}
		spent = max(0, min(spent, b.AmountMinor))
		back := min(b.AmountMinor-spent, max(left, 0))
		b.SpentMinor = b.AmountMinor - back
		if back > 0 {
			e, err := postMoneyTx(ctx, sp, MoneyEntry{WorkspaceID: b.WorkspaceID, Capability: CapabilityCurrencyAccounts, Kind: "task_budget_return",
				IdempotencyKey: "task_budget:" + b.ID + ":return", Funding: b.Funding, Memo: "Unspent task budget back to the company: " + b.Task,
				Postings: []MoneyPosting{{AccountID: agent, AmountMinor: -back}, {AccountID: company, AmountMinor: back}}}, noPartnerPostings)
			if err != nil {
				b.Status, b.Detail, b.SpentMinor = "refused", err.Error(), 0
			} else {
				b.ReturnedMinor, b.ReturnEntryID = back, e.ID
			}
		}
	}
	if b.Status == "returned" {
		if err := sp.Commit(ctx); err != nil {
			return fail(err)
		}
	} else if err := sp.Rollback(ctx); err != nil {
		return fail(err)
	}
	ended := now
	b.EndedAt = &ended
	if _, err := tx.Exec(ctx, `UPDATE money_task_budgets SET status = $2, spent_minor = $3, returned_minor = $4,
		return_entry_id = NULLIF($5, ''), detail = $6, ended_at = $7 WHERE id = $1`,
		b.ID, b.Status, b.SpentMinor, b.ReturnedMinor, b.ReturnEntryID, b.Detail, ended); err != nil {
		return fail(err)
	}
	return b, nil
}
