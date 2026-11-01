package rooms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// takeover.go — B41.148: A ROOM'S OWNER CAN CHANGE (migration 0293).
//
// The owner hands the room to a member (TransferOwner). Or, when the owner has done nothing in the room for 60 days —
// posted, proposed, decided a contribution or answered a notice — a voting member gives it notice (GiveNotice), which
// the room is told. An owner who does anything in the room within 14 days, or answers the notice (AnswerNotice),
// keeps it. Unanswered, the room's voting members — live, not viewers, not the owner — each vote for a lead among
// themselves (VoteLead): the first candidate whose voters hold more than half of the room's points becomes its owner.
// A member's points are the positive vote tallies on its accepted contributions, counted as a by_votes payout counts
// them (payouts.go), as they stood when the notice was given. Instead of voting, any voting member may fork the room (ForkRoom): a new room it owns, with the same
// title and terms, that names the original as its lineage.
//
// A new owner gets the room without the old owner's wallet: that wallet is an agent of the old owner's workspace,
// holding its money, and stays there; the room opens a new one in the new owner's workspace when it next reads the
// room. The old owner stays as an editor; every member's may_spend is cleared, for the new owner to grant again.

// Takeover windows, as B41.48 states them.
const (
	TakeoverInactiveDays = 60
	TakeoverAnswerDays   = 14
)

// Takeover notice statuses, as migration 0293 checks them.
const (
	TakeoverOpen     = "open"
	TakeoverAnswered = "answered"
	TakeoverElected  = "elected"
)

// TakeoverNotice is a member's notice to an inactive owner.
type TakeoverNotice struct {
	ID                   string     `json:"id"`
	OwnerWorkspaceID     string     `json:"owner_workspace_id"`
	NoticedByWorkspaceID string     `json:"noticed_by_workspace_id"`
	NoticedAt            time.Time  `json:"noticed_at"`
	VotingFrom           time.Time  `json:"voting_from"` // 14 days after the notice: members vote or fork from then
	Status               string     `json:"status"`
	ElectedWorkspaceID   string     `json:"elected_workspace_id,omitempty"`
	ClosedAt             *time.Time `json:"closed_at,omitempty"`
}

// TakeoverMember is a voting member (or the owner) with its points and, while a notice is open, its vote.
type TakeoverMember struct {
	WorkspaceID string `json:"workspace_id"`
	Role        string `json:"role"`
	Points      int64  `json:"points"`
	VotedFor    string `json:"voted_for,omitempty"`
}

// Takeover is GET /v1/rooms/{id}/takeover: whether the owner may be given notice, the latest notice, and the vote.
type Takeover struct {
	OwnerWorkspaceID  string           `json:"owner_workspace_id"`
	OwnerLastActiveAt time.Time        `json:"owner_last_active_at"`
	NoticeFrom        time.Time        `json:"notice_from"` // 60 days after the owner's last activity
	Notice            *TakeoverNotice  `json:"notice"`      // the open notice, else the latest closed one; nil, never any
	Voting            bool             `json:"voting"`      // the open notice went unanswered for 14 days
	Members           []TakeoverMember `json:"members"`
	TotalPoints       int64            `json:"total_points"`
	Tally             map[string]int64 `json:"tally"` // candidate → the points of the members voting for it
}

func (s *Store) takeoverTx(ctx context.Context, roomID, ws string, fn func(tx pgx.Tx, r Room, me Member, t *Takeover) error) (Takeover, error) {
	var t Takeover
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: only the room's members see or take part in a takeover", ErrNotFound)
		}
		if t, err = readTakeover(ctx, tx, r); err != nil {
			return err
		}
		if fn == nil {
			return nil
		}
		if err := shut(r); err != nil {
			return err
		}
		if err := fn(tx, r, me, &t); err != nil {
			return err
		}
		if r, err = lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		}
		t, err = readTakeover(ctx, tx, r)
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrForbidden) &&
		!errors.Is(err, ErrInvalid) && !errors.Is(err, ErrPlanLimit) {
		err = fmt.Errorf("rooms: takeover: %w", err)
	}
	return t, err
}

// memberPoints is room member m's points now: the positive vote tallies on its accepted contributions, from voting
// members other than itself, as a by_votes payout counts them (payouts.go) — but only votes from members that joined by
// $3, the owner's last activity, so workspaces that join while the owner is away cannot make a candidate's points.
const memberPoints = `COALESCE((SELECT sum(GREATEST(v.tally, 0)) FROM (SELECT COALESCE(sum(rv.value), 0) AS tally
	FROM room_contributions c LEFT JOIN room_votes rv ON rv.contribution_id = c.id AND rv.workspace_id <> c.author_workspace_id
		AND EXISTS (SELECT 1 FROM room_members vm WHERE vm.room_id = c.room_id AND vm.workspace_id = rv.workspace_id
			AND vm.removed_at IS NULL AND vm.role <> 'viewer' AND vm.joined_at <= $3)
	WHERE c.room_id = m.room_id AND c.author_workspace_id = m.workspace_id AND c.status = 'accepted' GROUP BY c.id) v), 0)::bigint`

// readTakeover reads r's takeover state, r's row held by tx. An open notice the owner has acted since is answered here.
func readTakeover(ctx context.Context, tx pgx.Tx, r Room) (Takeover, error) {
	t := Takeover{OwnerWorkspaceID: r.OwnerWorkspaceID, Members: []TakeoverMember{}, Tally: map[string]int64{}}
	if err := tx.QueryRow(ctx, `SELECT GREATEST(COALESCE(r.owner_since, r.created_at),
			(SELECT max(created_at) FROM room_messages WHERE room_id = r.id AND author_workspace_id = r.owner_workspace_id),
			(SELECT max(created_at) FROM room_contributions WHERE room_id = r.id AND author_workspace_id = r.owner_workspace_id),
			(SELECT max(decided_at) FROM room_contributions WHERE room_id = r.id AND decided_by_workspace_id = r.owner_workspace_id),
			(SELECT max(closed_at) FROM room_takeovers WHERE room_id = r.id AND status = 'answered'))
		FROM rooms r WHERE r.id = $1`, r.ID).Scan(&t.OwnerLastActiveAt); err != nil {
		return t, err
	}
	t.NoticeFrom = t.OwnerLastActiveAt.AddDate(0, 0, TakeoverInactiveDays)
	var n TakeoverNotice
	err := tx.QueryRow(ctx, `SELECT id, owner_workspace_id, noticed_by_workspace_id, noticed_at, status, elected_workspace_id, closed_at
		FROM room_takeovers WHERE room_id = $1 ORDER BY status = 'open' DESC, noticed_at DESC LIMIT 1`, r.ID).Scan(&n.ID,
		&n.OwnerWorkspaceID, &n.NoticedByWorkspaceID, &n.NoticedAt, &n.Status, &n.ElectedWorkspaceID, &n.ClosedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return t, err
	default:
		if n.Status == TakeoverOpen && (t.OwnerLastActiveAt.After(n.NoticedAt) || n.OwnerWorkspaceID != r.OwnerWorkspaceID) {
			if err := tx.QueryRow(ctx, `UPDATE room_takeovers SET status = 'answered', closed_at = now() WHERE id = $1 RETURNING status, closed_at`,
				n.ID).Scan(&n.Status, &n.ClosedAt); err != nil {
				return t, err
			}
		}
		n.VotingFrom = n.NoticedAt.AddDate(0, 0, TakeoverAnswerDays)
		t.Notice = &n
		t.Voting = n.Status == TakeoverOpen && !time.Now().Before(n.VotingFrom)
	}
	// While a notice is open, each member's points are the ones it found (room_takeover_points), and so is the total:
	// nobody who joins, votes or is removed or made a viewer since moves the majority.
	openID := ""
	if t.Notice != nil && t.Notice.Status == TakeoverOpen {
		openID = t.Notice.ID
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(points), 0)::bigint FROM room_takeover_points WHERE takeover_id = $1`,
			openID).Scan(&t.TotalPoints); err != nil {
			return t, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT m.workspace_id, m.role,
			CASE WHEN $2 = '' THEN `+memberPoints+`
				ELSE COALESCE((SELECT p.points FROM room_takeover_points p WHERE p.takeover_id = $2 AND p.workspace_id = m.workspace_id), 0) END,
			COALESCE((SELECT tv.candidate_workspace_id FROM room_takeover_votes tv WHERE tv.takeover_id = $2 AND tv.workspace_id = m.workspace_id), '')
		FROM room_members m WHERE m.room_id = $1 AND m.removed_at IS NULL AND m.role <> 'viewer'
		ORDER BY m.role = 'owner' DESC, m.joined_at, m.workspace_id`, r.ID, openID, t.OwnerLastActiveAt)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var m TakeoverMember
		if err := rows.Scan(&m.WorkspaceID, &m.Role, &m.Points, &m.VotedFor); err != nil {
			return t, err
		}
		t.Members = append(t.Members, m)
		if openID == "" {
			t.TotalPoints += m.Points
		}
	}
	if err := rows.Err(); err != nil {
		return t, err
	}
	// A vote counts only for a candidate still voting itself: one removed or made a viewer since gathers nothing.
	for _, m := range t.Members {
		if m.VotedFor != "" && t.voter(m.VotedFor) {
			t.Tally[m.VotedFor] += m.Points
		}
	}
	return t, nil
}

// voter reports whether ws is a voting member: live, not a viewer, not the owner.
func (t *Takeover) voter(ws string) bool {
	for _, m := range t.Members {
		if m.WorkspaceID == ws {
			return m.Role != RoleOwner
		}
	}
	return false
}

// Takeover is the room's takeover state, to its members.
func (s *Store) Takeover(ctx context.Context, viewer, roomID string) (Takeover, error) {
	return s.takeoverTx(ctx, roomID, viewer, nil)
}

// GiveNotice gives the room's owner notice, as a voting member, once the owner has been inactive for 60 days.
func (s *Store) GiveNotice(ctx context.Context, ws, roomID string) (Takeover, error) {
	return s.takeoverTx(ctx, roomID, ws, func(tx pgx.Tx, r Room, me Member, t *Takeover) error {
		switch {
		case !t.voter(ws):
			return forbidden("only a member that is not the owner or a viewer gives the owner notice")
		case t.Notice != nil && t.Notice.Status == TakeoverOpen:
			return fmt.Errorf("%w: the owner already has notice, given %s", ErrConflict, t.Notice.NoticedAt.UTC().Format(time.DateOnly))
		case time.Now().Before(t.NoticeFrom):
			return fmt.Errorf("%w: the owner was last active %s; notice can be given from %s", ErrConflict,
				t.OwnerLastActiveAt.UTC().Format(time.DateOnly), t.NoticeFrom.UTC().Format(time.DateOnly))
		}
		id := "rtko_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := tx.Exec(ctx, `INSERT INTO room_takeovers (id, room_id, owner_workspace_id, noticed_by_workspace_id) VALUES ($1, $2, $3, $4)`,
			id, r.ID, r.OwnerWorkspaceID, ws); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_takeover_points (takeover_id, workspace_id, points)
			SELECT $2, m.workspace_id, `+memberPoints+` FROM room_members m WHERE m.room_id = $1 AND m.removed_at IS NULL AND m.role <> 'viewer'`,
			r.ID, id, t.OwnerLastActiveAt); err != nil {
			return err
		}
		return postSystem(ctx, tx, r, fmt.Sprintf("The owner has been inactive for %d days and has been given notice. If the owner does "+
			"nothing in the room within %d days, members can elect a new lead or fork the room.", TakeoverInactiveDays, TakeoverAnswerDays),
			map[string]any{"takeover_id": id, "noticed_by_workspace_id": ws})
	})
}

// AnswerNotice is the owner answering the open notice: it keeps the room, and its 60 days start again.
func (s *Store) AnswerNotice(ctx context.Context, ws, roomID string) (Takeover, error) {
	return s.takeoverTx(ctx, roomID, ws, func(tx pgx.Tx, r Room, me Member, t *Takeover) error {
		if me.Role != RoleOwner {
			return forbidden("only the room's owner answers its notice")
		}
		if t.Notice == nil || t.Notice.Status != TakeoverOpen {
			return fmt.Errorf("%w: the owner has no open notice to answer", ErrConflict)
		}
		_, err := tx.Exec(ctx, `UPDATE room_takeovers SET status = 'answered', closed_at = now() WHERE id = $1`, t.Notice.ID)
		return err
	})
}

// VoteLead is a voting member's vote for candidate as the room's lead, once the notice went unanswered for 14 days. The
// first candidate whose voters hold more than half of the room's points becomes its owner.
func (s *Store) VoteLead(ctx context.Context, ws, roomID, candidate string) (Takeover, error) {
	candidate = strings.TrimSpace(candidate)
	return s.takeoverTx(ctx, roomID, ws, func(tx pgx.Tx, r Room, me Member, t *Takeover) error {
		switch {
		case !t.voter(ws):
			return forbidden("only a member that is not the owner or a viewer votes for a lead")
		case !t.Voting:
			return fmt.Errorf("%w: members vote for a lead only once the owner's notice has gone unanswered for %d days", ErrConflict, TakeoverAnswerDays)
		case !t.voter(candidate):
			return invalid("candidate_workspace_id must be a member of the room that is not the owner or a viewer")
		}
		if c, _, err := member(ctx, tx, r.ID, candidate); err != nil {
			return err
		} else if !c.TermsCurrent {
			return invalid("the candidate has not accepted the room's current terms")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_takeover_votes (takeover_id, workspace_id, candidate_workspace_id) VALUES ($1, $2, $3)
			ON CONFLICT (takeover_id, workspace_id) DO UPDATE SET candidate_workspace_id = EXCLUDED.candidate_workspace_id, voted_at = now()`,
			t.Notice.ID, ws, candidate); err != nil {
			return err
		}
		after, err := readTakeover(ctx, tx, r)
		if err != nil {
			return err
		}
		if after.TotalPoints == 0 || after.Tally[candidate]*2 <= after.TotalPoints {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE room_takeovers SET status = 'elected', elected_workspace_id = $2, closed_at = now() WHERE id = $1`,
			t.Notice.ID, candidate); err != nil {
			return err
		}
		return s.handOver(ctx, tx, r, candidate, "Members holding most of the room's points elected a new lead, who now owns the room.")
	})
}

// TransferOwner is the owner handing the room to a member that is not a viewer and has accepted its current terms.
func (s *Store) TransferOwner(ctx context.Context, actor, roomID, to string) (Detail, error) {
	to = strings.TrimSpace(to)
	_, err := s.takeoverTx(ctx, roomID, actor, func(tx pgx.Tx, r Room, me Member, t *Takeover) error {
		if me.Role != RoleOwner {
			return forbidden("only the room's owner hands it to another member")
		}
		them, ok, err := member(ctx, tx, r.ID, to)
		switch {
		case err != nil:
			return err
		case !ok || to == actor || them.Role == RoleViewer:
			return invalid("workspace_id must be another member of the room that is not a viewer")
		case !them.TermsCurrent:
			return invalid("that member has not accepted the room's current terms")
		}
		return s.handOver(ctx, tx, r, to, "The owner handed the room to another member, who now owns it.")
	})
	if err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, actor, false, roomID)
}

// handOver makes to the owner of r, whose row tx holds, when to's plan allows it one more room as opening one would:
// the old owner stays as an editor, and the room leaves the old owner's wallet behind. Nobody keeps may_spend: the old
// owner's grants were to spend its wallet, not the new owner's.
func (s *Store) handOver(ctx context.Context, tx pgx.Tx, r Room, to, body string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rooms:create:' || $1))`, to); err != nil {
		return err
	}
	p, err := s.LimitsOf(ctx, tx, to)
	if err != nil {
		return err
	}
	var owned int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM rooms WHERE owner_workspace_id = $1 AND visibility = $2 AND status <> 'closed'`,
		to, r.Visibility).Scan(&owned); err != nil {
		return err
	}
	if err := s.refuse(p, r.Visibility+"_rooms", "the new owner's", owned); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE room_members SET may_spend = false, role = CASE role WHEN 'owner' THEN 'editor' ELSE role END
		WHERE room_id = $1`, r.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE room_members SET role = 'owner', may_spend = true WHERE room_id = $1 AND workspace_id = $2`, r.ID, to); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE rooms SET owner_workspace_id = $2, owner_since = now(), wallet_agent_id = NULL WHERE id = $1`, r.ID, to); err != nil {
		return err
	}
	return postSystem(ctx, tx, r, body, map[string]any{"owner_workspace_id": to, "previous_owner_workspace_id": r.OwnerWorkspaceID})
}

// ForkRoom opens a new room for ws, a voting member, once the owner's notice has gone unanswered for 14 days: the original's
// title, topic, description, visibility and current terms, naming the original as its lineage. The original is told.
func (s *Store) ForkRoom(ctx context.Context, ws, user, roomID string) (Detail, error) {
	var d Draft
	if _, err := s.takeoverTx(ctx, roomID, ws, func(tx pgx.Tx, r Room, me Member, t *Takeover) error {
		switch {
		case !t.voter(ws):
			return forbidden("only a member that is not the owner or a viewer forks the room")
		case !t.Voting:
			return fmt.Errorf("%w: members fork the room only once the owner's notice has gone unanswered for %d days", ErrConflict, TakeoverAnswerDays)
		}
		d = Draft{Title: r.Title, Topic: r.Topic, Description: r.Description, Visibility: r.Visibility}
		return tx.QueryRow(ctx, `SELECT split_rule, remix_share_bps, default_price_usd_micros, spend_policy FROM room_terms
			WHERE room_id = $1 AND version = $2`, r.ID, r.TermsVersion).Scan(&d.Terms.SplitRule, &d.Terms.RemixShareBPS,
			&d.Terms.DefaultPriceUSDMicros, &d.Terms.SpendPolicy)
	}); err != nil {
		return Detail{}, err
	}
	// shortcut: the fork opens after the check's transaction, so a notice answered in between still lets this fork open.
	nd, err := s.Create(ctx, ws, user, d)
	if err != nil {
		return Detail{}, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE rooms SET forked_from_room_id = $2 WHERE id = $1`, nd.ID, roomID); err != nil {
			return err
		}
		r, err := lockRoomAny(ctx, tx, roomID)
		if err != nil {
			return err
		}
		return postSystem(ctx, tx, r, "A member forked this room while its owner was away.", map[string]any{"fork_room_id": nd.ID})
	})
	if err != nil {
		return Detail{}, fmt.Errorf("rooms: fork: %w", err)
	}
	return s.Get(ctx, ws, false, nd.ID)
}
