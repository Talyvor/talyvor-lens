package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// agent_rule_boosts.go — B28.308: A TEMPORARY LIMIT BOOST THAT REVERTS ITSELF.
//
// A boost (migration 0193) raises one of an agent's limits to a value until a time. agentRulesInForce reads the
// rules with the boosts in force at the request's time laid over them wherever the rules are judged — the hold and
// the debit (enforceAgentRules), the settle's allowance (agentAllowance) and so the simulator — and from until on
// the boost is simply not read. Nothing runs at expiry, and agent_rules and its versions (B28.307) are never touched.
// A boost raises the limit it was set over: once the rules change that limit, it no longer applies.

// ErrBoostNotFound: the agent has no boost in force on that limit.
var ErrBoostNotFound = errors.New("economy: the agent has no boost in force on that limit")

// AgentRuleBoost is one of an agent's limits raised until a time.
type AgentRuleBoost struct {
	Rule       string    `json:"rule"`        // the limit, as AgentRules names it: daily_limit_ulxc, requests_per_minute, …
	RaisedFrom int64     `json:"raised_from"` // the limit as the rules set it when the boost was set
	Value      int64     `json:"value"`       // what it is raised to: µLXC, or requests a minute
	Until      time.Time `json:"until"`
	CreatedBy  string    `json:"created_by"` // the credential that set it, as a rules version names it; empty when unknown
	CreatedAt  time.Time `json:"created_at"`
}

// boostableRules are the limits a boost may raise, each with where AgentRules keeps it.
var boostableRules = map[string]func(*AgentRules) *int64{
	"max_per_request_ulxc": func(r *AgentRules) *int64 { return &r.MaxPerRequestULXC },
	"hourly_limit_ulxc":    func(r *AgentRules) *int64 { return r.HourlyLimitULXC },
	"daily_limit_ulxc":     func(r *AgentRules) *int64 { return &r.DailyLimitULXC },
	"weekly_limit_ulxc":    func(r *AgentRules) *int64 { return r.WeeklyLimitULXC },
	"monthly_limit_ulxc":   func(r *AgentRules) *int64 { return &r.MonthlyLimitULXC },
	"approval_above_ulxc":  func(r *AgentRules) *int64 { return &r.ApprovalAboveULXC },
	"requests_per_minute":  func(r *AgentRules) *int64 { return r.RequestsPerMinute },
}

// BoostableRules are the names of the limits a boost may raise, sorted.
func BoostableRules() []string {
	names := make([]string, 0, len(boostableRules))
	for name := range boostableRules {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// applyBoosts raises r's limits to the boosts — each only while the rules still set its limit where it was when
// the boost was set. A limit the rules have since changed, up or down, is the rules'.
func (r *AgentRules) applyBoosts(boosts []AgentRuleBoost) {
	for _, b := range boosts {
		field, ok := boostableRules[b.Rule]
		if !ok {
			continue
		}
		if v := field(r); v != nil && *v == b.RaisedFrom && b.Value > *v {
			*v = b.Value
		}
	}
}

// agentRulesInForce reads an agent's rules with the boosts in force at `at` laid over them; pgx.ErrNoRows when it
// has no rules, and so no limit to raise.
func agentRulesInForce(ctx context.Context, tx pgx.Tx, agentID string, at time.Time) (AgentRules, error) {
	r, err := scanAgentRules(tx.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if err != nil {
		return r, err
	}
	boosts, err := agentBoosts(ctx, tx, agentID, at)
	if err != nil {
		return r, err
	}
	r.applyBoosts(boosts)
	return r, nil
}

// agentBoosts are the agent's boosts in force at `at`, the soonest to end first.
func agentBoosts(ctx context.Context, q pgxDB, agentID string, at time.Time) ([]AgentRuleBoost, error) {
	rows, err := q.Query(ctx, `SELECT rule, raised_from, value, until, created_by, created_at FROM agent_rule_boosts
		WHERE agent_id = $1 AND until > $2 ORDER BY until, rule`, agentID, at)
	if err != nil {
		return nil, fmt.Errorf("economy: agent boosts: %w", err)
	}
	defer rows.Close()
	out := []AgentRuleBoost{}
	for rows.Next() {
		var b AgentRuleBoost
		if err := rows.Scan(&b.Rule, &b.RaisedFrom, &b.Value, &b.Until, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("economy: agent boosts: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ruleAmount is v as the limit rule counts it: LXC, or requests a minute.
func ruleAmount(rule string, v int64) string {
	if rule == "requests_per_minute" {
		return strconv.FormatInt(v, 10) + " requests a minute"
	}
	return lxcString(v) + " LXC"
}

// AgentBoosts lists the agent's boosts in force now, the soonest to end first.
func (s *DualTokenStore) AgentBoosts(ctx context.Context, workspaceID, agentID string) ([]AgentRuleBoost, error) {
	if _, err := s.GetAgentRules(ctx, workspaceID, agentID); err != nil {
		return nil, err
	}
	return agentBoosts(ctx, s.pool, agentID, time.Now())
}

// BoostAgentRule raises one of the agent's limits to b.Value until b.Until, replacing the boost that limit had. The
// limit must be set, and the boost above it: a boost raises a limit, it does not make one, and only while the rules
// leave that limit as it is now. Who sets it is who ctx names (WithRulesChange).
func (s *DualTokenStore) BoostAgentRule(ctx context.Context, workspaceID, agentID string, b AgentRuleBoost) (AgentRuleBoost, error) {
	return s.boostAgentRule(ctx, s.pool, workspaceID, agentID, b)
}

// boostAgentRule is BoostAgentRule through q, so a signed boost is judged and saved in the transaction spending its
// signature (B54.29).
func (s *DualTokenStore) boostAgentRule(ctx context.Context, q pgxDB, workspaceID, agentID string, b AgentRuleBoost) (AgentRuleBoost, error) {
	field, ok := boostableRules[b.Rule]
	if !ok {
		return b, fmt.Errorf("%w: %q is not a limit a boost can raise; it may raise %s", ErrAgentRule, b.Rule, strings.Join(BoostableRules(), ", "))
	}
	if b.Value <= 0 {
		return b, fmt.Errorf("%w: a boost needs the value the limit is raised to", ErrAgentRule)
	}
	if !b.Until.After(time.Now()) {
		return b, fmt.Errorf("%w: a boost needs a time in the future to last until", ErrAgentRule)
	}
	r, err := getAgentRules(ctx, q, workspaceID, agentID)
	if err != nil {
		return b, err
	}
	name := strings.ReplaceAll(strings.TrimSuffix(b.Rule, "_ulxc"), "_", " ")
	switch current := *field(&r); {
	case current == 0:
		return b, fmt.Errorf("%w: the agent has no %s to raise; set it in the rules first", ErrAgentRule, name)
	case b.Value <= current:
		return b, fmt.Errorf("%w: the agent's %s is %s, so a boost must raise it above that", ErrAgentRule, name, ruleAmount(b.Rule, current))
	}
	// B32.32: a room wallet's monthly limit is not raised past what the room owner's plan allows, even for a while.
	if b.Rule == "monthly_limit_ulxc" {
		if err := s.checkRoomBudget(ctx, q, workspaceID, agentID, b.Value); err != nil {
			return b, err
		}
	}
	b.RaisedFrom, b.CreatedBy = *field(&r), rulesChangeFrom(ctx).by
	err = q.QueryRow(ctx, `
		INSERT INTO agent_rule_boosts (agent_id, workspace_id, rule, raised_from, value, until, created_by)
		SELECT id, workspace_id, $3, $4, $5, $6, $7 FROM agent_accounts WHERE id = $1 AND workspace_id = $2
		ON CONFLICT (agent_id, rule) DO UPDATE SET raised_from = EXCLUDED.raised_from, value = EXCLUDED.value,
		       until = EXCLUDED.until, created_by = EXCLUDED.created_by, created_at = now()
		RETURNING until, created_at`, agentID, workspaceID, b.Rule, b.RaisedFrom, b.Value, b.Until, b.CreatedBy).Scan(&b.Until, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrAgentNotFound
	}
	if err != nil {
		return b, fmt.Errorf("economy: boost agent rule: %w", err)
	}
	return b, revokeAgentKYA(ctx, q, agentID, KYARevokedRules) // B30.5: its credential states the limit
}

// EndAgentBoost ends the agent's boost on rule now, before its time: the limit is the rules' again.
func (s *DualTokenStore) EndAgentBoost(ctx context.Context, workspaceID, agentID, rule string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM agent_rule_boosts WHERE agent_id = $1 AND workspace_id = $2 AND rule = $3 AND until > $4`,
		agentID, workspaceID, rule, time.Now())
	if err != nil {
		return fmt.Errorf("economy: end agent boost: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return revokeAgentKYA(ctx, s.pool, agentID, KYARevokedRules) // B30.5: its credential states the limit
	}
	if _, err := s.GetAgentRules(ctx, workspaceID, agentID); err != nil {
		return err
	}
	return ErrBoostNotFound
}
