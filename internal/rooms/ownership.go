package rooms

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
)

// ownership.go — B41.141: SHARED OWNERSHIP (INTENT ONLY): THE OWNER SETS THE CONTRIBUTOR POOL (migration 0292).
//
// A room's owner switches shared ownership on and sets the share of the room's future value it intends for
// contributors: 10–50%, 25% by default. It records intent and has no legal effect, and every answer carries the
// notice that says so. Capability room_equity is RED: a room whose owner, or a caller, is not a synthetic workspace
// answers ErrNotFound — as if the feature did not exist — until an operator records the lawyer's clearance of the
// wording. It is shown only to the room's live members who accepted its current terms.

// OwnershipNotice is the wording every shared-ownership answer carries, exactly as the lawyer's brief gives it.
const OwnershipNotice = "No shares, options or rights arise unless and until a company is formed, a regulated partner adopts a Contributor Plan, and the Gate is met"

// The contributor pool's bounds and default, in basis points.
const (
	MinPoolBPS     = 1000
	MaxPoolBPS     = 5000
	DefaultPoolBPS = 2500
)

// SharedOwnership is a room's shared-ownership setting as its members see it.
type SharedOwnership struct {
	RoomID    string     `json:"room_id"`
	Enabled   bool       `json:"enabled"`
	PoolBPS   int        `json:"pool_bps"`
	Notice    string     `json:"notice"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"` // nil: the owner has not set it
}

// OwnershipChange is PUT /v1/rooms/{id}/shared-ownership: on or off, and the pool (25% when left out).
type OwnershipChange struct {
	Enabled bool `json:"enabled"`
	PoolBPS *int `json:"pool_bps"`
}

// equityOpen answers ErrNotFound unless room_equity reaches roomID for ws: both the room's owner and ws are synthetic
// workspaces, or the capability is cleared. Every shared-ownership route asks it first.
func equityOpen(ctx context.Context, tx pgx.Tx, roomID, ws string) error {
	var synthetic bool
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = r.owner_workspace_id), false)
		AND COALESCE((SELECT synthetic FROM workspaces WHERE id = $2), false) FROM rooms r WHERE r.id = $1`, roomID, ws).Scan(&synthetic)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no such room", ErrNotFound)
	}
	if err != nil || synthetic {
		return err
	}
	cleared, err := economy.ClearedAnywhere(ctx, tx, economy.CapabilityRoomEquity)
	if err == nil && !cleared {
		err = fmt.Errorf("%w: no such room", ErrNotFound)
	}
	return err
}

// ownershipMember answers ErrNotFound to anyone who is not a live member of roomID, and ErrConflict to a member that
// has not accepted the room's current terms.
func ownershipMember(ctx context.Context, tx pgx.Tx, roomID, ws string) (Member, error) {
	if err := equityOpen(ctx, tx, roomID, ws); err != nil {
		return Member{}, err
	}
	m, ok, err := member(ctx, tx, roomID, ws)
	switch {
	case err != nil:
		return m, err
	case !ok:
		return m, fmt.Errorf("%w: no such room", ErrNotFound)
	case !m.TermsCurrent:
		return m, fmt.Errorf("%w: accept the room's current terms first", ErrConflict)
	}
	return m, nil
}

func readOwnership(ctx context.Context, tx pgx.Tx, roomID string) (SharedOwnership, error) {
	o := SharedOwnership{RoomID: roomID, PoolBPS: DefaultPoolBPS, Notice: OwnershipNotice}
	err := tx.QueryRow(ctx, `SELECT enabled, pool_bps, updated_at FROM room_shared_ownership WHERE room_id = $1`, roomID).
		Scan(&o.Enabled, &o.PoolBPS, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return o, err
}

// SharedOwnership is roomID's shared-ownership setting, for ws, a member that accepted the room's current terms.
func (s *Store) SharedOwnership(ctx context.Context, ws, roomID string) (o SharedOwnership, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		o, err = readOwnership(ctx, tx, roomID)
		return err
	})
	return o, err
}

// SetSharedOwnership switches roomID's shared ownership on or off and sets its pool; only the room's owner does.
func (s *Store) SetSharedOwnership(ctx context.Context, ws, user, roomID string, c OwnershipChange) (o SharedOwnership, err error) {
	pool := DefaultPoolBPS
	if c.PoolBPS != nil {
		pool = *c.PoolBPS
	}
	if pool < MinPoolBPS || pool > MaxPoolBPS {
		return o, invalid("the contributor pool is between %d and %d basis points (10%%–50%%)", MinPoolBPS, MaxPoolBPS)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		m, err := ownershipMember(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		if m.Role != RoleOwner {
			return forbidden("only the room's owner sets its shared ownership")
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if r.Status != Open {
			return fmt.Errorf("%w: the room is %s", ErrConflict, r.Status)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_shared_ownership (room_id, enabled, pool_bps, set_by_user_id) VALUES ($1, $2, $3, $4)
			ON CONFLICT (room_id) DO UPDATE SET enabled = EXCLUDED.enabled, pool_bps = EXCLUDED.pool_bps,
				set_by_user_id = EXCLUDED.set_by_user_id, updated_at = now()`, roomID, c.Enabled, pool, user); err != nil {
			return err
		}
		o, err = readOwnership(ctx, tx, roomID)
		return err
	})
	return o, err
}
