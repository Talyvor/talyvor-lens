package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrWorkspaceFrozen: an operator froze the workspace's money capabilities on a compliance case (B30.8).
var ErrWorkspaceFrozen = errors.New("economy: refused: an operator has frozen this workspace's money capabilities")

// Freeze is an operator's freeze on a workspace's money capabilities, on a compliance case: while it stands every
// AMBER and RED capability refuses the workspace's money, test or live, and the GREEN ones — Talyvor's own
// services — go on.
type Freeze struct {
	WorkspaceID string    `json:"workspace_id"`
	CaseID      string    `json:"case_id"`
	Reason      string    `json:"reason"`
	FrozenBy    string    `json:"frozen_by"`
	FrozenAt    time.Time `json:"frozen_at"`
}

// FreezeLockSQL serialises a freeze with the money it stops: a movement holds the workspace's freeze lock shared until
// its transaction ends, and a freeze or an unfreeze takes it exclusively, so a freeze waits for the money already
// moving and every movement after it sees it.
const FreezeLockSQL = `SELECT pg_advisory_xact_lock_shared(hashtextextended('compliance_freeze:' || $1, 0))`

// FreezeLockExclusiveSQL is FreezeLockSQL's exclusive side, taken by a freeze or an unfreeze.
const FreezeLockExclusiveSQL = `SELECT pg_advisory_xact_lock(hashtextextended('compliance_freeze:' || $1, 0))`

// WorkspaceFreeze is the freeze on workspaceID now, or nil.
func WorkspaceFreeze(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, workspaceID string) (*Freeze, error) {
	var f Freeze
	err := q.QueryRow(ctx, `SELECT workspace_id, case_id, reason, frozen_by, frozen_at FROM compliance_freezes WHERE workspace_id = $1`,
		workspaceID).Scan(&f.WorkspaceID, &f.CaseID, &f.Reason, &f.FrozenBy, &f.FrozenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("economy: compliance freeze: %w", err)
	}
	return &f, nil
}

// frozenRefusal refuses capability c for workspaceID while an operator has it frozen — c AMBER or RED, test money or
// live — and answers nil for a GREEN one, or a workspace not frozen.
func frozenRefusal(ctx context.Context, q pgxDB, workspaceID string, c Capability) (*CapabilityRefusal, error) {
	if c.Class == ClassGreen {
		return nil, nil
	}
	return freezeRefusal(ctx, q, workspaceID, c)
}

// freezeRefusal refuses c for workspaceID while it is frozen, whatever c's class. In a transaction it holds the
// freeze lock shared until the transaction ends (FreezeLockSQL).
func freezeRefusal(ctx context.Context, q pgxDB, workspaceID string, c Capability) (*CapabilityRefusal, error) {
	if _, ok := q.(pgx.Tx); ok {
		if _, err := q.Exec(ctx, FreezeLockSQL, workspaceID); err != nil {
			return nil, fmt.Errorf("economy: compliance freeze: %w", err)
		}
	}
	f, err := WorkspaceFreeze(ctx, q, workspaceID)
	if err != nil || f == nil {
		return nil, err
	}
	return &CapabilityRefusal{Capability: c, Freeze: f}, nil
}

// refuseFrozen is frozenRefusal as an error: nil when c may move workspaceID's money.
func refuseFrozen(ctx context.Context, q pgxDB, workspaceID string, c Capability) error {
	return refusalErr(frozenRefusal(ctx, q, workspaceID, c))
}

// refuseFrozenAnyClass is freezeRefusal as an error: money a GREEN capability would move into or out of a frozen
// workspace — to another workspace of the same owner — is refused too, so a freeze cannot be stepped round.
func refuseFrozenAnyClass(ctx context.Context, q pgxDB, workspaceID string, c Capability) error {
	return refusalErr(freezeRefusal(ctx, q, workspaceID, c))
}

func refusalErr(r *CapabilityRefusal, err error) error {
	if err != nil {
		return err
	}
	if r != nil {
		return r
	}
	return nil
}
