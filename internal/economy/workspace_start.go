package economy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// workspace_start.go — B45.7: a new workspace starts with test money and a first agent, so its owner can make a
// first request within two minutes of signing up. Identity checks wait for the first real-money action, which the
// wallet capabilities already keep off until a partner signs.

// LXCTypeStarterTest is the lxc_ledger row of a new workspace's starting test money: funding "test" on its
// metadata, counted in test_funded_ulxc, never cash-backed.
const LXCTypeStarterTest = "starter_test_money"

// FirstAgentName is the name of the agent a new workspace starts with.
const FirstAgentName = "My first agent"

// FirstAgentTemplate is the rule template the first agent starts on: the tightest one, with a small cap on each
// request, an hourly cap that stops a loop, a person asked above a few LXC and a pause on unusual spend.
const FirstAgentTemplate = "support-bot"

// WorkspaceStart is what StartWorkspace made: the first agent, its key (shown once) and the test money it holds.
type WorkspaceStart struct {
	Agent         Agent      `json:"agent"`
	Rules         AgentRules `json:"rules"`
	Template      string     `json:"template"`
	Key           string     `json:"key"`
	KeyID         string     `json:"key_id"`
	TestMoneyULXC int64      `json:"test_money_ulxc"`
}

// KeyIssuer mints the first agent's proxy key inside the start's transaction and returns its raw value and id.
type KeyIssuer func(ctx context.Context, tx pgx.Tx, workspaceID, name string) (raw, keyID string, err error)

// StartWorkspace gives workspaceID, in one transaction, a first agent owned by ownerUserID on the
// FirstAgentTemplate rules, a key for it from issue, and testMoneyULXC of test money credited to the workspace
// and funded to that agent (none when it is 0). A workspace that already has an agent is left as it is:
// started is false and nothing changes, so a retried create moves no money and makes no second agent.
func (s *DualTokenStore) StartWorkspace(ctx context.Context, workspaceID, ownerUserID string, testMoneyULXC int64, issue KeyIssuer) (out WorkspaceStart, started bool, err error) {
	return s.StartWorkspaceAgent(ctx, workspaceID, ownerUserID, FirstAgentName, nil, testMoneyULXC, issue)
}

// StartWorkspaceAgent is StartWorkspace with the first agent called name and, when adjust is not nil, its
// FirstAgentTemplate rules changed by adjust before they are saved (B45.33: N1CO's monthly budget).
func (s *DualTokenStore) StartWorkspaceAgent(ctx context.Context, workspaceID, ownerUserID, name string, adjust func(*AgentRules),
	testMoneyULXC int64, issue KeyIssuer) (out WorkspaceStart, started bool, err error) {
	if testMoneyULXC < 0 {
		return out, false, errors.New("economy: starting test money cannot be negative")
	}
	tmpl, ok := RuleTemplateByID(FirstAgentTemplate)
	if !ok {
		return out, false, fmt.Errorf("economy: no rule template %q", FirstAgentTemplate)
	}
	if s.pool == nil {
		return out, false, nil // no database, no agents
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockWorkspaceAgents(ctx, tx, workspaceID); err != nil {
		return out, false, err
	}
	var has bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE workspace_id = $1 AND kind = 'agent')`,
		workspaceID).Scan(&has); err != nil {
		return out, false, fmt.Errorf("economy: workspace start: %w", err)
	}
	if has {
		return out, false, nil
	}
	if out.Agent, err = createAgentTx(ctx, tx, workspaceID, name, ownerUserID); err != nil {
		return out, false, err
	}
	if out.Key, out.KeyID, err = issue(ctx, tx, workspaceID, name); err != nil {
		return out, false, fmt.Errorf("economy: first agent's key: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_account_keys (scoped_key_id, agent_id) VALUES ($1, $2)`, out.KeyID, out.Agent.ID); err != nil {
		return out, false, fmt.Errorf("economy: attach the first agent's key: %w", err)
	}
	out.Agent.Keys = []string{out.KeyID}
	rulesCtx := WithRulesChange(ctx, ownerUserID, "template "+tmpl.ID)
	rules := tmpl.Rules
	if adjust != nil {
		adjust(&rules)
	}
	if out.Rules, err = s.setAgentRulesTx(rulesCtx, tx, workspaceID, out.Agent.ID, rules); err != nil {
		return out, false, err
	}
	out.Template = tmpl.ID
	if testMoneyULXC > 0 {
		if _, err := s.creditLXCTxTyped(ctx, tx, workspaceID, testMoneyULXC, LXCTypeStarterTest, "starting test money",
			map[string]interface{}{"funding": FundingTest, "agent_id": out.Agent.ID}); err != nil {
			return out, false, err
		}
		if err := postEntry(ctx, tx, workspaceID, "fund", "starter", leg{"workspace", -testMoneyULXC},
			leg{agentAccount(out.Agent.ID), testMoneyULXC}); err != nil {
			return out, false, err
		}
		out.TestMoneyULXC = testMoneyULXC
	}
	if err := tx.Commit(ctx); err != nil {
		return out, false, fmt.Errorf("economy: workspace start: %w", err)
	}
	s.openNewAgentWallet(ctx, workspaceID, out.Agent.ID) // B46.4
	return out, true, nil
}
