package economy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// low_balance_alerts.go — B28.334: ALERT WHEN AN AGENT'S BALANCE FALLS BELOW A THRESHOLD.
//
// The owner sets a threshold for one agent's balance in one currency. CheckLowBalanceAlerts reads each agent's balance
// from the ledger (its open accounts in that currency, test and live money together, as Balance reads one) and records
// an alert while it is below: one for each UTC hour it is found below, however often it is checked in that hour. The
// server checks every minute, and reading the alerts checks first.

// ErrBalanceThresholdNotFound is a threshold that is not the workspace's.
var ErrBalanceThresholdNotFound = errors.New("economy: no such balance threshold")

var thresholdCurrency = regexp.MustCompile(`^([A-Z]{3}|USDC)$`)

// balanceMaxThresholds bounds one workspace's thresholds, and so what every minute's sweep reads for it.
const balanceMaxThresholds = 500

// BalanceThreshold is an owner's threshold for an agent's balance, with what the agent holds now.
type BalanceThreshold struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	Currency       string `json:"currency"`
	ThresholdMinor int64  `json:"threshold_minor"`
	BalanceMinor   int64  `json:"balance_minor"` // what the agent's open accounts in currency hold now
	HasAccount     bool   `json:"has_account"`   // false: the agent has no open account in currency, so nothing is checked
	Below          bool   `json:"below"`
}

// LowBalanceAlert is an hour an agent's balance was found below its threshold.
type LowBalanceAlert struct {
	ID             string    `json:"id"`
	AgentID        string    `json:"agent_id"`
	Currency       string    `json:"currency"`
	HourStart      time.Time `json:"hour_start"`
	BalanceMinor   int64     `json:"balance_minor"`
	ThresholdMinor int64     `json:"threshold_minor"`
	CreatedAt      time.Time `json:"created_at"`
}

// SetBalanceThreshold saves workspaceID's threshold for t's agent and currency, replacing the amount of one it has, and
// answers it with the agent's balance now.
func (s *DualTokenStore) SetBalanceThreshold(ctx context.Context, workspaceID string, t BalanceThreshold, by string) (BalanceThreshold, error) {
	switch {
	case t.AgentID == "":
		return BalanceThreshold{}, fmt.Errorf("%w: agent_id is required", ErrSpending)
	case !thresholdCurrency.MatchString(t.Currency):
		return BalanceThreshold{}, fmt.Errorf("%w: currency must be a currency code such as GBP, or USDC", ErrSpending)
	case t.ThresholdMinor <= 0 || t.ThresholdMinor > 1<<40:
		return BalanceThreshold{}, fmt.Errorf("%w: threshold_minor must be above zero", ErrSpending)
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		t.AgentID, workspaceID).Scan(&ok); err != nil {
		return BalanceThreshold{}, fmt.Errorf("economy: balance threshold: %w", err)
	}
	if !ok {
		return BalanceThreshold{}, fmt.Errorf("%w: agent_id must be one of your agents", ErrSpending)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM balance_thresholds WHERE workspace_id = $1 AND NOT (agent_id = $2 AND currency = $3)`,
		workspaceID, t.AgentID, t.Currency).Scan(&count); err != nil {
		return BalanceThreshold{}, fmt.Errorf("economy: balance threshold: %w", err)
	}
	if count >= balanceMaxThresholds {
		return BalanceThreshold{}, fmt.Errorf("%w: at most %d balance thresholds in a workspace", ErrSpending, balanceMaxThresholds)
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO balance_thresholds (id, workspace_id, agent_id, currency, threshold_minor, set_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (workspace_id, agent_id, currency) DO UPDATE
		SET threshold_minor = EXCLUDED.threshold_minor, set_by = EXCLUDED.set_by, updated_at = now()
		RETURNING id`, "lbt_"+uuid.NewString(), workspaceID, t.AgentID, t.Currency, t.ThresholdMinor, by).Scan(&id)
	if err != nil {
		return BalanceThreshold{}, fmt.Errorf("economy: balance threshold: %w", err)
	}
	all, err := s.BalanceThresholds(ctx, workspaceID)
	if err != nil {
		return BalanceThreshold{}, err
	}
	for _, got := range all {
		if got.ID == id {
			return got, nil
		}
	}
	return BalanceThreshold{}, ErrBalanceThresholdNotFound
}

// DeleteBalanceThreshold removes workspaceID's threshold id; the alerts it raised are kept.
func (s *DualTokenStore) DeleteBalanceThreshold(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM balance_thresholds WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: balance threshold: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBalanceThresholdNotFound
	}
	return nil
}

// BalanceThresholds is workspaceID's thresholds, each with what its agent holds in its currency now.
func (s *DualTokenStore) BalanceThresholds(ctx context.Context, workspaceID string) ([]BalanceThreshold, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.id, t.agent_id, t.currency, t.threshold_minor,
		COALESCE(sum(b.balance_minor), 0)::bigint, count(a.id) > 0
		FROM balance_thresholds t
		LEFT JOIN money_accounts a ON a.workspace_id = t.workspace_id AND a.agent_id = t.agent_id AND a.currency = t.currency
			AND a.purpose = 'agent' AND a.status <> 'closed'
		LEFT JOIN money_account_balances b ON b.account_id = a.id
		WHERE t.workspace_id = $1 GROUP BY t.id ORDER BY t.agent_id, t.currency`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: balance thresholds: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BalanceThreshold, error) {
		var t BalanceThreshold
		err := row.Scan(&t.ID, &t.AgentID, &t.Currency, &t.ThresholdMinor, &t.BalanceMinor, &t.HasAccount)
		t.Below = t.HasAccount && t.BalanceMinor < t.ThresholdMinor
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: balance thresholds: %w", err)
	}
	return out, nil
}

// CheckLowBalanceAlerts records an alert for each of workspaceID's agents whose balance is below its threshold as of
// now and has not been alerted in now's UTC hour, and answers the new ones.
func (s *DualTokenStore) CheckLowBalanceAlerts(ctx context.Context, workspaceID string, now time.Time) ([]LowBalanceAlert, error) {
	thresholds, err := s.BalanceThresholds(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	hour := now.UTC().Truncate(time.Hour)
	out := []LowBalanceAlert{}
	for _, t := range thresholds {
		if !t.Below {
			continue
		}
		a := LowBalanceAlert{AgentID: t.AgentID, Currency: t.Currency, HourStart: hour, BalanceMinor: t.BalanceMinor, ThresholdMinor: t.ThresholdMinor}
		err := s.pool.QueryRow(ctx, `INSERT INTO low_balance_alerts (id, workspace_id, agent_id, currency, hour_start, balance_minor, threshold_minor)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (workspace_id, agent_id, currency, hour_start) DO NOTHING RETURNING id, created_at`,
			"lba_"+uuid.NewString(), workspaceID, a.AgentID, a.Currency, hour, a.BalanceMinor, a.ThresholdMinor).Scan(&a.ID, &a.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) { // alerted already this hour
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("economy: low balance alerts: %w", err)
		}
		out = append(out, a)
	}
	return out, nil
}

// LowBalanceAlerts is workspaceID's low-balance alerts, newest first.
func (s *DualTokenStore) LowBalanceAlerts(ctx context.Context, workspaceID string) ([]LowBalanceAlert, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, currency, hour_start, balance_minor, threshold_minor, created_at
		FROM low_balance_alerts WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: low balance alerts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LowBalanceAlert, error) {
		var a LowBalanceAlert
		err := row.Scan(&a.ID, &a.AgentID, &a.Currency, &a.HourStart, &a.BalanceMinor, &a.ThresholdMinor, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: low balance alerts: %w", err)
	}
	return out, nil
}

// SweepLowBalanceAlerts runs CheckLowBalanceAlerts for every workspace with a balance threshold, and answers how many
// alerts it raised.
func (s *DualTokenStore) SweepLowBalanceAlerts(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT workspace_id FROM balance_thresholds`)
	if err != nil {
		return 0, fmt.Errorf("economy: sweep low balance alerts: %w", err)
	}
	workspaces, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("economy: sweep low balance alerts: %w", err)
	}
	n := 0
	var errs []error
	for _, ws := range workspaces {
		wsCtx, cancel := context.WithTimeout(ctx, 20*time.Second) // one slow workspace does not hold up the rest
		raised, err := s.CheckLowBalanceAlerts(wsCtx, ws, now)
		cancel()
		n += len(raised)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ws, err))
		}
	}
	return n, errors.Join(errs...)
}
