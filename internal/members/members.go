// Package members is B52.35: a person joins another workspace through its owner's invite link, lists the workspaces
// they are in, and is removed by the owner (migration 0324).
//
// A workspace's owner is the identity POST /v1/provision derives it from; a member is any other identity that accepted
// one of the owner's invite links. An invite link is an unguessable token with an expiry and a use count, kept only as
// its SHA-256 as B32.29's room invites are: it admits identities until it is revoked, expires or is used up, and then
// answers ErrNotFound, as an invite that never existed does.
package members

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// tokenPrefix marks a workspace invite token, so a pasted one is recognisable.
const tokenPrefix = "wsinvtok_"

// listLimit caps every list this package answers.
const listLimit = 200

// Invite is a workspace invite link as the workspace's owner sees it. Token is set only in the answer that made it.
type Invite struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Token       string     `json:"token,omitempty"`
	MaxUses     int        `json:"max_uses"`
	Uses        int        `json:"uses"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Live        bool       `json:"live"` // not revoked, expired or used up
	CreatedAt   time.Time  `json:"created_at"`
}

// Member is a live member of a workspace.
type Member struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Identity    string    `json:"identity"`
	Role        string    `json:"role"`
	AddedAt     time.Time `json:"added_at"`
}

// Workspace is one workspace a person is in, with their role in it.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"` // owner or member
}

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

const inviteCols = `id, workspace_id, max_uses, uses, expires_at, revoked_at,
	revoked_at IS NULL AND uses < max_uses AND expires_at > now(), created_at`

func scanInvite(row pgx.Row) (Invite, error) {
	var i Invite
	err := row.Scan(&i.ID, &i.WorkspaceID, &i.MaxUses, &i.Uses, &i.ExpiresAt, &i.RevokedAt, &i.Live, &i.CreatedAt)
	return i, err
}

const memberCols = `id, workspace_id, identity, role, added_at`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.WorkspaceID, &m.Identity, &m.Role, &m.AddedAt)
	return m, err
}

// CreateInvite makes an invite link into ws admitting up to maxUses people until expiresAt. Neither has a default.
func (s *Store) CreateInvite(ctx context.Context, ws string, maxUses int, expiresAt *time.Time) (Invite, error) {
	switch {
	case maxUses < 1 || maxUses > math.MaxInt32:
		return Invite{}, fmt.Errorf("%w: max_uses, how many people the link may admit, must be between 1 and %d", ErrInvalid, math.MaxInt32)
	case expiresAt == nil || !expiresAt.After(time.Now()):
		return Invite{}, fmt.Errorf("%w: expires_at, when the link stops admitting anyone, must be in the future", ErrInvalid)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Invite{}, err
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	inv, err := scanInvite(s.pool.QueryRow(ctx, `INSERT INTO workspace_member_invites (id, workspace_id, token_sha256, max_uses, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+inviteCols,
		"wsinv_"+strings.ReplaceAll(uuid.NewString(), "-", ""), ws, hashToken(token), maxUses, *expiresAt))
	if err != nil {
		return Invite{}, fmt.Errorf("members: invite: %w", err)
	}
	inv.Token = token
	return inv, nil
}

// Invites is ws's invite links, newest first: never a token.
func (s *Store) Invites(ctx context.Context, ws string) ([]Invite, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+inviteCols+` FROM workspace_member_invites WHERE workspace_id = $1
		ORDER BY created_at DESC, id LIMIT $2`, ws, listLimit)
	if err != nil {
		return nil, fmt.Errorf("members: invites: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invite, error) { return scanInvite(r) })
}

// RevokeInvite stops one of ws's invite links admitting anyone. Revoking it again changes nothing.
func (s *Store) RevokeInvite(ctx context.Context, ws, id string) (Invite, error) {
	inv, err := scanInvite(s.pool.QueryRow(ctx, `UPDATE workspace_member_invites SET revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1 AND workspace_id = $2 RETURNING `+inviteCols, id, ws))
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, fmt.Errorf("%w: no such invite in this workspace", ErrNotFound)
	}
	return inv, err
}

// Members is ws's live members, oldest first.
func (s *Store) Members(ctx context.Context, ws string) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+memberCols+` FROM workspace_members WHERE workspace_id = $1 AND removed_at IS NULL
		ORDER BY added_at, id LIMIT $2`, ws, listLimit)
	if err != nil {
		return nil, fmt.Errorf("members: list: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) { return scanMember(r) })
}

// RemoveMember ends one live membership of ws.
func (s *Store) RemoveMember(ctx context.Context, ws, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE workspace_members SET removed_at = now() WHERE id = $1 AND workspace_id = $2 AND removed_at IS NULL`, id, ws)
	if err != nil {
		return fmt.Errorf("members: remove: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such member in this workspace", ErrNotFound)
	}
	return nil
}

// Accept makes identity a member of the invite link's workspace and uses the link once; created is false, and the
// link is not used, when identity already was one. ownWS is the workspace identity owns, which it cannot join. A link
// that is revoked, expired or used up is ErrNotFound and adds nobody.
func (s *Store) Accept(ctx context.Context, identity, ownWS, token string) (m Member, created bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		inv, err := scanInvite(tx.QueryRow(ctx, `SELECT `+inviteCols+` FROM workspace_member_invites WHERE token_sha256 = $1 FOR UPDATE`,
			hashToken(token)))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !inv.Live) {
			return fmt.Errorf("%w: no such invite", ErrNotFound)
		}
		if err != nil {
			return err
		}
		if inv.WorkspaceID == ownWS {
			return fmt.Errorf("%w: this is your own workspace", ErrConflict)
		}
		m, err = scanMember(tx.QueryRow(ctx, `INSERT INTO workspace_members (id, workspace_id, identity)
			VALUES ($1, $2, $3) ON CONFLICT (workspace_id, identity) WHERE removed_at IS NULL DO NOTHING RETURNING `+memberCols,
			"wsm_"+strings.ReplaceAll(uuid.NewString(), "-", ""), inv.WorkspaceID, identity))
		if errors.Is(err, pgx.ErrNoRows) {
			m, err = scanMember(tx.QueryRow(ctx, `SELECT `+memberCols+` FROM workspace_members
				WHERE workspace_id = $1 AND identity = $2 AND removed_at IS NULL`, inv.WorkspaceID, identity))
			return err
		}
		if err != nil {
			return err
		}
		created = true
		_, err = tx.Exec(ctx, `UPDATE workspace_member_invites SET uses = uses + 1 WHERE id = $1`, inv.ID)
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
		err = fmt.Errorf("members: accept: %w", err)
	}
	return m, created, err
}

// IsMember reports whether identity is a live member of ws.
func (s *Store) IsMember(ctx context.Context, ws, identity string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_members WHERE workspace_id = $1 AND identity = $2
		AND removed_at IS NULL)`, ws, identity).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("members: is member: %w", err)
	}
	return ok, nil
}

// Workspaces is every workspace identity is in: ownWS first when it exists, then those it is a member of, oldest
// membership first.
func (s *Store) Workspaces(ctx context.Context, identity, ownWS string) ([]Workspace, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, role FROM (
			SELECT id, name, 'owner' AS role, 0 AS ord, NULL::timestamptz AS added_at, '' AS mid FROM workspaces WHERE id = $1
			UNION ALL
			SELECT w.id, w.name, 'member', 1, m.added_at, m.id FROM workspace_members m JOIN workspaces w ON w.id = m.workspace_id
				WHERE m.identity = $2 AND m.removed_at IS NULL
		) x ORDER BY ord, added_at, mid LIMIT $3`, ownWS, identity, listLimit)
	if err != nil {
		return nil, fmt.Errorf("members: workspaces: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Workspace, error) {
		var w Workspace
		return w, r.Scan(&w.ID, &w.Name, &w.Role)
	})
}
