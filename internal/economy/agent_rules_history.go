package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// agent_rules_history.go — B28.307: EVERY CHANGE TO AN AGENT'S RULES IS A VERSION, AND THE RULES CAN BE ROLLED BACK
// TO ANY OF THEM.
//
// SetAgentRules and RollbackAgentRules record a version (migration 0192) in the transaction that changes the rules:
// the whole agent_rules row as the change left it, who changed it — the credential the change rides ctx with
// (WithRulesChange) — and how. A save that leaves the rules as they were is not a new version.

// ErrRulesVersionNotFound: the agent has no such version of its rules.
var ErrRulesVersionNotFound = errors.New("economy: the agent has no such version of its rules")

type rulesChangeKey struct{}

type rulesChange struct{ by, what string }

// WithRulesChange names who changes an agent's rules with ctx, and how ("set", "template <id>"): the version the
// change makes records both. A change without them records nobody, as "set".
func WithRulesChange(ctx context.Context, by, what string) context.Context {
	return context.WithValue(ctx, rulesChangeKey{}, rulesChange{by, what})
}

func rulesChangeFrom(ctx context.Context) rulesChange {
	c, _ := ctx.Value(rulesChangeKey{}).(rulesChange)
	if c.what == "" {
		c.what = "set"
	}
	return c
}

// AgentRulesVersion is one version of an agent's rules: the rules as a change left them, who changed them and how.
type AgentRulesVersion struct {
	Version   int        `json:"version"`
	Rules     AgentRules `json:"rules"`
	ChangedBy string     `json:"changed_by"` // the credential: "operator", or its method, user and key ("jwt:user:…")
	Change    string     `json:"change"`     // set | template <id> | rollback to <n> | before history
	CreatedAt time.Time  `json:"created_at"`
}

// recordRulesVersion records the agent's rules as tx has left them as its next version, unless they are its latest.
// The change holds the agent_rules row locked, so concurrent changes are numbered in the order they commit. A new
// version revokes the agent's Know Your Agent credential, whose limits it changed (B30.5).
func recordRulesVersion(ctx context.Context, tx pgx.Tx, agentID string, c rulesChange) error {
	tag, err := tx.Exec(ctx, `
		WITH cur AS (SELECT workspace_id, to_jsonb(r) - 'agent_id' - 'workspace_id' - 'updated_at' AS rules
		               FROM agent_rules r WHERE agent_id = $1),
		     latest AS (SELECT version, rules FROM agent_rules_versions WHERE agent_id = $1 ORDER BY version DESC LIMIT 1)
		INSERT INTO agent_rules_versions (agent_id, workspace_id, version, rules, changed_by, change)
		SELECT $1, cur.workspace_id, COALESCE((SELECT version FROM latest), 0) + 1, cur.rules, $2, $3 FROM cur
		 WHERE cur.rules IS DISTINCT FROM (SELECT rules FROM latest)`, agentID, c.by, c.what)
	if err != nil {
		return fmt.Errorf("economy: record agent rules version: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return revokeAgentKYA(ctx, tx, agentID, KYARevokedRules)
	}
	return nil
}

func agentExists(ctx context.Context, q pgxDB, workspaceID, agentID string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return fmt.Errorf("economy: agent: %w", err)
	}
	if !exists {
		return ErrAgentNotFound
	}
	return nil
}

// prefixedRow scans the columns before the rules into prefix, and the rules' columns as scanAgentRules does.
type prefixedRow struct {
	pgx.Row
	prefix []any
}

func (r prefixedRow) Scan(dest ...any) error { return r.Row.Scan(append(r.prefix, dest...)...) }

// AgentRulesHistory reads the versions of an agent's rules, newest first: the first is the rules in force. An agent
// whose rules were never set has none.
func (s *DualTokenStore) AgentRulesHistory(ctx context.Context, workspaceID, agentID string) ([]AgentRulesVersion, error) {
	if err := agentExists(ctx, s.pool, workspaceID, agentID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT v.version, v.changed_by, v.change, v.created_at, `+agentRulesColumns+`
		FROM agent_rules_versions v, jsonb_populate_record(NULL::agent_rules, v.rules)
		WHERE v.agent_id = $1 ORDER BY v.version DESC LIMIT 200`, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: agent rules history: %w", err)
	}
	defer rows.Close()
	out := []AgentRulesVersion{}
	for rows.Next() {
		var v AgentRulesVersion
		if v.Rules, err = scanAgentRules(prefixedRow{rows, []any{&v.Version, &v.ChangedBy, &v.Change, &v.CreatedAt}}); err != nil {
			return nil, fmt.Errorf("economy: agent rules history: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RollbackAgentRules puts the agent's rules back exactly as they were at version — every rule the version names; a
// rule added since keeps its value — and records that as a new version, by whoever ctx names. It returns the rules
// now in force.
func (s *DualTokenStore) RollbackAgentRules(ctx context.Context, workspaceID, agentID string, version int) (AgentRules, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentRules{}, fmt.Errorf("economy: roll back agent rules: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := s.rollbackAgentRulesTx(ctx, tx, workspaceID, agentID, version)
	if err != nil {
		return r, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, fmt.Errorf("economy: roll back agent rules: %w", err)
	}
	return r, nil
}

// rollbackAgentRulesTx is RollbackAgentRules in tx.
func (s *DualTokenStore) rollbackAgentRulesTx(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, version int) (AgentRules, error) {
	if err := agentExists(ctx, tx, workspaceID, agentID); err != nil {
		return AgentRules{}, err
	}
	var snapshot string
	err := tx.QueryRow(ctx, `SELECT rules::text FROM agent_rules_versions WHERE agent_id = $1 AND version = $2`, agentID, version).Scan(&snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRules{}, ErrRulesVersionNotFound
	}
	if err != nil {
		return AgentRules{}, fmt.Errorf("economy: roll back agent rules: %w", err)
	}
	// The row is written back whole, so every column the version names is restored without naming them here; its
	// removal locks the agent's rules against a concurrent change until this commits.
	current := "{}"
	if err := tx.QueryRow(ctx, `DELETE FROM agent_rules WHERE agent_id = $1 RETURNING to_jsonb(agent_rules.*)::text`, agentID).
		Scan(&current); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentRules{}, fmt.Errorf("economy: roll back agent rules: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_rules SELECT (jsonb_populate_record(NULL::agent_rules,
		$1::jsonb || $2::jsonb || jsonb_build_object('agent_id', $3::text, 'workspace_id', $4::text, 'updated_at', now()))).*`,
		current, snapshot, agentID, workspaceID); err != nil {
		return AgentRules{}, fmt.Errorf("economy: roll back agent rules: %w", err)
	}
	if err := recordRulesVersion(ctx, tx, agentID, rulesChange{rulesChangeFrom(ctx).by, fmt.Sprintf("rollback to %d", version)}); err != nil {
		return AgentRules{}, err
	}
	r, err := scanAgentRules(tx.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if err != nil {
		return r, fmt.Errorf("economy: roll back agent rules: %w", err)
	}
	// B32.32: an earlier version of a room wallet's rules is judged as the rules saved now are.
	return r, s.checkRoomBudget(ctx, tx, workspaceID, agentID, r.MonthlyLimitULXC)
}
