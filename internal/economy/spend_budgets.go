package economy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// spend_budgets.go — B53.4: WEEKLY AND MONTHLY BUDGETS, WITH ALERTS AT 50, 80 AND 100 PERCENT.
//
// A budget is what the owner means to spend in a week (from Monday) or a month, both begun at midnight in the budget's
// own time zone: overall or on one category (B53.3), for their own wallet, one agent's, or every wallet together. Its
// progress is worked out from the postings each time it is read (Spending), never stored.
//
// CheckSpendAlerts records an alert the first time spending reaches 50, 80 and 100% of a budget in its period, of a
// card's cap, and of each of an agent's hourly, daily, weekly and monthly limits (counted as its rules count them). A
// unique key makes each fire once per period; the next period begins another. The server checks every minute.

// Budget periods, and the wallet a budget covering every wallet names.
const (
	BudgetWeek       = "week"
	BudgetMonth      = "month"
	BudgetAllWallets = "all"
)

// Kinds of spend alert.
const (
	SpendAlertBudget     = "budget"
	SpendAlertCard       = "card"
	SpendAlertAgentLimit = "agent_limit"
)

// SpendAlertPercents are the points of a budget or limit that raise an alert.
var SpendAlertPercents = []int{50, 80, 100}

// ErrBudgetNotFound is a budget that is not the workspace's.
var ErrBudgetNotFound = errors.New("economy: no such budget")

var budgetCurrency = regexp.MustCompile(`^[A-Z]{3,4}$`)

// spendMaxBudgets bounds one workspace's budgets, and so the reads every minute's sweep makes for it.
const spendMaxBudgets = 100

// SpendBudget is one budget and how far its current period has gone.
type SpendBudget struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"` // empty: the owner's own wallet; "all": every wallet
	Category    string    `json:"category"` // empty: all spending
	Period      string    `json:"period"`   // week or month
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	Timezone    string    `json:"timezone"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	SpentMinor  int64     `json:"spent_minor"`
	LeftMinor   int64     `json:"left_minor"` // below zero once it is over
	Percent     int64     `json:"percent"`    // of the budget spent, rounded down
}

// SpendAlert is spending reaching a point of a budget, a card's cap or an agent's limit.
type SpendAlert struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`    // budget, card or agent_limit
	Subject     string    `json:"subject"` // the budget's id, the card's or the agent's
	AgentID     string    `json:"agent_id,omitempty"`
	Category    string    `json:"category,omitempty"`
	Period      string    `json:"period,omitempty"` // week or month; an agent's hourly, daily, weekly or monthly limit
	PeriodStart time.Time `json:"period_start"`
	Percent     int       `json:"percent"`
	SpentMinor  int64     `json:"spent_minor"`
	LimitMinor  int64     `json:"limit_minor"`
	Currency    string    `json:"currency"` // LXC for an agent's limit, counted in µLXC
	CreatedAt   time.Time `json:"created_at"`
}

// budgetPeriod is the period of kind week or month that now falls in, begun at midnight in loc: a week from Monday.
func budgetPeriod(period string, loc *time.Location, now time.Time) (time.Time, time.Time) {
	n := now.In(loc)
	if period == BudgetWeek {
		start := time.Date(n.Year(), n.Month(), n.Day()-(int(n.Weekday())+6)%7, 0, 0, 0, 0, loc)
		return start, start.AddDate(0, 0, 7)
	}
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 1, 0)
}

// SetSpendBudget saves workspaceID's budget for b's wallet, category, period and currency (one of each), replacing
// its amount and time zone if it has one, and answers it with its progress as of now.
func (s *DualTokenStore) SetSpendBudget(ctx context.Context, workspaceID string, b SpendBudget, by string, now time.Time) (SpendBudget, error) {
	if b.Timezone == "" {
		b.Timezone = "UTC"
	}
	switch {
	case b.Period != BudgetWeek && b.Period != BudgetMonth:
		return SpendBudget{}, fmt.Errorf("%w: period must be week or month", ErrSpending)
	case b.Category != "" && !slices.Contains(SpendCategories, b.Category):
		return SpendBudget{}, fmt.Errorf("%w: category must be empty (all spending) or one of %s", ErrSpending, strings.Join(SpendCategories, ", "))
	case b.AmountMinor <= 0 || b.AmountMinor > 1<<40:
		return SpendBudget{}, fmt.Errorf("%w: amount_minor must be above zero", ErrSpending)
	case !budgetCurrency.MatchString(b.Currency):
		return SpendBudget{}, fmt.Errorf("%w: currency must be a currency code such as GBP", ErrSpending)
	}
	if _, err := time.LoadLocation(b.Timezone); err != nil {
		return SpendBudget{}, fmt.Errorf("%w: timezone %q is not an IANA time zone", ErrSpending, b.Timezone)
	}
	if b.AgentID != "" && b.AgentID != BudgetAllWallets {
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
			b.AgentID, workspaceID).Scan(&ok); err != nil {
			return SpendBudget{}, fmt.Errorf("economy: budget: %w", err)
		}
		if !ok {
			return SpendBudget{}, fmt.Errorf("%w: agent_id must be empty (your own wallet), %q or one of your agents", ErrSpending, BudgetAllWallets)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM spend_budgets WHERE workspace_id = $1 AND NOT (agent_id = $2 AND category = $3
		AND period = $4 AND currency = $5)`, workspaceID, b.AgentID, b.Category, b.Period, b.Currency).Scan(&count); err != nil {
		return SpendBudget{}, fmt.Errorf("economy: budget: %w", err)
	}
	if count >= spendMaxBudgets {
		return SpendBudget{}, fmt.Errorf("%w: at most %d budgets in a workspace", ErrSpending, spendMaxBudgets)
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO spend_budgets (id, workspace_id, agent_id, category, period, amount_minor, currency, timezone, set_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (workspace_id, agent_id, category, period, currency) DO UPDATE
		SET amount_minor = EXCLUDED.amount_minor, timezone = EXCLUDED.timezone, set_by = EXCLUDED.set_by, updated_at = now()
		RETURNING id`, "bdg_"+uuid.NewString(), workspaceID, b.AgentID, b.Category, b.Period, b.AmountMinor, b.Currency, b.Timezone, by).Scan(&b.ID)
	if err != nil {
		return SpendBudget{}, fmt.Errorf("economy: budget: %w", err)
	}
	all, err := s.SpendBudgets(ctx, workspaceID, now)
	if err != nil {
		return SpendBudget{}, err
	}
	for _, got := range all {
		if got.ID == b.ID {
			return got, nil
		}
	}
	return SpendBudget{}, ErrBudgetNotFound
}

// DeleteSpendBudget removes workspaceID's budget id; the alerts it raised are kept.
func (s *DualTokenStore) DeleteSpendBudget(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM spend_budgets WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: budget: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBudgetNotFound
	}
	return nil
}

// SpendBudgets is workspaceID's budgets, each with its period as of now and what has been spent in it.
func (s *DualTokenStore) SpendBudgets(ctx context.Context, workspaceID string, now time.Time) ([]SpendBudget, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, category, period, amount_minor, currency, timezone FROM spend_budgets
		WHERE workspace_id = $1 ORDER BY agent_id, period, category, currency`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: budgets: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SpendBudget, error) {
		var b SpendBudget
		err := row.Scan(&b.ID, &b.AgentID, &b.Category, &b.Period, &b.AmountMinor, &b.Currency, &b.Timezone)
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: budgets: %w", err)
	}
	periods := map[[2]time.Time][]SpendPayment{} // one read of the payments per distinct period
	for i := range out {
		b := &out[i]
		loc, err := time.LoadLocation(b.Timezone)
		if err != nil {
			loc = time.UTC
		}
		b.PeriodStart, b.PeriodEnd = budgetPeriod(b.Period, loc, now)
		key := [2]time.Time{b.PeriodStart.UTC(), b.PeriodEnd.UTC()}
		payments, ok := periods[key]
		if !ok {
			sp, err := s.Spending(ctx, workspaceID, key[0], key[1])
			if err != nil {
				return nil, err
			}
			payments, periods[key] = sp.Payments, sp.Payments
		}
		for _, p := range payments {
			if p.Currency == b.Currency && (b.AgentID == BudgetAllWallets || p.AgentID == b.AgentID) && (b.Category == "" || p.Category == b.Category) {
				b.SpentMinor += p.AmountMinor
			}
		}
		b.LeftMinor = b.AmountMinor - b.SpentMinor
		b.Percent = b.SpentMinor * 100 / b.AmountMinor
	}
	return out, nil
}

// CheckSpendAlerts records the alerts workspaceID's spending has reached as of now and not yet raised in their period,
// and answers the new ones.
func (s *DualTokenStore) CheckSpendAlerts(ctx context.Context, workspaceID string, now time.Time) ([]SpendAlert, error) {
	var reached []SpendAlert
	budgets, err := s.SpendBudgets(ctx, workspaceID, now)
	if err != nil {
		return nil, err
	}
	for _, b := range budgets {
		reached = append(reached, SpendAlert{Kind: SpendAlertBudget, Subject: b.ID, AgentID: b.AgentID, Category: b.Category, Period: b.Period,
			PeriodStart: b.PeriodStart, SpentMinor: b.SpentMinor, LimitMinor: b.AmountMinor, Currency: b.Currency})
	}
	// A card's period is its life: it has one cap.
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, created_at, spent_minor, cap_minor, currency FROM money_cards
		WHERE workspace_id = $1 AND spent_minor > 0 AND updated_at >= $2`, workspaceID, now.AddDate(0, 0, -40))
	if err != nil {
		return nil, fmt.Errorf("economy: spend alerts: %w", err)
	}
	cards, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SpendAlert, error) {
		a := SpendAlert{Kind: SpendAlertCard}
		err := row.Scan(&a.Subject, &a.AgentID, &a.PeriodStart, &a.SpentMinor, &a.LimitMinor, &a.Currency)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: spend alerts: %w", err)
	}
	reached = append(reached, cards...)
	limits, err := s.agentLimitProgress(ctx, workspaceID, now)
	if err != nil {
		return nil, err
	}
	reached = append(reached, limits...)

	out := []SpendAlert{}
	for _, a := range reached {
		for _, pct := range SpendAlertPercents {
			if a.LimitMinor <= 0 || a.SpentMinor*100 < a.LimitMinor*int64(pct) {
				break
			}
			got := a
			got.Percent = pct
			err := s.pool.QueryRow(ctx, `INSERT INTO spend_alerts (id, workspace_id, kind, subject, agent_id, category, period, period_start,
					percent, spent_minor, limit_minor, currency) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
				ON CONFLICT (workspace_id, kind, subject, period, period_start, percent) DO NOTHING RETURNING id, created_at`,
				"sal_"+uuid.NewString(), workspaceID, a.Kind, a.Subject, a.AgentID, a.Category, a.Period, a.PeriodStart, pct,
				a.SpentMinor, a.LimitMinor, a.Currency).Scan(&got.ID, &got.CreatedAt)
			if errors.Is(err, pgx.ErrNoRows) { // raised already in this period
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("economy: spend alerts: %w", err)
			}
			out = append(out, got)
		}
	}
	return out, nil
}

// agentLimitProgress is, for each of workspaceID's agents' hourly, daily, weekly and monthly limits, what it has spent
// in that limit's period as of now, as its rules count it.
func (s *DualTokenStore) agentLimitProgress(ctx context.Context, workspaceID string, now time.Time) ([]SpendAlert, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id FROM agent_rules WHERE workspace_id = $1 AND (daily_limit_ulxc > 0 OR monthly_limit_ulxc > 0
		OR hourly_limit_ulxc > 0 OR weekly_limit_ulxc > 0) ORDER BY agent_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: agent limit alerts: %w", err)
	}
	agents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("economy: agent limit alerts: %w", err)
	}
	out := []SpendAlert{}
	for _, agentID := range agents {
		r, err := s.GetAgentRules(ctx, workspaceID, agentID)
		if err != nil {
			return nil, err
		}
		loc, err := r.location()
		if err != nil {
			loc = time.UTC
		}
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			for _, limit := range r.periodLimits(now.In(loc), loc) {
				if limit.ulxc <= 0 {
					continue
				}
				spent, err := agentSpentSince(ctx, tx, workspaceID, agentID, limit.since)
				if err != nil {
					return err
				}
				out = append(out, SpendAlert{Kind: SpendAlertAgentLimit, Subject: agentID, AgentID: agentID, Period: limit.name,
					PeriodStart: limit.since, SpentMinor: spent, LimitMinor: limit.ulxc, Currency: "LXC"})
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("economy: agent limit alerts: %w", err)
		}
	}
	return out, nil
}

// SpendAlerts is workspaceID's alerts, newest first.
func (s *DualTokenStore) SpendAlerts(ctx context.Context, workspaceID string) ([]SpendAlert, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, subject, agent_id, category, period, period_start, percent, spent_minor, limit_minor,
		currency, created_at FROM spend_alerts WHERE workspace_id = $1 ORDER BY created_at DESC, percent DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: spend alerts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SpendAlert, error) {
		var a SpendAlert
		err := row.Scan(&a.ID, &a.Kind, &a.Subject, &a.AgentID, &a.Category, &a.Period, &a.PeriodStart, &a.Percent, &a.SpentMinor,
			&a.LimitMinor, &a.Currency, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: spend alerts: %w", err)
	}
	return out, nil
}

// SweepSpendAlerts runs CheckSpendAlerts for every workspace that has a budget, a card spent on lately or an agent
// with a spending limit, and answers how many alerts it raised.
// shortcut: every such workspace is checked each sweep; check only those with new postings when that grows slow.
func (s *DualTokenStore) SweepSpendAlerts(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id FROM spend_budgets
		UNION SELECT workspace_id FROM money_cards WHERE spent_minor > 0 AND updated_at >= $1
		UNION SELECT workspace_id FROM agent_rules WHERE daily_limit_ulxc > 0 OR monthly_limit_ulxc > 0 OR hourly_limit_ulxc > 0 OR weekly_limit_ulxc > 0`,
		now.AddDate(0, 0, -40))
	if err != nil {
		return 0, fmt.Errorf("economy: sweep spend alerts: %w", err)
	}
	workspaces, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("economy: sweep spend alerts: %w", err)
	}
	n := 0
	var errs []error
	for _, ws := range workspaces {
		wsCtx, cancel := context.WithTimeout(ctx, 20*time.Second) // one slow workspace does not hold up the rest
		raised, err := s.CheckSpendAlerts(wsCtx, ws, now)
		cancel()
		n += len(raised)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ws, err))
		}
	}
	return n, errors.Join(errs...)
}
