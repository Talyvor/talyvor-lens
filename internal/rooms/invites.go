package rooms

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
)

// B32.29 — a workspace joins a private room through an invite (migration 0211).
//
// A link invite is an unguessable token with an expiry and a use count, made by the owner or an editor: it opens the
// room's title and terms to whoever holds it, and admits workspaces until it is revoked, expires or is used up — then
// it answers 404, as a room that does not exist does. Lens keeps only the token's SHA-256, so the token is shown once,
// when the invite is made. A named invite is the owner naming one workspace: that workspace sees the room, reads its
// terms and joins it once with POST /v1/rooms/{id}/join. Each join that makes a new member uses its invite, and every
// join is still within the room owner's members_per_room.

// tokenPrefix marks a room invite token, so a pasted one is recognisable.
const tokenPrefix = "rinvtok_"

// Invite is a room invite as its room's owner and editors see it. Token is set only in the answer that made it.
type Invite struct {
	ID            string     `json:"id"`
	RoomID        string     `json:"room_id"`
	Kind          string     `json:"kind"`                   // link or named
	WorkspaceID   string     `json:"workspace_id,omitempty"` // the workspace a named invite names
	WorkspaceName string     `json:"workspace_name,omitempty"`
	Token         string     `json:"token,omitempty"`
	MaxUses       int        `json:"max_uses"`
	Uses          int        `json:"uses"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	Live          bool       `json:"live"` // not revoked, expired or used up
	CreatedBy     string     `json:"created_by_workspace_id"`
	CreatedAt     time.Time  `json:"created_at"`
}

// InviteDraft is POST /v1/rooms/{id}/invites: either workspace_id, naming one workspace, or a link's max_uses and
// expires_at — the link has no default for either.
type InviteDraft struct {
	WorkspaceID string     `json:"workspace_id"`
	MaxUses     int        `json:"max_uses"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// InvitePreview is what a live link opens: the room, its current terms, and what is left of the link.
type InvitePreview struct {
	Room      Room      `json:"room"`
	Terms     Terms     `json:"terms"`
	ExpiresAt time.Time `json:"expires_at"`
	UsesLeft  int       `json:"uses_left"`
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

var inviteCols = `i.id, i.room_id, i.workspace_id, i.max_uses, i.uses, i.expires_at, i.revoked_at,
	i.revoked_at IS NULL AND i.uses < i.max_uses AND (i.expires_at IS NULL OR i.expires_at > now()),
	i.created_by_workspace_id, i.created_at, ` + workspaceName("i.workspace_id")

func scanInvite(row pgx.Row) (Invite, error) {
	var i Invite
	err := row.Scan(&i.ID, &i.RoomID, &i.WorkspaceID, &i.MaxUses, &i.Uses, &i.ExpiresAt, &i.RevokedAt, &i.Live,
		&i.CreatedBy, &i.CreatedAt, &i.WorkspaceName)
	i.Kind = "link"
	if i.WorkspaceID != "" {
		i.Kind = "named"
	}
	return i, err
}

// manager locks the private room roomID and answers actor's membership, which must be the owner's or an editor's.
func manager(ctx context.Context, tx pgx.Tx, actor, roomID string) (Room, Member, error) {
	r, err := lockRoom(ctx, tx, actor, roomID)
	if err != nil {
		return r, Member{}, err
	}
	me, ok, err := member(ctx, tx, roomID, actor)
	if err != nil {
		return r, me, err
	}
	if !ok || (me.Role != RoleOwner && me.Role != RoleEditor) {
		return r, me, forbidden("only the room's owner or an editor manages its invites")
	}
	return r, me, nil
}

// CreateInvite makes an invite to the private room, as actor: a link by its owner or an editor, or the owner naming
// one workspace. created is false when that workspace already had a live named invite, which is answered instead.
func (s *Store) CreateInvite(ctx context.Context, actor, roomID string, d InviteDraft) (inv Invite, created bool, err error) {
	d.WorkspaceID = strings.TrimSpace(d.WorkspaceID)
	named := d.WorkspaceID != ""
	switch {
	case named && (d.MaxUses != 0 || d.ExpiresAt != nil):
		return inv, false, invalid("an invite names a workspace, or is a link with max_uses and expires_at — not both")
	case !named && d.MaxUses < 1:
		return inv, false, invalid("a link invite needs max_uses, how many workspaces it may admit (at least 1)")
	case !named && d.MaxUses > math.MaxInt32:
		return inv, false, invalid("max_uses must be at most %d", math.MaxInt32)
	case !named && d.ExpiresAt == nil:
		return inv, false, invalid("a link invite needs expires_at, when it stops admitting anyone")
	case !named && !d.ExpiresAt.After(time.Now()):
		return inv, false, invalid("expires_at must be in the future")
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, me, err := manager(ctx, tx, actor, roomID)
		if err != nil {
			return err
		}
		if r.Visibility != Private {
			return invalid("anyone may join a public room; invites are for private rooms")
		}
		if r.Status == Closed {
			return fmt.Errorf("%w: the room is closed", ErrConflict)
		}
		id := "rinv_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if named {
			if me.Role != RoleOwner {
				return forbidden("only the room's owner names a workspace to join it; an editor makes an invite link")
			}
			if _, already, err := member(ctx, tx, roomID, d.WorkspaceID); err != nil {
				return err
			} else if already {
				return fmt.Errorf("%w: that workspace is already a member of this room", ErrConflict)
			}
			inv, err = scanInvite(tx.QueryRow(ctx, `SELECT `+inviteCols+` FROM room_invites i WHERE i.room_id = $1
				AND i.workspace_id = $2 AND i.revoked_at IS NULL AND i.uses < i.max_uses ORDER BY i.created_at LIMIT 1`, roomID, d.WorkspaceID))
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			created = true
			inv, err = scanInvite(tx.QueryRow(ctx, `INSERT INTO room_invites AS i (id, room_id, workspace_id, max_uses, created_by_workspace_id)
				VALUES ($1, $2, $3, 1, $4) RETURNING `+inviteCols, id, roomID, d.WorkspaceID, actor))
			return err
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		token := tokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
		created = true
		inv, err = scanInvite(tx.QueryRow(ctx, `INSERT INTO room_invites AS i (id, room_id, token_sha256, max_uses, expires_at, created_by_workspace_id)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+inviteCols, id, roomID, hashToken(token), d.MaxUses, *d.ExpiresAt, actor))
		inv.Token = token
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrConflict) {
		err = fmt.Errorf("rooms: invite: %w", err)
	}
	return inv, created, err
}

// Invites is the room's invites, newest first, as its owner or an editor sees them: never a link's token.
func (s *Store) Invites(ctx context.Context, actor, roomID string) ([]Invite, error) {
	out := []Invite{}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, _, err := manager(ctx, tx, actor, roomID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+inviteCols+` FROM room_invites i WHERE i.room_id = $1
			ORDER BY i.created_at DESC, i.id LIMIT $2`, roomID, listLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvite(rows)
			if err != nil {
				return err
			}
			out = append(out, inv)
		}
		return rows.Err()
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
		err = fmt.Errorf("rooms: invites: %w", err)
	}
	return out, err
}

// RevokeInvite stops one of the room's invites admitting anyone, as its owner or an editor. Revoking it again
// changes nothing.
func (s *Store) RevokeInvite(ctx context.Context, actor, roomID, inviteID string) (Invite, error) {
	var inv Invite
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, _, err := manager(ctx, tx, actor, roomID); err != nil {
			return err
		}
		var err error
		inv, err = scanInvite(tx.QueryRow(ctx, `UPDATE room_invites AS i SET revoked_at = COALESCE(i.revoked_at, now())
			WHERE i.id = $1 AND i.room_id = $2 RETURNING `+inviteCols, inviteID, roomID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such invite in this room", ErrNotFound)
		}
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
		err = fmt.Errorf("rooms: revoke invite: %w", err)
	}
	return inv, err
}

// liveLink answers the live link invite whose token is token, locked for update when lock is set.
func liveLink(ctx context.Context, tx pgx.Tx, token string, lock bool) (Invite, error) {
	q := `SELECT ` + inviteCols + ` FROM room_invites i WHERE i.token_sha256 = $1 AND i.workspace_id = ''`
	if lock {
		q += ` FOR UPDATE`
	}
	inv, err := scanInvite(tx.QueryRow(ctx, q, hashToken(token)))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !inv.Live) {
		return inv, fmt.Errorf("%w: no such invite", ErrNotFound)
	}
	return inv, err
}

// PreviewInvite is what a live invite link opens: the room's title and terms, before its holder joins.
func (s *Store) PreviewInvite(ctx context.Context, token string) (InvitePreview, error) {
	var p InvitePreview
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		inv, err := liveLink(ctx, tx, token, false)
		if err != nil {
			return err
		}
		p.ExpiresAt, p.UsesLeft = *inv.ExpiresAt, inv.MaxUses-inv.Uses
		if p.Room, err = scanRoom(tx.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms r WHERE r.id = $1`, inv.RoomID)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT version, split_rule, remix_share_bps, default_price_usd_micros, spend_policy, created_at
			FROM room_terms WHERE room_id = $1 AND version = $2`, inv.RoomID, p.Room.TermsVersion).Scan(&p.Terms.Version,
			&p.Terms.SplitRule, &p.Terms.RemixShareBPS, &p.Terms.DefaultPriceUSDMicros, &p.Terms.SpendPolicy, &p.Terms.CreatedAt)
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		err = fmt.Errorf("rooms: invite: %w", err)
	}
	return p, err
}

// JoinByInvite makes ws a member of the invite link's room under the terms version it accepted, as Join does, and
// uses the link once when ws is a new member. A link that is revoked, expired or used up is ErrNotFound.
func (s *Store) JoinByInvite(ctx context.Context, ws, user, token string, termsVersion int) (m Member, roomID string, created bool, err error) {
	if termsVersion < 1 {
		return m, "", false, invalid("joining accepts the room's terms: send the terms_version the invite showed you")
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The room is locked before the invite, the order every other path takes them in.
		inv, err := liveLink(ctx, tx, token, false)
		if err != nil {
			return err
		}
		r, err := lockRoomAny(ctx, tx, inv.RoomID)
		if err != nil {
			return err
		}
		if inv, err = liveLink(ctx, tx, token, true); err != nil {
			return err
		}
		roomID = r.ID
		if m, created, err = s.admit(ctx, tx, r, ws, user, termsVersion); err != nil || !created {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE room_invites SET uses = uses + 1 WHERE id = $1`, inv.ID)
		return err
	})
	return m, roomID, created, joinErr(err)
}

// Invited is the private rooms whose owner named ws in an invite still live, and that ws has not joined yet.
func (s *Store) Invited(ctx context.Context, ws string) ([]Room, error) {
	if ws == "" {
		return []Room{}, nil
	}
	rooms, err := collectRooms(s.pool.Query(ctx, `SELECT `+roomCols+` FROM rooms r
		WHERE r.status <> 'closed' AND EXISTS (SELECT 1 FROM room_invites i WHERE i.room_id = r.id AND i.workspace_id = $1
			AND i.revoked_at IS NULL AND i.uses < i.max_uses)
		AND NOT EXISTS (SELECT 1 FROM room_members m WHERE m.room_id = r.id AND m.workspace_id = $1 AND m.removed_at IS NULL)
		ORDER BY r.last_activity_at DESC, r.id LIMIT $2`, ws, listLimit))
	if err != nil {
		return nil, fmt.Errorf("rooms: invited: %w", err)
	}
	return rooms, nil
}
