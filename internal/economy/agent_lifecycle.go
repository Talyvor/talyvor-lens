package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// agent_lifecycle.go — B28.298: an agent is renamed, described and archived.
//
// Archiving retires an agent in ONE transaction, under its lock: its whole balance goes back to the workspace
// as one withdraw entry, its proxy keys are deleted from workspace_api_keys (so they no longer authenticate),
// its top-up and the schedules it pays or is paid by stop, and archived_at is set. From then on it cannot be
// funded, and every movement of its own — a hold, a spend, a payment — is refused (refuseIfPaused), so even a
// key that somehow still authenticated would write no hold. What it keeps on the record: its postings, its
// statement, its name.

// ErrAgentArchived: the agent is archived, so it cannot be funded, given a key or archived again.
var ErrAgentArchived = errors.New("economy: this agent is archived")

// ErrAgentPotsHeld: the agent still keeps LXC in its pots, which may be locked; it must move them back first.
var ErrAgentPotsHeld = errors.New("economy: this agent keeps LXC in its pots — move it back to its balance before archiving")

// ErrAgentDetails: a blank name, or a description longer than MaxAgentDescription.
var ErrAgentDetails = errors.New("economy: invalid agent details")

// MaxAgentDescription is the longest description an agent takes, in characters.
const MaxAgentDescription = 500

// AgentArchive is what archiving an agent did.
type AgentArchive struct {
	AgentID     string    `json:"agent_id"`
	SweptULXC   int64     `json:"swept_ulxc"`   // the balance moved back to the workspace, in one withdraw entry
	RevokedKeys []string  `json:"revoked_keys"` // the agent's proxy keys, revoked
	ArchivedAt  time.Time `json:"archived_at"`
}

// requireNotArchived refuses a movement into an archived agent (B28.298), inside the caller's transaction.
func requireNotArchived(ctx context.Context, tx pgx.Tx, agentID string) error {
	var archived bool
	if err := tx.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM agent_accounts WHERE id = $1`, agentID).Scan(&archived); err != nil {
		return fmt.Errorf("economy: agent archived: %w", err)
	}
	if archived {
		return ErrAgentArchived
	}
	return nil
}

// UpdateAgent renames and/or describes an agent; a nil field is left as it is. A name cannot be blank, and a
// description is at most MaxAgentDescription characters. Returns the agent as the book reads it.
func (s *DualTokenStore) UpdateAgent(ctx context.Context, workspaceID, agentID string, name, description *string) (Agent, error) {
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" {
			return Agent{}, fmt.Errorf("%w: an agent's name cannot be blank", ErrAgentDetails)
		}
		name = &trimmed
	}
	if description != nil {
		trimmed := strings.TrimSpace(*description)
		if utf8.RuneCountInString(trimmed) > MaxAgentDescription {
			return Agent{}, fmt.Errorf("%w: a description is at most %d characters", ErrAgentDetails, MaxAgentDescription)
		}
		description = &trimmed
	}
	tag, err := s.pool.Exec(ctx, `UPDATE agent_accounts SET name = COALESCE($3, name), description = COALESCE($4, description)
		WHERE id = $1 AND workspace_id = $2`, agentID, workspaceID, name, description)
	if err != nil {
		return Agent{}, fmt.Errorf("economy: update agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Agent{}, ErrAgentNotFound
	}
	book, err := s.AgentBook(ctx, workspaceID)
	if err != nil {
		return Agent{}, err
	}
	for _, a := range book.Agents {
		if a.ID == agentID {
			return a, nil
		}
	}
	return Agent{}, ErrAgentNotFound
}

// ArchiveAgent retires an agent: its balance back to the workspace in one withdraw entry, its keys revoked,
// its top-up and schedules stopped, archived_at set — all in one transaction. An agent with LXC in its pots
// is refused (ErrAgentPotsHeld): a pot may be locked, and its owner moves it back first.
func (s *DualTokenStore) ArchiveAgent(ctx context.Context, workspaceID, agentID string) (AgentArchive, error) {
	out := AgentArchive{AgentID: agentID, RevokedKeys: []string{}}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return out, err
	}
	if err := requireNotArchived(ctx, tx, agentID); err != nil {
		return out, err
	}
	var pots int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(b.balance_ulxc), 0)::bigint FROM agent_account_balances b
		JOIN agent_pots t ON b.account = 'pot:' || t.id WHERE t.agent_id = $1 AND b.workspace_id = $2`,
		agentID, workspaceID).Scan(&pots); err != nil {
		return out, fmt.Errorf("economy: agent pots: %w", err)
	}
	if pots != 0 {
		return out, fmt.Errorf("%w (%d µLXC)", ErrAgentPotsHeld, pots)
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(agentID))
	if err != nil {
		return out, err
	}
	if bal > 0 {
		if err := postEntry(ctx, tx, workspaceID, "withdraw", "archive:"+agentID, leg{agentAccount(agentID), -bal}, leg{"workspace", bal}); err != nil {
			return out, err
		}
		out.SweptULXC = bal
	}
	rows, err := tx.Query(ctx, `DELETE FROM workspace_api_keys WHERE workspace_id = $2
		AND id::text IN (SELECT scoped_key_id FROM agent_account_keys WHERE agent_id = $1) RETURNING id::text`, agentID, workspaceID)
	if err != nil {
		return out, fmt.Errorf("economy: revoke agent keys: %w", err)
	}
	revoked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return out, fmt.Errorf("economy: revoke agent keys: %w", err)
	}
	out.RevokedKeys = append(out.RevokedKeys, revoked...)
	if _, err := tx.Exec(ctx, `DELETE FROM agent_topups WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID); err != nil {
		return out, fmt.Errorf("economy: stop agent top-up: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_payment_schedules SET active = false, awaiting_signature = false
		WHERE workspace_id = $2 AND (from_agent_id = $1 OR to_agent_id = $1)`, agentID, workspaceID); err != nil {
		return out, fmt.Errorf("economy: stop agent schedules: %w", err)
	}
	if err := tx.QueryRow(ctx, `UPDATE agent_accounts SET archived_at = now() WHERE id = $1 RETURNING archived_at`,
		agentID).Scan(&out.ArchivedAt); err != nil {
		return out, fmt.Errorf("economy: archive agent: %w", err)
	}
	if err := revokeAgentKYA(ctx, tx, agentID, KYARevokedArchived); err != nil { // B30.5
		return out, err
	}
	return out, tx.Commit(ctx)
}
