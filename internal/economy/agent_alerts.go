package economy

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/passkey"
)

// agent_alerts.go — B19.6: UNUSUAL SPEND IS FLAGGED, AND THE MONTH-END IS FORECAST.
//
// THE RULE. Every hold, debit or payment an agent makes is watched under its row lock, after its rules
// let it through (enforceAgentRules): when what the agent has spent in the last hour, this movement
// included, reaches UnusualSpendMultiple times its usual hourly rate — what it spent in the seven days
// before that hour, over 168 — an alert is raised. An agent with nothing spent in those seven days has
// no pattern yet and raises none, and an agent raises at most one alert an hour. If its rules say
// pause_on_unusual_spend, the alert also pauses it: the movement that raised it goes through, and every
// one after it is refused before the provider is called until the workspace's owner resumes the agent.
// "Spent" is what the spending rules count: spends, holds and their settles and releases, and payments
// out to other agents.
//
// THE FORECAST. An agent's month-end spend is its spend so far this month (UTC) run on at the same rate
// to the end of the month: spent × month length ÷ time elapsed. The workspace's is the same over all its
// agents together.

// UnusualSpendMultiple is how many times its usual hourly rate an agent spends in an hour to raise an alert.
const UnusualSpendMultiple = 5

// UnusualSpendRule is the rule, as the alerts route states it.
const UnusualSpendRule = "An alert is raised when an agent's spend in the last hour reaches 5 times its usual hourly rate " +
	"(what it spent in the 7 days before that hour, over 168). An agent with no spend in those 7 days raises none, " +
	"and an agent raises at most one alert an hour. With pause_on_unusual_spend in its rules, the alert also pauses it."

const (
	usualSpendHours = 7 * 24
	// What the spending rules count as an agent's spend (agent_rules.go spentSince).
	agentSpendPostings = `(kind IN ('spend', 'hold', 'settle', 'release', 'card', 'platform_fee') OR (kind = 'pay' AND amount_ulxc < 0))`
)

// AgentSpendAlert is one alert raised by an agent's unusual spend.
type AgentSpendAlert struct {
	ID               string    `json:"id"`
	AgentID          string    `json:"agent_id"`
	LastHourULXC     int64     `json:"last_hour_ulxc"`      // spent in the hour, with the movement that raised it
	UsualPerHourULXC int64     `json:"usual_per_hour_ulxc"` // the seven days before that hour, over 168
	Paused           bool      `json:"paused"`              // the alert paused the agent
	CreatedAt        time.Time `json:"created_at"`
}

// refuseIfPaused refuses any movement of an agent that is archived (B28.298) or paused, on its own or with
// every agent in its workspace (B19.7).
func refuseIfPaused(ctx context.Context, tx pgx.Tx, agentID string) error {
	var pausedAt, allPausedAt, archivedAt *time.Time
	var reason, allReason string
	if err := tx.QueryRow(ctx, `SELECT a.paused_at, a.paused_reason, w.paused_at, COALESCE(w.reason, ''), a.archived_at
		FROM agent_accounts a LEFT JOIN agent_workspace_pauses w ON w.workspace_id = a.workspace_id WHERE a.id = $1`,
		agentID).Scan(&pausedAt, &reason, &allPausedAt, &allReason, &archivedAt); err != nil {
		return fmt.Errorf("economy: agent paused: %w", err)
	}
	if archivedAt != nil {
		return ruleRefusal("the agent is archived")
	}
	if allPausedAt != nil {
		if allReason == "" {
			allReason = "paused by the workspace's owner"
		}
		return ruleRefusal("every agent in this workspace is paused (%s) — the workspace's owner can resume them", allReason)
	}
	if pausedAt == nil {
		return nil
	}
	if reason == "" {
		reason = "paused by the workspace's owner"
	}
	return ruleRefusal("the agent is paused (%s) — the workspace's owner can resume it", reason)
}

// watchAgentSpend applies the unusual-spend rule to a movement of amount µLXC the agent's rules have let
// through, inside its transaction.
func watchAgentSpend(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, amount int64, now time.Time, pauseOn bool) error {
	hourAgo := now.Add(-time.Hour)
	var lastHour, usualWeek int64
	if err := tx.QueryRow(ctx, `SELECT
		  COALESCE(-sum(amount_ulxc) FILTER (WHERE created_at >= $3), 0)::bigint,
		  COALESCE(-sum(amount_ulxc) FILTER (WHERE created_at < $3), 0)::bigint
		FROM agent_postings WHERE workspace_id = $1 AND account = $2 AND created_at >= $4 AND `+agentSpendPostings,
		workspaceID, agentAccount(agentID), hourAgo, hourAgo.Add(-usualSpendHours*time.Hour)).Scan(&lastHour, &usualWeek); err != nil {
		return fmt.Errorf("economy: agent spend pattern: %w", err)
	}
	lastHour += amount
	if usualWeek <= 0 || lastHour*usualSpendHours < UnusualSpendMultiple*usualWeek {
		return nil
	}
	var raised bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_spend_alerts WHERE agent_id = $1 AND created_at >= $2)`,
		agentID, hourAgo).Scan(&raised); err != nil {
		return fmt.Errorf("economy: agent alerts: %w", err)
	}
	if raised {
		return nil
	}
	usual := usualWeek / usualSpendHours
	if _, err := tx.Exec(ctx, `INSERT INTO agent_spend_alerts (id, workspace_id, agent_id, last_hour_ulxc, usual_per_hour_ulxc, paused)
		VALUES ($1, $2, $3, $4, $5, $6)`, "alr_"+uuid.NewString(), workspaceID, agentID, lastHour, usual, pauseOn); err != nil {
		return fmt.Errorf("economy: raise agent alert: %w", err)
	}
	if pauseOn {
		reason := fmt.Sprintf("unusual spend: %s LXC in an hour, against a usual %s LXC an hour", lxcString(lastHour), lxcString(usual))
		if _, err := tx.Exec(ctx, `UPDATE agent_accounts SET paused_at = now(), paused_reason = $2 WHERE id = $1 AND paused_at IS NULL`,
			agentID, reason); err != nil {
			return fmt.Errorf("economy: pause agent: %w", err)
		}
		return revokeAgentKYA(ctx, tx, agentID, KYARevokedFrozen) // B30.5
	}
	return nil
}

// PauseAgent pauses an agent: every movement it makes is refused until ResumeAgent, and its Know Your Agent
// credential is revoked (B30.5).
func (s *DualTokenStore) PauseAgent(ctx context.Context, workspaceID, agentID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: pause agent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE agent_accounts SET paused_at = COALESCE(paused_at, now()), paused_reason = $3
		WHERE id = $1 AND workspace_id = $2`, agentID, workspaceID, reason)
	if err != nil {
		return fmt.Errorf("economy: pause agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentNotFound
	}
	if err := revokeAgentKYA(ctx, tx, agentID, KYARevokedFrozen); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PauseAllAgents pauses every agent in a workspace, those it creates later included, until ResumeAllAgents, and
// revokes each one's Know Your Agent credential (B30.5).
func (s *DualTokenStore) PauseAllAgents(ctx context.Context, workspaceID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: pause every agent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_workspace_pauses (workspace_id, reason) VALUES ($1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET reason = EXCLUDED.reason`, workspaceID, reason); err != nil {
		return fmt.Errorf("economy: pause every agent: %w", err)
	}
	if err := revokeWorkspaceKYA(ctx, tx, workspaceID, KYARevokedFrozen); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResumeAllAgents lifts PauseAllAgents. An agent paused on its own stays paused.
func (s *DualTokenStore) ResumeAllAgents(ctx context.Context, workspaceID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM agent_workspace_pauses WHERE workspace_id = $1`, workspaceID); err != nil {
		return fmt.Errorf("economy: resume every agent: %w", err)
	}
	return nil
}

// ResumeAgent lets a paused agent move money again.
func (s *DualTokenStore) ResumeAgent(ctx context.Context, workspaceID, agentID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_accounts SET paused_at = NULL, paused_reason = '' WHERE id = $1 AND workspace_id = $2`,
		agentID, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: resume agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// B41.108 — stopping is instant, restarting is signed. Pausing takes nothing but the owner's session; once the
// workspace has a passkey or device key, resuming an agent or the whole fleet also takes an assertion by one of
// them over a single-use challenge naming what is resumed (resumePurpose), as approvals do (AuthorizeApprovalDecision).

// resumePurpose names the challenge resuming agentID is signed over; "" is every agent (ResumeAllAgents).
func resumePurpose(agentID string) string {
	if agentID == "" {
		return "resume-all"
	}
	return "resume:" + agentID
}

// ResumeChallenge issues the challenge resuming agentID ("" : every agent) is signed over, and the workspace's
// passkeys that may sign it.
func (s *DualTokenStore) ResumeChallenge(ctx context.Context, workspaceID, agentID string) (challenge string, credentialIDs []string, err error) {
	if agentID != "" {
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
			agentID, workspaceID).Scan(&ok); err != nil {
			return "", nil, fmt.Errorf("economy: resume agent: %w", err)
		}
		if !ok {
			return "", nil, ErrAgentNotFound
		}
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, resumePurpose(agentID))
	return c, credentialIDs, err
}

// AuthorizeResume lets resuming agentID ("" : every agent) through: at once for a workspace with no passkey or device
// key, and otherwise only with a's assertion over ResumeChallenge's challenge, which it spends.
func (s *DualTokenStore) AuthorizeResume(ctx context.Context, workspaceID, agentID string, a *passkey.Assertion) error {
	return s.authorizeSigned(ctx, workspaceID, resumePurpose(agentID), a)
}

// ListAgentSpendAlerts reads a workspace's alerts, newest first.
func (s *DualTokenStore) ListAgentSpendAlerts(ctx context.Context, workspaceID string) ([]AgentSpendAlert, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, last_hour_ulxc, usual_per_hour_ulxc, paused, created_at
		FROM agent_spend_alerts WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: agent alerts: %w", err)
	}
	defer rows.Close()
	out := []AgentSpendAlert{}
	for rows.Next() {
		var a AgentSpendAlert
		if err := rows.Scan(&a.ID, &a.AgentID, &a.LastHourULXC, &a.UsualPerHourULXC, &a.Paused, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentForecast is one agent's spend this month and where it is heading.
type AgentForecast struct {
	AgentID      string `json:"agent_id"`
	Name         string `json:"name"`
	SpentULXC    int64  `json:"spent_ulxc"`
	ForecastULXC int64  `json:"forecast_ulxc"`
}

// SpendForecast is a workspace's month-end forecast, as of At.
type SpendForecast struct {
	At           time.Time       `json:"at"`
	MonthStart   time.Time       `json:"month_start"`
	MonthEnd     time.Time       `json:"month_end"`
	SpentULXC    int64           `json:"spent_ulxc"`    // every agent, this month to At
	ForecastULXC int64           `json:"forecast_ulxc"` // every agent, to the end of the month
	Agents       []AgentForecast `json:"agents"`
}

// runOn is spent over elapsed of the month, run on to its whole length: spent × length ÷ elapsed.
func runOn(spent int64, elapsed, length time.Duration) int64 {
	if elapsed <= 0 || spent <= 0 {
		return max(spent, 0)
	}
	v := new(big.Int).Mul(big.NewInt(spent), big.NewInt(int64(length)))
	return v.Quo(v, big.NewInt(int64(elapsed))).Int64()
}

// AgentSpendForecast forecasts each of a workspace's agents' month-end spend, and the workspace's, from
// the month so far as of at.
func (s *DualTokenStore) AgentSpendForecast(ctx context.Context, workspaceID string, at time.Time) (SpendForecast, error) {
	at = at.UTC()
	start := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	f := SpendForecast{At: at, MonthStart: start, MonthEnd: start.AddDate(0, 1, 0), Agents: []AgentForecast{}}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, COALESCE((SELECT -sum(p.amount_ulxc) FROM agent_postings p
		         WHERE p.workspace_id = a.workspace_id AND p.account = 'agent:' || a.id
		           AND p.created_at >= $2 AND p.created_at < $3 AND `+agentSpendPostings+`), 0)::bigint
		  FROM agent_accounts a WHERE a.workspace_id = $1 ORDER BY a.created_at, a.id`, workspaceID, start, at)
	if err != nil {
		return f, fmt.Errorf("economy: forecast: %w", err)
	}
	defer rows.Close()
	elapsed, length := at.Sub(start), f.MonthEnd.Sub(start)
	for rows.Next() {
		var a AgentForecast
		if err := rows.Scan(&a.AgentID, &a.Name, &a.SpentULXC); err != nil {
			return f, err
		}
		a.ForecastULXC = runOn(a.SpentULXC, elapsed, length)
		f.SpentULXC += a.SpentULXC
		f.Agents = append(f.Agents, a)
	}
	if err := rows.Err(); err != nil {
		return f, fmt.Errorf("economy: forecast: %w", err)
	}
	f.ForecastULXC = runOn(f.SpentULXC, elapsed, length)
	return f, nil
}
