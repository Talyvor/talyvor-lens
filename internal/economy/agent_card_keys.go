package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// agent_card_keys.go — B45.23: what an agent's A2A Agent Card says (internal/a2acard signs it) and the public keys it
// is signed with (agent_card_keys, migration 0264).

// AgentCardFacts is what an agent's card says of it: its name and description, and who answers for it — never the
// agent itself.
type AgentCardFacts struct {
	AgentID     string
	WorkspaceID string
	Name        string
	Description string
	Owner       KYAOwner
	Archived    bool
}

// AgentCardKey is the public half of one agent's card signing key.
type AgentCardKey struct {
	Kid           string
	WorkspaceID   string
	AgentID       string
	Generation    int
	PublicKey     string // base64url Ed25519
	CreatedAt     time.Time
	RevokedAt     *time.Time
	RevokedReason string
}

// ErrAgentCardKeyNotFound: Talyvor never made a card key with that kid.
var ErrAgentCardKeyNotFound = errors.New("economy: no such agent card key")

// AgentCardFacts reads what agentID's card says now.
func (s *DualTokenStore) AgentCardFacts(ctx context.Context, agentID string) (AgentCardFacts, error) {
	f := AgentCardFacts{AgentID: agentID}
	err := s.pool.QueryRow(ctx, `SELECT workspace_id, name, description, archived_at IS NOT NULL FROM agent_accounts WHERE id = $1`,
		agentID).Scan(&f.WorkspaceID, &f.Name, &f.Description, &f.Archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrAgentNotFound
	}
	if err != nil {
		return f, fmt.Errorf("economy: agent card: %w", err)
	}
	f.Owner, err = kyaOwner(ctx, s.pool, f.WorkspaceID)
	return f, err
}

const agentCardKeyColumns = `kid, workspace_id, agent_id, generation, public_key, created_at, revoked_at, revoked_reason`

func scanAgentCardKey(row pgx.Row) (AgentCardKey, error) {
	var k AgentCardKey
	err := row.Scan(&k.Kid, &k.WorkspaceID, &k.AgentID, &k.Generation, &k.PublicKey, &k.CreatedAt, &k.RevokedAt, &k.RevokedReason)
	return k, err
}

// AgentCardKeyInForce is agentID's card key in force, if it has one, and the highest generation it has had (0 for none).
func (s *DualTokenStore) AgentCardKeyInForce(ctx context.Context, agentID string) (k AgentCardKey, ok bool, generation int, err error) {
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(generation), 0) FROM agent_card_keys WHERE agent_id = $1`, agentID).Scan(&generation); err != nil {
		return k, false, 0, fmt.Errorf("economy: agent card key: %w", err)
	}
	k, err = scanAgentCardKey(s.pool.QueryRow(ctx, `SELECT `+agentCardKeyColumns+` FROM agent_card_keys
		WHERE agent_id = $1 AND revoked_at IS NULL`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return k, false, generation, nil
	}
	if err != nil {
		return k, false, 0, fmt.Errorf("economy: agent card key: %w", err)
	}
	return k, true, generation, nil
}

// AddAgentCardKey records k as its agent's key in force. It adds nothing when the agent has one already, or has had
// that generation — another request made it first — and says whether it added k.
func (s *DualTokenStore) AddAgentCardKey(ctx context.Context, k AgentCardKey) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO agent_card_keys (kid, workspace_id, agent_id, generation, public_key)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, k.Kid, k.WorkspaceID, k.AgentID, k.Generation, k.PublicKey)
	if err != nil {
		return false, fmt.Errorf("economy: agent card key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeAgentCardKey revokes agentID's card key in force, if it has one, for reason.
func (s *DualTokenStore) RevokeAgentCardKey(ctx context.Context, agentID, reason string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE agent_card_keys SET revoked_at = now(), revoked_reason = $2
		WHERE agent_id = $1 AND revoked_at IS NULL`, agentID, reason); err != nil {
		return fmt.Errorf("economy: agent card key: %w", err)
	}
	return nil
}

// AgentCardKey is the card key kid, revoked or not.
func (s *DualTokenStore) AgentCardKey(ctx context.Context, kid string) (AgentCardKey, error) {
	k, err := scanAgentCardKey(s.pool.QueryRow(ctx, `SELECT `+agentCardKeyColumns+` FROM agent_card_keys WHERE kid = $1`, kid))
	if errors.Is(err, pgx.ErrNoRows) {
		return k, ErrAgentCardKeyNotFound
	}
	if err != nil {
		return k, fmt.Errorf("economy: agent card key: %w", err)
	}
	return k, nil
}
