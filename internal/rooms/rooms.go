// Package rooms is B32's rooms: open chats where people and their agents build a product together (migration 0210).
//
// A room belongs to the workspace that created it, which joins it as its owner. Another workspace joins by accepting
// the room's terms at their current version — how a sale of the room's work is split, the share a fork pays its
// original, a contribution's default price and who may spend the room's budget. When the owner changes the terms
// they get a new version; each member keeps the version it accepted and is asked again before its next contribution.
//
// Roles are owner, editor, member and viewer. The owner or an editor changes a member's role and may_spend or removes
// it; only the owner makes or unmakes an editor, and nobody changes the owner. A viewer is never given may_spend. A
// member's agents join only as that member's, and leave with it. A removed member's row stays, with removed_at, so
// what it contributed stays its own.
//
// How many rooms a workspace opens, and how many members and agents each holds, is its plan's rooms_plan_limits
// (limits.go). A private room is listed and shown to its members only; a workspace joins one through an invite link
// or by being named by its owner (invites.go). Members post messages, which a public room's scan reads first and every
// member's event stream receives (messages.go). Members propose work as contributions, fork each other's with lineage
// and vote on them, and the owner or an editor accepts or rejects them (contributions.go). A room has a wallet, whose
// monthly limit is its budget, spent by its owner and the members it lets spend (wallet.go).
package rooms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/market"
)

var (
	// ErrInvalid wraps every reason a request about a room is not well formed.
	ErrInvalid = errors.New("rooms: invalid request")
	// ErrNotFound: no such room (or member, or agent) this caller may see.
	ErrNotFound = errors.New("rooms: not found")
	// ErrForbidden: the caller may see the room but not do this in it.
	ErrForbidden = errors.New("rooms: not allowed")
	// ErrConflict: the room is not open, or the terms accepted are not the room's current ones.
	ErrConflict = errors.New("rooms: conflict")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func forbidden(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, args...))
}

// Roles, visibilities, statuses, split rules and spend policies, as migration 0210 checks them.
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleMember = "member"
	RoleViewer = "viewer"

	Public  = "public"
	Private = "private"

	Open   = "open"
	Locked = "locked"
	Closed = "closed"

	SplitOwnerDecides = "owner_decides"
	SplitEqual        = "equal"
	SplitByVotes      = "by_votes"

	SpendOwnerOnly        = "owner_only"
	SpendMembersWithSpend = "members_with_spend"
)

const (
	maxTitle       = 120
	maxTopic       = 60
	maxDescription = 4000
	// listLimit bounds GET /v1/rooms.
	listLimit = 100
)

// Terms is one version of what a member accepts by joining.
type Terms struct {
	Version               int       `json:"version"`
	SplitRule             string    `json:"split_rule"`
	RemixShareBPS         int       `json:"remix_share_bps"`
	DefaultPriceUSDMicros int64     `json:"default_price_usd_micros"`
	SpendPolicy           string    `json:"spend_policy"`
	CreatedAt             time.Time `json:"created_at"`
}

// Room is a room as listed.
type Room struct {
	ID               string    `json:"id"`
	OwnerWorkspaceID string    `json:"owner_workspace_id"`
	Title            string    `json:"title"`
	Topic            string    `json:"topic"`
	Description      string    `json:"description"`
	Visibility       string    `json:"visibility"`
	Status           string    `json:"status"`
	TermsVersion     int       `json:"terms_version"`
	MemberCount      int       `json:"member_count"`
	CreatedAt        time.Time `json:"created_at"`
	LastActivityAt   time.Time `json:"last_activity_at"`
}

// Member is a workspace's membership of a room.
type Member struct {
	WorkspaceID  string     `json:"workspace_id"`
	UserID       string     `json:"user_id,omitempty"` // shown to its own workspace (and an admin) only
	Role         string     `json:"role"`
	MaySpend     bool       `json:"may_spend"`
	TermsVersion int        `json:"terms_version"`
	TermsCurrent bool       `json:"terms_current"` // false: the room's terms changed since it accepted them
	JoinedAt     time.Time  `json:"joined_at"`
	RemovedAt    *time.Time `json:"removed_at,omitempty"`
	MutedAt      *time.Time `json:"muted_at,omitempty"`  // muted: it reads the room and posts nothing (B32.52)
	BannedAt     *time.Time `json:"banned_at,omitempty"` // banned: removed, and does not join again until unbanned
}

// Agent is an agent in a room, there as its workspace's.
type Agent struct {
	AgentID     string    `json:"agent_id"`
	Name        string    `json:"name"`
	WorkspaceID string    `json:"workspace_id"`
	JoinedAt    time.Time `json:"joined_at"`
}

// Detail is GET /v1/rooms/{id}: the room, its current terms, its members and agents, and the caller's membership.
type Detail struct {
	Room
	Terms   Terms    `json:"terms"`
	Members []Member `json:"members"`
	Agents  []Agent  `json:"agents"`
	Me      *Member  `json:"me"` // nil: the caller is not a member
	// Wallet is the room's budget, shown to its members (B32.32); nil to anyone else, or while the room has none.
	Wallet *Wallet `json:"wallet,omitempty"`
	// UnderReview: a public room off the public list, reported by LENS_ROOM_REPORTS_HIDE workspaces, until the operator
	// reviews it (B32.52).
	UnderReview bool `json:"under_review,omitempty"`
	// ForkedFromRoomID is the room this one was forked from while its owner was away (B41.148); nil, none.
	ForkedFromRoomID *string `json:"forked_from_room_id,omitempty"`
}

// Draft is POST /v1/workspaces/{ws}/rooms.
type Draft struct {
	Title       string     `json:"title"`
	Topic       string     `json:"topic"`
	Description string     `json:"description"`
	Visibility  string     `json:"visibility"` // public (default) or private
	Terms       TermsDraft `json:"terms"`
}

// TermsDraft is the terms a room is created with, or changed to.
type TermsDraft struct {
	SplitRule             string `json:"split_rule"`      // owner_decides (default), equal or by_votes
	RemixShareBPS         int    `json:"remix_share_bps"` // 0 to LENS_LINEAGE_MAX_SHARE_BPS
	DefaultPriceUSDMicros int64  `json:"default_price_usd_micros"`
	SpendPolicy           string `json:"spend_policy"` // owner_only (default) or members_with_spend
}

// MemberChange is PATCH /v1/rooms/{id}/members/{ws}; a nil field is left as it is. Muted and Banned are sent alone.
type MemberChange struct {
	Role     *string `json:"role"`
	MaySpend *bool   `json:"may_spend"`
	Remove   bool    `json:"remove"`
	Muted    *bool   `json:"muted"`  // B32.52: mute or unmute a member
	Banned   *bool   `json:"banned"` // B32.52: ban a member — it is removed — or unban it
}

// Store reads and writes rooms.
type Store struct {
	pool      *pgxpool.Pool
	maxShare  int
	limits    map[string]Limits
	perMinute int           // LENS_ROOM_MESSAGES_PER_MINUTE (messages.go)
	market    *market.Store // where contributions are published (contributions.go)
	keys      Keys          // issues each room wallet's key (wallet.go); nil, rooms have no wallet

	contextMessages int // LENS_ROOM_CONTEXT_MESSAGES (runs.go)
	reportsHideAt   int // LENS_ROOM_REPORTS_HIDE (reports.go)
}

// NewStore answers a Store whose rooms may ask a fork for at most maxShareBPS of its sales (LENS_LINEAGE_MAX_SHARE_BPS),
// under the rooms_plan_limits this process runs with.
func NewStore(pool *pgxpool.Pool, maxShareBPS int) *Store {
	return &Store{pool: pool, maxShare: maxShareBPS, limits: CurrentLimits()}
}

func (s *Store) checkTerms(d TermsDraft) (TermsDraft, error) {
	if d.SplitRule == "" {
		d.SplitRule = SplitOwnerDecides
	}
	if d.SpendPolicy == "" {
		d.SpendPolicy = SpendOwnerOnly
	}
	switch d.SplitRule {
	case SplitOwnerDecides, SplitEqual, SplitByVotes:
	default:
		return d, invalid("split_rule must be owner_decides, equal or by_votes")
	}
	switch d.SpendPolicy {
	case SpendOwnerOnly, SpendMembersWithSpend:
	default:
		return d, invalid("spend_policy must be owner_only or members_with_spend")
	}
	if d.RemixShareBPS < 0 || d.RemixShareBPS > s.maxShare {
		return d, invalid("remix_share_bps must be 0 to %d (LENS_LINEAGE_MAX_SHARE_BPS)", s.maxShare)
	}
	if d.DefaultPriceUSDMicros < 0 {
		return d, invalid("default_price_usd_micros cannot be negative")
	}
	return d, nil
}

func checkText(name, v string, max int, required bool) (string, error) {
	v = strings.TrimSpace(v)
	if required && v == "" {
		return v, invalid("a room needs a %s", name)
	}
	if utf8.RuneCountInString(v) > max {
		return v, invalid("the %s must be at most %d characters", name, max)
	}
	return v, nil
}

// Create opens a room owned by ws, with user as the owner's member, at terms version 1.
func (s *Store) Create(ctx context.Context, ws, user string, d Draft) (Detail, error) {
	var err error
	if d.Title, err = checkText("title", d.Title, maxTitle, true); err != nil {
		return Detail{}, err
	}
	if d.Topic, err = checkText("topic", d.Topic, maxTopic, false); err != nil {
		return Detail{}, err
	}
	if d.Description, err = checkText("description", d.Description, maxDescription, false); err != nil {
		return Detail{}, err
	}
	if d.Visibility == "" {
		d.Visibility = Public
	}
	if d.Visibility != Public && d.Visibility != Private {
		return Detail{}, invalid("visibility must be public or private")
	}
	if d.Terms, err = s.checkTerms(d.Terms); err != nil {
		return Detail{}, err
	}
	id := "room_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// B32.32: the room's wallet spends with a key of the owner's workspace, issued first because its store is not this
	// transaction's; it is revoked again when the room is not opened.
	keyID := ""
	if s.keys != nil {
		if keyID, err = s.issueWalletKey(ctx, ws, d.Title); err != nil {
			return Detail{}, err
		}
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// One room at a time per owner, so two creates cannot both take its last room.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rooms:create:' || $1))`, ws); err != nil {
			return err
		}
		p, err := s.LimitsOf(ctx, tx, ws)
		if err != nil {
			return err
		}
		var owned int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM rooms WHERE owner_workspace_id = $1 AND visibility = $2 AND status <> 'closed'`,
			ws, d.Visibility).Scan(&owned); err != nil {
			return err
		}
		if err := s.refuse(p, d.Visibility+"_rooms", "your", owned); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO rooms (id, owner_workspace_id, title, topic, description, visibility)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, ws, d.Title, d.Topic, d.Description, d.Visibility); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_terms (room_id, version, split_rule, remix_share_bps, default_price_usd_micros, spend_policy)
			VALUES ($1, 1, $2, $3, $4, $5)`, id, d.Terms.SplitRule, d.Terms.RemixShareBPS, d.Terms.DefaultPriceUSDMicros, d.Terms.SpendPolicy); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO room_members (room_id, workspace_id, user_id, role, may_spend, terms_version)
			VALUES ($1, $2, $3, 'owner', true, 1)`, id, ws, user); err != nil || keyID == "" {
			return err
		}
		return openWallet(ctx, tx, id, ws, user, d.Title, keyID)
	})
	if err != nil && keyID != "" {
		s.revokeWalletKey(ctx, keyID)
	}
	if errors.Is(err, ErrPlanLimit) {
		return Detail{}, err
	}
	if err != nil {
		return Detail{}, fmt.Errorf("rooms: create: %w", err)
	}
	return s.Get(ctx, ws, false, id)
}

const roomCols = `r.id, r.owner_workspace_id, r.title, r.topic, r.description, r.visibility, r.status, r.terms_version,
	(SELECT count(*) FROM room_members m WHERE m.room_id = r.id AND m.removed_at IS NULL), r.created_at, r.last_activity_at`

// scanRoom scans roomCols, then any more columns the query selects into more.
func scanRoom(row pgx.Row, more ...any) (Room, error) {
	var r Room
	err := row.Scan(append([]any{&r.ID, &r.OwnerWorkspaceID, &r.Title, &r.Topic, &r.Description, &r.Visibility, &r.Status,
		&r.TermsVersion, &r.MemberCount, &r.CreatedAt, &r.LastActivityAt}, more...)...)
	return r, err
}

func collectRooms(rows pgx.Rows, err error) ([]Room, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenPublic is the list of open chats: the open public rooms, latest activity first, of one topic when topic is set. A
// room reported by LENS_ROOM_REPORTS_HIDE workspaces is left off until the operator reviews it (reports.go).
func (s *Store) OpenPublic(ctx context.Context, topic string) ([]Room, error) {
	rooms, err := collectRooms(s.pool.Query(ctx, `SELECT `+roomCols+` FROM rooms r
		WHERE r.visibility = 'public' AND r.status = 'open' AND ($1 = '' OR lower(r.topic) = lower($1)) AND `+openReporters+` < $3
		ORDER BY r.last_activity_at DESC, r.id LIMIT $2`, strings.TrimSpace(topic), listLimit, s.reportsHide()))
	if err != nil {
		return nil, fmt.Errorf("rooms: list: %w", err)
	}
	return rooms, nil
}

// Joined is the rooms ws is a member of, latest activity first.
func (s *Store) Joined(ctx context.Context, ws string) ([]Room, error) {
	if ws == "" {
		return []Room{}, nil
	}
	rooms, err := collectRooms(s.pool.Query(ctx, `SELECT `+roomCols+` FROM rooms r
		JOIN room_members me ON me.room_id = r.id AND me.workspace_id = $1 AND me.removed_at IS NULL
		ORDER BY r.last_activity_at DESC, r.id LIMIT $2`, ws, listLimit))
	if err != nil {
		return nil, fmt.Errorf("rooms: joined: %w", err)
	}
	return rooms, nil
}

const memberCols = `m.workspace_id, m.user_id, m.role, m.may_spend, m.terms_version, m.terms_version = r.terms_version, m.joined_at, m.removed_at,
	m.muted_at, m.banned_at`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.WorkspaceID, &m.UserID, &m.Role, &m.MaySpend, &m.TermsVersion, &m.TermsCurrent, &m.JoinedAt, &m.RemovedAt,
		&m.MutedAt, &m.BannedAt)
	return m, err
}

// member answers ws's live membership of room, or ok false.
func member(ctx context.Context, q pgx.Tx, roomID, ws string) (m Member, ok bool, err error) {
	m, err = scanMember(q.QueryRow(ctx, `SELECT `+memberCols+` FROM room_members m JOIN rooms r ON r.id = m.room_id
		WHERE m.room_id = $1 AND m.workspace_id = $2 AND m.removed_at IS NULL`, roomID, ws))
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, false, nil
	}
	return m, err == nil, err
}

// mayEnter reports whether ws may see a private room: it is a member, or the owner named it in an invite still live.
func mayEnter(ctx context.Context, tx pgx.Tx, roomID, ws string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_members WHERE room_id = $1 AND workspace_id = $2 AND removed_at IS NULL)
		OR EXISTS (SELECT 1 FROM room_invites WHERE room_id = $1 AND workspace_id = $2 AND $2 <> ''
			AND revoked_at IS NULL AND uses < max_uses)`, roomID, ws).Scan(&ok)
	return ok, err
}

// lockRoomAny reads the room for update, whoever may see it.
func lockRoomAny(ctx context.Context, tx pgx.Tx, roomID string) (Room, error) {
	r, err := scanRoom(tx.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms r WHERE r.id = $1 FOR UPDATE`, roomID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: no such room", ErrNotFound)
	}
	return r, err
}

// lockRoom reads the room for update and answers ErrNotFound when viewer may not see it: a private room is seen by its
// members, and by a workspace its owner named.
func lockRoom(ctx context.Context, tx pgx.Tx, viewer, roomID string) (Room, error) {
	r, err := lockRoomAny(ctx, tx, roomID)
	if err != nil {
		return r, err
	}
	if r.Visibility == Private {
		if ok, err := mayEnter(ctx, tx, roomID, viewer); err != nil {
			return r, err
		} else if !ok {
			return r, fmt.Errorf("%w: no such room", ErrNotFound)
		}
	}
	return r, nil
}

func touch(ctx context.Context, tx pgx.Tx, roomID string) error {
	_, err := tx.Exec(ctx, `UPDATE rooms SET last_activity_at = now() WHERE id = $1`, roomID)
	return err
}

// Get answers the room as viewer sees it. A room opened before rooms had wallets gets its wallet when its owner reads it.
func (s *Store) Get(ctx context.Context, viewer string, admin bool, roomID string) (Detail, error) {
	d, err := s.get(ctx, viewer, admin, roomID)
	if err == nil && d.Wallet == nil && d.Me != nil && d.Me.Role == RoleOwner && s.keys != nil {
		if err := s.ensureWallet(ctx, roomID); err != nil {
			return Detail{}, err
		}
		return s.get(ctx, viewer, admin, roomID)
	}
	return d, err
}

func (s *Store) get(ctx context.Context, viewer string, admin bool, roomID string) (Detail, error) {
	var d Detail
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var wallet *string
		var reporters int
		r, err := scanRoom(tx.QueryRow(ctx, `SELECT `+roomCols+`, r.wallet_agent_id, `+openReporters+`, r.forked_from_room_id FROM rooms r WHERE r.id = $1`, roomID),
			&wallet, &reporters, &d.ForkedFromRoomID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such room", ErrNotFound)
		}
		if err != nil {
			return err
		}
		me, isMember, err := member(ctx, tx, roomID, viewer)
		if err != nil {
			return err
		}
		if r.Visibility == Private && !isMember && !admin {
			if named, err := mayEnter(ctx, tx, roomID, viewer); err != nil {
				return err
			} else if !named {
				return fmt.Errorf("%w: no such room", ErrNotFound)
			}
		}
		d.Room = r
		d.UnderReview = r.Visibility == Public && r.Status == Open && reporters >= s.reportsHide()
		if isMember {
			d.Me = &me
		}
		if err := tx.QueryRow(ctx, `SELECT version, split_rule, remix_share_bps, default_price_usd_micros, spend_policy, created_at
			FROM room_terms WHERE room_id = $1 AND version = $2`, roomID, r.TermsVersion).Scan(&d.Terms.Version, &d.Terms.SplitRule,
			&d.Terms.RemixShareBPS, &d.Terms.DefaultPriceUSDMicros, &d.Terms.SpendPolicy, &d.Terms.CreatedAt); err != nil {
			return err
		}
		if wallet != nil && d.Me != nil {
			if d.Wallet, err = s.readWallet(ctx, tx, r, *wallet, d.Terms, me); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT `+memberCols+` FROM room_members m JOIN rooms r ON r.id = m.room_id
			WHERE m.room_id = $1 AND m.removed_at IS NULL
			ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'editor' THEN 1 WHEN 'member' THEN 2 ELSE 3 END, m.joined_at`, roomID)
		if err != nil {
			return err
		}
		d.Members = []Member{}
		for rows.Next() {
			m, err := scanMember(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if m.WorkspaceID != viewer && !admin {
				m.UserID = ""
			}
			d.Members = append(d.Members, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT a.agent_id, ag.name, a.workspace_id, a.joined_at FROM room_member_agents a
			JOIN agent_accounts ag ON ag.id = a.agent_id WHERE a.room_id = $1 ORDER BY a.joined_at, a.agent_id`, roomID)
		if err != nil {
			return err
		}
		defer rows.Close()
		d.Agents = []Agent{}
		for rows.Next() {
			var a Agent
			if err := rows.Scan(&a.AgentID, &a.Name, &a.WorkspaceID, &a.JoinedAt); err != nil {
				return err
			}
			d.Agents = append(d.Agents, a)
		}
		return rows.Err()
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Detail{}, err
		}
		return Detail{}, fmt.Errorf("rooms: get: %w", err)
	}
	return d, nil
}

// Join makes ws a member of the room under the terms version it accepted, which must be the room's current one. A
// member joining again accepts the current version; a removed member joining again is a member again. A private room
// takes a workspace its owner named; anyone else joins it through an invite link (JoinByInvite). created is false
// when ws was already a member.
func (s *Store) Join(ctx context.Context, ws, user, roomID string, termsVersion int) (m Member, created bool, err error) {
	if termsVersion < 1 {
		return Member{}, false, invalid("joining accepts the room's terms: send the terms_version you read from GET /v1/rooms/{id}")
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		m, created, err = s.admit(ctx, tx, r, ws, user, termsVersion)
		return err
	})
	return m, created, joinErr(err)
}

func joinErr(err error) error {
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInvalid) &&
		!errors.Is(err, ErrPlanLimit) {
		return fmt.Errorf("rooms: join: %w", err)
	}
	return err
}

// admit makes ws a member of r, locked for update, under termsVersion: r must be open, the version current, and a new
// member must fit within the room owner's members_per_room. A new member uses every invite naming it, however it
// came in, so none is left to let it back in once it is removed.
func (s *Store) admit(ctx context.Context, tx pgx.Tx, r Room, ws, user string, termsVersion int) (m Member, created bool, err error) {
	if r.Status != Open {
		return m, false, fmt.Errorf("%w: the room is %s and takes no new members", ErrConflict, r.Status)
	}
	if termsVersion != r.TermsVersion {
		return m, false, fmt.Errorf("%w: the room's terms are at version %d; read them and join with terms_version %d",
			ErrConflict, r.TermsVersion, r.TermsVersion)
	}
	var existed, isBanned bool
	if err := tx.QueryRow(ctx, `SELECT removed_at IS NULL, banned_at IS NOT NULL FROM room_members WHERE room_id = $1 AND workspace_id = $2`,
		r.ID, ws).Scan(&existed, &isBanned); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return m, false, err
	}
	if isBanned {
		return m, false, errBanned
	}
	if !existed {
		p, err := s.LimitsOf(ctx, tx, r.OwnerWorkspaceID)
		if err != nil {
			return m, false, err
		}
		// Counted now, under the room's lock: r's count was read before the lock was granted.
		var members int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM room_members WHERE room_id = $1 AND removed_at IS NULL`, r.ID).Scan(&members); err != nil {
			return m, false, err
		}
		if err := s.refuse(p, "members_per_room", "the room owner's", members); err != nil {
			return m, false, err
		}
	}
	// A returning member comes back as a member with no budget, whatever it was before it was removed.
	if _, err := tx.Exec(ctx, `INSERT INTO room_members (room_id, workspace_id, user_id, role, may_spend, terms_version)
		VALUES ($1, $2, $3, 'member', false, $4)
		ON CONFLICT (room_id, workspace_id) DO UPDATE SET terms_version = EXCLUDED.terms_version,
			user_id = CASE WHEN room_members.removed_at IS NULL THEN room_members.user_id ELSE EXCLUDED.user_id END,
			role = CASE WHEN room_members.removed_at IS NULL THEN room_members.role ELSE 'member' END,
			may_spend = room_members.may_spend AND room_members.removed_at IS NULL,
			joined_at = CASE WHEN room_members.removed_at IS NULL THEN room_members.joined_at ELSE now() END,
			removed_at = NULL`, r.ID, ws, user, termsVersion); err != nil {
		return m, false, err
	}
	if !existed {
		if _, err := tx.Exec(ctx, `UPDATE room_invites SET uses = max_uses WHERE room_id = $1 AND workspace_id = $2
			AND $2 <> '' AND revoked_at IS NULL AND uses < max_uses`, r.ID, ws); err != nil {
			return m, false, err
		}
	}
	if err := touch(ctx, tx, r.ID); err != nil {
		return m, false, err
	}
	m, _, err = member(ctx, tx, r.ID, ws)
	return m, !existed, err
}

// SetTerms gives the room a new terms version, written by its owner, who accepts it as it writes it. Terms the same
// as the current ones change nothing, so nobody is asked again for nothing.
func (s *Store) SetTerms(ctx context.Context, actor, roomID string, d TermsDraft) (Terms, error) {
	d, err := s.checkTerms(d)
	if err != nil {
		return Terms{}, err
	}
	var t Terms
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, actor, roomID)
		if err != nil {
			return err
		}
		if r.OwnerWorkspaceID != actor {
			return forbidden("only the room's owner changes its terms")
		}
		if err := shut(r); err != nil {
			return err
		}
		read := func(v int) error {
			return tx.QueryRow(ctx, `SELECT version, split_rule, remix_share_bps, default_price_usd_micros, spend_policy, created_at
				FROM room_terms WHERE room_id = $1 AND version = $2`, roomID, v).Scan(&t.Version, &t.SplitRule, &t.RemixShareBPS,
				&t.DefaultPriceUSDMicros, &t.SpendPolicy, &t.CreatedAt)
		}
		if err := read(r.TermsVersion); err != nil {
			return err
		}
		if t.SplitRule == d.SplitRule && t.RemixShareBPS == d.RemixShareBPS &&
			t.DefaultPriceUSDMicros == d.DefaultPriceUSDMicros && t.SpendPolicy == d.SpendPolicy {
			return nil
		}
		next := r.TermsVersion + 1
		if _, err := tx.Exec(ctx, `INSERT INTO room_terms (room_id, version, split_rule, remix_share_bps, default_price_usd_micros, spend_policy)
			VALUES ($1, $2, $3, $4, $5, $6)`, roomID, next, d.SplitRule, d.RemixShareBPS, d.DefaultPriceUSDMicros, d.SpendPolicy); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE rooms SET terms_version = $2, last_activity_at = now() WHERE id = $1`, roomID, next); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE room_members SET terms_version = $2 WHERE room_id = $1 AND role = 'owner'`, roomID, next); err != nil {
			return err
		}
		return read(next)
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrForbidden) {
		err = fmt.Errorf("rooms: terms: %w", err)
	}
	return t, err
}

// ChangeMember applies c to target's membership, as actor. The owner or an editor changes a member's role and
// may_spend or removes it; only the owner makes or unmakes an editor; nobody changes the owner; a viewer is never
// given may_spend, and a member made a viewer loses it. Any member but the owner may remove itself.
func (s *Store) ChangeMember(ctx context.Context, actor, roomID, target string, c MemberChange) (Member, error) {
	if c.Muted != nil || c.Banned != nil {
		if c.Role != nil || c.MaySpend != nil || c.Remove || (c.Muted != nil && c.Banned != nil) {
			return Member{}, invalid("mute or ban a member on its own, not with another change")
		}
		return s.moderateMember(ctx, actor, roomID, target, c)
	}
	if c.Role == nil && c.MaySpend == nil && !c.Remove {
		return Member{}, invalid("say what changes: role, may_spend, remove, muted or banned")
	}
	if c.Remove && (c.Role != nil || c.MaySpend != nil) {
		return Member{}, invalid("remove a member, or change its role or may_spend, not both")
	}
	if c.Role != nil {
		switch *c.Role {
		case RoleEditor, RoleMember, RoleViewer:
		case RoleOwner:
			return Member{}, invalid("a room has one owner, the workspace that created it")
		default:
			return Member{}, invalid("role must be editor, member or viewer")
		}
	}
	var out Member
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockRoom(ctx, tx, actor, roomID); err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, actor)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no such room", ErrNotFound)
		}
		them, ok, err := member(ctx, tx, roomID, target)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: that workspace is not a member of this room", ErrNotFound)
		}
		leaving := c.Remove && actor == target
		switch {
		case them.Role == RoleOwner:
			return invalid("the owner's membership cannot be changed")
		case leaving:
		case me.Role != RoleOwner && me.Role != RoleEditor:
			return forbidden("only the room's owner or an editor changes its members")
		case me.Role == RoleEditor && (them.Role == RoleEditor || (c.Role != nil && *c.Role == RoleEditor)):
			return forbidden("only the room's owner makes, changes or removes an editor")
		}
		if c.Remove {
			if _, err := tx.Exec(ctx, `DELETE FROM room_member_agents WHERE room_id = $1 AND workspace_id = $2`, roomID, target); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE room_members SET removed_at = now(), may_spend = false WHERE room_id = $1 AND workspace_id = $2`,
				roomID, target); err != nil {
				return err
			}
			if err := touch(ctx, tx, roomID); err != nil {
				return err
			}
			out, err = scanMember(tx.QueryRow(ctx, `SELECT `+memberCols+` FROM room_members m JOIN rooms r ON r.id = m.room_id
				WHERE m.room_id = $1 AND m.workspace_id = $2`, roomID, target))
			return err
		}
		role, maySpend := them.Role, them.MaySpend
		if c.Role != nil {
			role = *c.Role
			if role == RoleViewer {
				maySpend = false
			}
		}
		if c.MaySpend != nil {
			if *c.MaySpend && role == RoleViewer {
				return invalid("a viewer cannot be given may_spend; make it a member first")
			}
			maySpend = *c.MaySpend
		}
		if _, err := tx.Exec(ctx, `UPDATE room_members SET role = $3, may_spend = $4 WHERE room_id = $1 AND workspace_id = $2`,
			roomID, target, role, maySpend); err != nil {
			return err
		}
		if err := touch(ctx, tx, roomID); err != nil {
			return err
		}
		out, _, err = member(ctx, tx, roomID, target)
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrForbidden) {
		err = fmt.Errorf("rooms: member: %w", err)
	}
	return out, err
}

// moderateMember mutes or unmutes, or bans or unbans, target as actor (B32.52): the room's owner or an editor does it,
// only the owner to an editor, and nobody to the owner. A banned member is removed, with its agents, and refused when it
// joins again; unbanned, it may join again itself.
func (s *Store) moderateMember(ctx context.Context, actor, roomID, target string, c MemberChange) (Member, error) {
	var out Member
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockRoom(ctx, tx, actor, roomID); err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, actor)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no such room", ErrNotFound)
		}
		// Any row of target's, live or removed: an unban reaches a member that is no longer in the room.
		read := func() (Member, error) {
			return scanMember(tx.QueryRow(ctx, `SELECT `+memberCols+` FROM room_members m JOIN rooms r ON r.id = m.room_id
				WHERE m.room_id = $1 AND m.workspace_id = $2`, roomID, target))
		}
		them, err := read()
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: that workspace is not a member of this room", ErrNotFound)
		}
		if err != nil {
			return err
		}
		switch {
		case me.Role != RoleOwner && me.Role != RoleEditor:
			return forbidden("only the room's owner or an editor mutes or bans a member")
		case them.Role == RoleOwner:
			return invalid("the room's owner is not muted or banned")
		case me.Role == RoleEditor && them.Role == RoleEditor:
			return forbidden("only the room's owner mutes or bans an editor")
		}
		switch {
		case c.Muted != nil:
			if them.RemovedAt != nil {
				return fmt.Errorf("%w: that workspace is not a member of this room", ErrNotFound)
			}
			_, err = tx.Exec(ctx, `UPDATE room_members SET muted_at = CASE WHEN $3 THEN COALESCE(muted_at, now()) END
				WHERE room_id = $1 AND workspace_id = $2`, roomID, target, *c.Muted)
		case *c.Banned:
			if _, err := tx.Exec(ctx, `DELETE FROM room_member_agents WHERE room_id = $1 AND workspace_id = $2`, roomID, target); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE room_members SET banned_at = COALESCE(banned_at, now()), removed_at = COALESCE(removed_at, now()),
				may_spend = false WHERE room_id = $1 AND workspace_id = $2`, roomID, target)
		default:
			_, err = tx.Exec(ctx, `UPDATE room_members SET banned_at = NULL WHERE room_id = $1 AND workspace_id = $2`, roomID, target)
		}
		if err != nil {
			return err
		}
		if err := touch(ctx, tx, roomID); err != nil {
			return err
		}
		out, err = read()
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrForbidden) {
		err = fmt.Errorf("rooms: member: %w", err)
	}
	return out, err
}

// AddAgent brings one of ws's agents into the room, as ws's: ws must be a member that is not a viewer, and the agent
// must be ws's own and not archived. created is false when it was already there.
func (s *Store) AddAgent(ctx context.Context, ws, roomID, agentID string) (a Agent, created bool, err error) {
	if strings.TrimSpace(agentID) == "" {
		return Agent{}, false, invalid("body must be {agent_id}")
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		if !ok {
			return forbidden("an agent joins a room only as its owner's member: join the room first")
		}
		if me.Role == RoleViewer {
			return forbidden("a viewer's agents do not join the room")
		}
		if r.Status != Open {
			return fmt.Errorf("%w: the room is %s and takes no new members", ErrConflict, r.Status)
		}
		// A room's wallet is a room's budget, not a member's agent (B32.32).
		if err := tx.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL
			AND kind = 'agent'`, agentID, ws).Scan(&a.Name); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such agent in your workspace", ErrNotFound)
		} else if err != nil {
			return err
		}
		var there bool
		var agents int64
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_member_agents WHERE room_id = $1 AND agent_id = $2),
			(SELECT count(*) FROM room_member_agents WHERE room_id = $1)`, roomID, agentID).Scan(&there, &agents); err != nil {
			return err
		}
		if !there {
			p, err := s.LimitsOf(ctx, tx, r.OwnerWorkspaceID)
			if err != nil {
				return err
			}
			if err := s.refuse(p, "agents_per_room", "the room owner's", agents); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `INSERT INTO room_member_agents (room_id, agent_id, workspace_id) VALUES ($1, $2, $3)
			ON CONFLICT (room_id, agent_id) DO NOTHING`, roomID, agentID, ws)
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		if created {
			if err := touch(ctx, tx, roomID); err != nil {
				return err
			}
		}
		a.AgentID, a.WorkspaceID = agentID, ws
		return tx.QueryRow(ctx, `SELECT joined_at FROM room_member_agents WHERE room_id = $1 AND agent_id = $2`, roomID, agentID).Scan(&a.JoinedAt)
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrConflict) &&
		!errors.Is(err, ErrPlanLimit) {
		err = fmt.Errorf("rooms: agent: %w", err)
	}
	return a, created, err
}
