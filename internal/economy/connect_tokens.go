package economy

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// B54.13 — what a Connect token (B52.4) is checked against on every payment, beyond what the token itself says.

// ConnectApprovalsWaiting is how many approvals one assistant's payments may leave waiting on one agent, so an
// assistant cannot flood the owner.
const ConnectApprovalsWaiting = 5

var (
	// ErrConnectEnded is a Connect token whose connection the person ended, or whose agent has another owner now.
	ErrConnectEnded = errors.New("economy: this assistant's connection has ended, or the agent has another owner now")
	// ErrConnectApprovalsWaiting is a payment that would leave one approval too many waiting for the owner.
	ErrConnectApprovalsWaiting = fmt.Errorf("economy: this assistant already has %d payments waiting for the owner's approval; "+
		"ask the owner to answer them first", ConnectApprovalsWaiting)
)

type connectClientKey struct{}

type connectPrepareKey struct{}

// WithConnectPrepareOnly marks a payment as made on a payments:prepare Connect token (B52.41): the agent's rules judge it
// as ever, and then it always waits for the owner's approval, whatever their approval amount allows.
func WithConnectPrepareOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, connectPrepareKey{}, true)
}

func prepareOnly(ctx context.Context) bool {
	on, _ := ctx.Value(connectPrepareKey{}).(bool)
	return on
}

// WithConnectClient marks a payment as made by the assistant clientID, so the approval it files counts against
// ConnectApprovalsWaiting.
func WithConnectClient(ctx context.Context, clientID string) context.Context {
	return context.WithValue(ctx, connectClientKey{}, clientID)
}

// RevokeConnect records that userID ended the connection of the assistant clientID, or of every assistant when clientID
// is "": every Connect token issued until now stops paying.
func (s *DualTokenStore) RevokeConnect(ctx context.Context, userID, clientID string) error {
	if userID == "" {
		return errors.New("economy: a revocation names the person")
	}
	// On Lens's clock, the one a token's issued-at is on, not the database's: a database behind it would spare a token.
	if _, err := s.pool.Exec(ctx, `INSERT INTO connect_revocations (user_id, client_id, revoked_at) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, client_id) DO UPDATE SET revoked_at = GREATEST(connect_revocations.revoked_at, EXCLUDED.revoked_at)`,
		userID, clientID, time.Now()); err != nil {
		return fmt.Errorf("economy: record the revocation: %w", err)
	}
	return nil
}

// ConnectLive is nil while a Connect token issued at issuedAt to userID for clientID may still pay from agentID:
// the person still owns the agent and has not ended the connection since. issuedAt is whole seconds, so a token
// minted in the second of a revocation counts as before it.
func (s *DualTokenStore) ConnectLive(ctx context.Context, workspaceID, agentID, userID, clientID string, issuedAt time.Time) error {
	var owns, revoked bool
	if err := s.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2 AND owner_user_id = $3 AND $3 <> ''),
		EXISTS (SELECT 1 FROM connect_revocations WHERE user_id = $3 AND client_id IN ($4, '') AND revoked_at >= $5)`,
		agentID, workspaceID, userID, clientID, issuedAt).Scan(&owns, &revoked); err != nil {
		return fmt.Errorf("economy: check the connection: %w", err)
	}
	if !owns || revoked {
		return ErrConnectEnded
	}
	return nil
}
