package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/passkey"
)

// agent_rules_proposals.go — B52.41: A RAISE FILED FOR THE PASSKEY.
//
// A connected assistant cannot sign with the owner's passkey, so a change it asks for that loosens an agent's rules is
// kept as a proposal and applies nothing. The owner opens it, signs its challenge (the same one PUT …/rules would ask
// for, B55.13) and approves it once. A proposal that only tightens applies at once, as PUT …/rules does unsigned.

// ErrRulesProposalNotFound: no such pending proposal for this agent.
var ErrRulesProposalNotFound = errors.New("economy: no such rules proposal waiting for this agent")

// FiledRules is a proposal as filed: pending with its id when it loosens the rules, applied when it does not.
type FiledRules struct {
	ID      string     `json:"id,omitempty"`
	Status  string     `json:"status"` // pending | applied
	Changes []string   `json:"changes"`
	Rules   AgentRules `json:"rules"`
}

// ProposeAgentRules files rules for the agent: kept as a pending proposal when they loosen any rule in force, applied
// at once (recorded with who ctx names, WithRulesChange) when they do not.
func (s *DualTokenStore) ProposeAgentRules(ctx context.Context, workspaceID, agentID string, rules AgentRules) (FiledRules, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FiledRules{}, fmt.Errorf("economy: propose agent rules: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now, j, _, err := s.proposeRulesTx(ctx, tx, workspaceID, agentID, RulesProposal{Rules: &rules})
	if err != nil {
		return FiledRules{}, err
	}
	if !j.Looser {
		return FiledRules{Status: "applied", Changes: j.Changes, Rules: now}, tx.Commit(ctx)
	}
	_ = tx.Rollback(ctx)
	b, err := json.Marshal(rules)
	if err != nil {
		return FiledRules{}, err
	}
	f := FiledRules{ID: "rprop_" + uuid.NewString(), Status: "pending", Changes: j.Changes, Rules: rules}
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_rules_proposals (id, workspace_id, agent_id, rules) VALUES ($1, $2, $3, $4)`,
		f.ID, workspaceID, agentID, b); err != nil {
		return FiledRules{}, fmt.Errorf("economy: file rules proposal: %w", err)
	}
	return f, nil
}

// pendingProposal reads a pending proposal's rules, locked inside a transaction so it is approved once.
func pendingProposal(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workspaceID, agentID, proposalID string) (AgentRules, error) {
	var b []byte
	err := q.QueryRow(ctx, `SELECT rules FROM agent_rules_proposals WHERE id = $1 AND workspace_id = $2 AND agent_id = $3
		AND status = 'pending' FOR UPDATE`, proposalID, workspaceID, agentID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRules{}, ErrRulesProposalNotFound
	}
	if err != nil {
		return AgentRules{}, fmt.Errorf("economy: rules proposal: %w", err)
	}
	var rules AgentRules
	return rules, json.Unmarshal(b, &rules)
}

// RulesProposalChallenge is RulesChallenge for a pending proposal's rules, and those rules.
func (s *DualTokenStore) RulesProposalChallenge(ctx context.Context, workspaceID, agentID, proposalID string) (RulesJudgement, AgentRules, error) {
	rules, err := pendingProposal(ctx, s.pool, workspaceID, agentID, proposalID)
	if err != nil {
		return RulesJudgement{}, rules, err
	}
	j, err := s.RulesChallenge(ctx, workspaceID, agentID, RulesProposal{Rules: &rules})
	return j, rules, err
}

// ApproveRulesProposal applies a pending proposal once and returns the rules now in force. When it still loosens them
// and the workspace has a passkey or device key, only with a's assertion over RulesProposalChallenge's challenge,
// spent in the same transaction, as ChangeAgentRules does.
func (s *DualTokenStore) ApproveRulesProposal(ctx context.Context, workspaceID, agentID, proposalID string, a *passkey.Assertion) (AgentRules, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentRules{}, fmt.Errorf("economy: approve rules proposal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rules, err := pendingProposal(ctx, tx, workspaceID, agentID, proposalID)
	if err != nil {
		return AgentRules{}, err
	}
	now, j, purpose, err := s.proposeRulesTx(ctx, tx, workspaceID, agentID, RulesProposal{Rules: &rules})
	if err != nil {
		return now, err
	}
	if j.Looser {
		if err := s.authorizeSignedTx(ctx, tx, workspaceID, purpose, a, ErrRulesUnsigned); err != nil {
			return now, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_rules_proposals SET status = 'applied', applied_at = now() WHERE id = $1`, proposalID); err != nil {
		return now, fmt.Errorf("economy: approve rules proposal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return now, fmt.Errorf("economy: approve rules proposal: %w", err)
	}
	return now, nil
}
