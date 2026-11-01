package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockdown.go — B41.116: LOST-PHONE LOCKDOWN. From any signed-in session, one step — in one transaction — revokes the
// lost phone's device keys and passkeys, freezes every agent card, pauses every agent and records the lockdown.
// Stopping takes nothing but the session, as pausing does (B41.108). Nothing restarts while the lockdown stands: no
// agent resumes and no card unfreezes. Lifting it takes an identity check (L2, B30.4) passed after the lockdown began —
// by a real provider, or by the Test provider for a synthetic workspace, of the person checked before it — so a stolen
// session cannot lift it.

// ErrLockedDown: the workspace is locked down, so nothing restarts until the lockdown is lifted.
var ErrLockedDown = errors.New("economy: this workspace is locked down after a lost phone: pass a new identity check and lift the lockdown before anything restarts")

// ErrIdentityRecheck: no identity check has passed since the lockdown began.
var ErrIdentityRecheck = errors.New("economy: lifting the lockdown takes an identity check of the workspace's person, passed since it began")

// ErrPasskeyNotFound: no such passkey in the workspace.
var ErrPasskeyNotFound = errors.New("economy: no such passkey in this workspace")

// Lockdown is where a workspace's lockdown stands, and what starting it changed.
type Lockdown struct {
	Active          bool       `json:"active"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	StartedBy       string     `json:"started_by,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	RevokedDevices  []string   `json:"revoked_devices"`
	RevokedPasskeys []string   `json:"revoked_passkeys"`
	FrozenCards     int64      `json:"frozen_cards"`
}

// LockDown revokes the named device keys and passkeys — every one of them when none is named — freezes every agent
// card, pauses every agent and records the lockdown, all at once. Locking down again restarts its clock.
func (s *DualTokenStore) LockDown(ctx context.Context, workspaceID, by, reason string, deviceIDs, passkeyIDs []string) (Lockdown, error) {
	all := len(deviceIDs) == 0 && len(passkeyIDs) == 0
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Lockdown{}, fmt.Errorf("economy: lockdown: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The enrolment lock: no key registers half-way through.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('key_enrol:' || $1, 0))`, workspaceID); err != nil {
		return Lockdown{}, fmt.Errorf("economy: lockdown: %w", err)
	}
	if err := allIn(ctx, tx, `SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND id = ANY($2)`, workspaceID, deviceIDs, ErrDeviceKeyNotFound); err != nil {
		return Lockdown{}, err
	}
	if err := allIn(ctx, tx, `SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1 AND credential_id = ANY($2)`, workspaceID, passkeyIDs, ErrPasskeyNotFound); err != nil {
		return Lockdown{}, err
	}
	l := Lockdown{Active: true, StartedBy: by, Reason: reason}
	if l.RevokedDevices, err = collectIDs(ctx, tx, `UPDATE workspace_device_keys SET revoked_at = now(), revoked_by = $2
		WHERE workspace_id = $1 AND revoked_at IS NULL AND ($3 OR id = ANY($4)) RETURNING id`, workspaceID, by, all, deviceIDs); err != nil {
		return Lockdown{}, err
	}
	if l.RevokedPasskeys, err = collectIDs(ctx, tx, `UPDATE workspace_passkeys SET revoked_at = now(), revoked_by = $2
		WHERE workspace_id = $1 AND revoked_at IS NULL AND ($3 OR credential_id = ANY($4)) RETURNING credential_id`, workspaceID, by, all, passkeyIDs); err != nil {
		return Lockdown{}, err
	}
	// Every agent's row lock, as a card purchase takes it (SetAgentCardFrozen): one being decided finishes first.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_accounts WHERE workspace_id = $1 ORDER BY id FOR UPDATE`, workspaceID); err != nil {
		return Lockdown{}, fmt.Errorf("economy: lockdown: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_cards SET frozen = true WHERE workspace_id = $1 AND NOT frozen`, workspaceID)
	if err != nil {
		return Lockdown{}, fmt.Errorf("economy: lockdown: freeze cards: %w", err)
	}
	l.FrozenCards = tag.RowsAffected()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_workspace_pauses (workspace_id, reason) VALUES ($1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET reason = EXCLUDED.reason`, workspaceID, "lockdown: "+reason); err != nil {
		return Lockdown{}, fmt.Errorf("economy: lockdown: pause every agent: %w", err)
	}
	if err := revokeWorkspaceKYA(ctx, tx, workspaceID, KYARevokedFrozen); err != nil {
		return Lockdown{}, err
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO workspace_lockdowns (workspace_id, started_by, reason) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET started_at = now(), started_by = EXCLUDED.started_by, reason = EXCLUDED.reason
		RETURNING started_at`, workspaceID, by, reason).Scan(&at); err != nil {
		return Lockdown{}, fmt.Errorf("economy: lockdown: %w", err)
	}
	l.StartedAt = &at
	return l, tx.Commit(ctx)
}

// allIn refuses with notFound unless every one of ids is a row count(q) finds in workspaceID.
func allIn(ctx context.Context, tx pgx.Tx, q, workspaceID string, ids []string, notFound error) error {
	if len(ids) == 0 {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, q, workspaceID, ids).Scan(&n); err != nil {
		return fmt.Errorf("economy: lockdown: %w", err)
	}
	if n != len(slices.Compact(slices.Sorted(slices.Values(ids)))) {
		return notFound
	}
	return nil
}

func collectIDs(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("economy: lockdown: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("economy: lockdown: %w", err)
	}
	return append([]string{}, ids...), nil
}

// LockdownStatus reads whether workspaceID is locked down, and since when.
func (s *DualTokenStore) LockdownStatus(ctx context.Context, workspaceID string) (Lockdown, error) {
	l := Lockdown{RevokedDevices: []string{}, RevokedPasskeys: []string{}}
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT started_at, started_by, reason FROM workspace_lockdowns WHERE workspace_id = $1`,
		workspaceID).Scan(&at, &l.StartedBy, &l.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, nil
	}
	if err != nil {
		return l, fmt.Errorf("economy: lockdown: %w", err)
	}
	l.Active, l.StartedAt = true, &at
	return l, nil
}

// refuseIfLockedDown is ErrLockedDown while workspaceID is locked down.
func refuseIfLockedDown(ctx context.Context, q pgxDB, workspaceID string) error {
	var locked bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_lockdowns WHERE workspace_id = $1)`, workspaceID).Scan(&locked); err != nil {
		return fmt.Errorf("economy: lockdown: %w", err)
	}
	if locked {
		return ErrLockedDown
	}
	return nil
}

// LiftLockdown lifts workspaceID's lockdown once an identity check has passed since it began. It restarts nothing:
// each agent and card is then resumed or unfrozen on its own, as before.
func (s *DualTokenStore) LiftLockdown(ctx context.Context, workspaceID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: lockdown: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var started time.Time
	err = tx.QueryRow(ctx, `SELECT started_at FROM workspace_lockdowns WHERE workspace_id = $1 FOR UPDATE`, workspaceID).Scan(&started)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: lockdown: %w", err)
	}
	checks, err := readVerificationChecks(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	synthetic, err := syntheticWorkspace(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	// The re-check is of the person checked before the lockdown, when there was one: a thief passes only as themself.
	var before []string
	for _, c := range checks {
		if c.Level == LevelIdentity && c.Status == "completed" && !c.StartedAt.After(started) {
			before = append(before, c.VerifiedName)
		}
	}
	if !slices.ContainsFunc(checks, func(c VerificationCheck) bool {
		return c.Level == LevelIdentity && c.Status == "completed" && c.StartedAt.After(started) && (!c.Test || synthetic) &&
			(len(before) == 0 || slices.ContainsFunc(before, func(n string) bool { return strings.EqualFold(n, c.VerifiedName) }))
	}) {
		return ErrIdentityRecheck
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_lockdowns WHERE workspace_id = $1`, workspaceID); err != nil {
		return fmt.Errorf("economy: lockdown: %w", err)
	}
	return tx.Commit(ctx)
}
