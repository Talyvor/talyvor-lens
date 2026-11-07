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

// objections.go — B41.147: OBJECTIONS AND DISPUTES (migration 0354).
//
// The room's owner freezes the leaderboard once, and that opens a 14-day objection window; from then on a decision that
// would write to the points ledger is refused, so the points only move by objection. In the window any member may
// object to the points an accepted contribution earned its author, proposing a change that leaves the author holding
// between none and the contribution's size at the highest quality. It objects as a member linked neither to the author
// nor to anyone who already objected to those points, so each person objects once, and no objection blocks another, so
// nobody can hold the window shut with one. Lens draws the panel at random, never
// the objector: three live members (not the owner or an editor, who decided the contributions, nor viewers or muted
// ones) under the room's current terms, not the objector or the holder and linked to neither nor to each other — the
// same person joined both, or the two share an owner key or a card, as the reviewer draw links them (B41.144). An
// objection Lens cannot seat three for is refused. Each panelist votes to uphold or dismiss within 14 days of the
// objection; two votes the same way decide it, and an upheld objection writes its change to the points ledger as an
// objection row. Undecided by then, it has lapsed and the points stand. B30.69's shared dispute flow is not built yet,
// so a room's objections are decided here.

// ObjectionDays is how long the objection window stays open after the freeze, and how long a panel has to decide.
const ObjectionDays = 14

// PanelSize is how many members decide each objection; a majority of them decides it.
const PanelSize = 3

// ObjectionRules are the published rules every objections answer carries.
const ObjectionRules = "Objections are raised within 14 days of the leaderboard freezing, once by each person other than the author. Each one is about the points a single " +
	"contribution earned its author and proposes a change that leaves the author with no fewer than none and no more than " +
	"the contribution's size at the highest quality. Three members linked neither to the objector nor to the author, drawn " +
	"at random, decide it within 14 days. Two votes the same way decide it, and an upheld objection changes the points. " +
	"If the panel has not decided it by then, the points stand."

// Objection statuses: lapsed is an open objection whose panel did not decide it in time.
const (
	ObjectionOpen      = "open"
	ObjectionUpheld    = "upheld"
	ObjectionDismissed = "dismissed"
	ObjectionLapsed    = "lapsed"
)

// Panelist is one member drawn to decide an objection.
type Panelist struct {
	WorkspaceID   string     `json:"workspace_id"`
	WorkspaceName string     `json:"workspace_name"`
	Uphold        *bool      `json:"uphold"` // null until it votes
	VotedAt       *time.Time `json:"voted_at,omitempty"`
}

// Objection is an objection to the points one contribution earned its author.
type Objection struct {
	ID                  string     `json:"id"`
	ContributionID      string     `json:"contribution_id"`
	HolderWorkspaceID   string     `json:"holder_workspace_id"`
	ObjectorWorkspaceID string     `json:"objector_workspace_id"`
	Points              int64      `json:"points"` // the change proposed, signed
	Reason              string     `json:"reason"`
	Status              string     `json:"status"`
	RaisedAt            time.Time  `json:"raised_at"`
	DecideBy            time.Time  `json:"decide_by"`
	DecidedAt           *time.Time `json:"decided_at,omitempty"`
	Panel               []Panelist `json:"panel"`
}

// Objections is a room's frozen leaderboard's objections as its members see them.
type Objections struct {
	RoomID            string      `json:"room_id"`
	FrozenAt          *time.Time  `json:"frozen_at"`           // null until the owner freezes the leaderboard
	ObjectionsCloseAt *time.Time  `json:"objections_close_at"` // the window's end
	Rules             string      `json:"rules"`
	Objections        []Objection `json:"objections"` // oldest first
}

// ObjectionRaise is POST /v1/rooms/{id}/objections.
type ObjectionRaise struct {
	ContributionID string `json:"contribution_id"`
	Points         int64  `json:"points"`
	Reason         string `json:"reason"`
}

// readFreeze is when roomID's leaderboard froze and its objection window closes; both nil until it is frozen.
func readFreeze(ctx context.Context, tx pgx.Tx, roomID string) (frozen, closes *time.Time, err error) {
	err = tx.QueryRow(ctx, `SELECT frozen_at, objections_close_at FROM room_leaderboard_freezes WHERE room_id = $1`, roomID).
		Scan(&frozen, &closes)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return frozen, closes, err
}

// frozenPoints refuses, inside Decide's transaction, a decision that would write to a frozen leaderboard's ledger.
func frozenPoints(ctx context.Context, tx pgx.Tx, c Contribution, was string) error {
	if c.Points <= 0 && was != ContributionAccepted {
		return nil
	}
	frozen, _, err := readFreeze(ctx, tx, c.RoomID)
	if err == nil && frozen != nil {
		err = fmt.Errorf("%w: the leaderboard is frozen: its points change only by objection", ErrConflict)
	}
	return err
}

// FreezeLeaderboard freezes roomID's leaderboard, for ws, its owner, and opens the objection window. It freezes once.
func (s *Store) FreezeLeaderboard(ctx context.Context, ws, user, roomID string) (out Objections, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		m, err := ownershipMember(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		if m.Role != RoleOwner {
			return forbidden("only the room's owner freezes its leaderboard")
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if err := shut(r); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO room_leaderboard_freezes (room_id, objections_close_at, frozen_by_user_id)
			VALUES ($1, now() + make_interval(days => $2), $3) ON CONFLICT (room_id) DO NOTHING`, roomID, ObjectionDays, user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: the leaderboard is already frozen", ErrConflict)
		}
		out, err = readObjections(ctx, tx, roomID)
		return err
	})
	return out, err
}

// RoomObjections is roomID's objections, for ws, a member that accepted the room's current terms.
func (s *Store) RoomObjections(ctx context.Context, ws, roomID string) (out Objections, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		out, err = readObjections(ctx, tx, roomID)
		return err
	})
	return out, err
}

// RaiseObjection records ws's objection to the points one of the room's contributions earned its author, while the
// window is open, and seats the three members Lens draws to decide it.
func (s *Store) RaiseObjection(ctx context.Context, ws, roomID string, in ObjectionRaise) (out Objection, err error) {
	in.Reason = strings.TrimSpace(in.Reason)
	switch {
	case in.Points == 0:
		return out, invalid("points is the change proposed: more than 0 to add, less than 0 to take away")
	case in.Reason == "" || len(in.Reason) > 2000:
		return out, invalid("reason is between 1 and 2000 characters")
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if err := shut(r); err != nil {
			return err
		}
		if _, closes, err := readFreeze(ctx, tx, roomID); err != nil {
			return err
		} else if closes == nil || !time.Now().Before(*closes) {
			return fmt.Errorf("%w: objections are raised in the %d days after the leaderboard freezes", ErrConflict, ObjectionDays)
		}
		c, err := readContribution(ctx, tx, ws, roomID, in.ContributionID)
		if err != nil {
			return err
		}
		if c.Status != ContributionAccepted {
			return fmt.Errorf("%w: the contribution is %s: objections are about the points accepted ones earned", ErrConflict, c.Status)
		}
		// One objection per person: never the author's, nor one linked to the author or to someone who already objected.
		var linkedParty bool
		if err := tx.QueryRow(ctx, `WITH p AS (SELECT $3::text AS ws UNION SELECT objector_workspace_id FROM room_objections
				WHERE contribution_id = $2)
			SELECT EXISTS (SELECT 1 FROM p WHERE p.ws = $1
				OR EXISTS (SELECT 1 FROM room_members a JOIN room_members b ON b.room_id = a.room_id AND b.user_id = a.user_id
					WHERE a.room_id = $4 AND a.workspace_id = p.ws AND b.workspace_id = $1 AND a.user_id <> '')
				OR EXISTS (SELECT 1 FROM workspace_owner_links x JOIN workspace_owner_links y ON y.owner_key = x.owner_key
					WHERE x.workspace_id = p.ws AND y.workspace_id = $1)
				OR EXISTS (SELECT 1 FROM workspace_card_fingerprints x JOIN workspace_card_fingerprints y
					ON y.fingerprint_hash = x.fingerprint_hash WHERE x.workspace_id = p.ws AND y.workspace_id = $1))`,
			ws, c.ID, c.AuthorWorkspaceID, roomID).Scan(&linkedParty); err != nil {
			return err
		} else if linkedParty {
			return fmt.Errorf("%w: objections come from members linked neither to the author nor to anyone who already objected to these points",
				ErrConflict)
		}
		var held int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(points), 0)::bigint FROM room_points WHERE contribution_id = $1
			AND workspace_id = $2`, c.ID, c.AuthorWorkspaceID).Scan(&held); err != nil {
			return err
		}
		if most := c.Points * 15_000 / 10_000; held+in.Points < 0 || held+in.Points > most {
			return invalid("the change leaves the author between 0 and %d points for this contribution; it holds %d", most, held)
		}
		id := "robj_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		tag, err := tx.Exec(ctx, `INSERT INTO room_objections (id, room_id, contribution_id, holder_workspace_id,
			objector_workspace_id, points, reason, decide_by) VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(days => $8))
			ON CONFLICT (contribution_id, objector_workspace_id) DO NOTHING`,
			id, roomID, c.ID, c.AuthorWorkspaceID, ws, in.Points, in.Reason, ObjectionDays)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: you have already objected to this contribution's points", ErrConflict)
		}
		// One seat at a time, each linked neither to the objector and the holder nor to a panelist already seated.
		apart := []string{ws, c.AuthorWorkspaceID}
		for range PanelSize {
			var seated string
			err := tx.QueryRow(ctx, `INSERT INTO room_objection_panel (objection_id, workspace_id)
				SELECT $2, m.workspace_id FROM room_members m JOIN rooms r ON r.id = m.room_id
				WHERE m.room_id = $1 AND m.removed_at IS NULL AND m.muted_at IS NULL AND m.role = 'member'
					AND m.terms_version = r.terms_version AND m.workspace_id <> ALL ($3::text[])
					AND NOT EXISTS (SELECT 1 FROM room_members a WHERE a.room_id = $1 AND a.workspace_id = ANY ($3::text[])
						AND a.user_id <> '' AND a.user_id = m.user_id)
					AND NOT EXISTS (SELECT 1 FROM workspace_owner_links x JOIN workspace_owner_links y ON y.owner_key = x.owner_key
						WHERE x.workspace_id = ANY ($3::text[]) AND y.workspace_id = m.workspace_id)
					AND NOT EXISTS (SELECT 1 FROM workspace_card_fingerprints x JOIN workspace_card_fingerprints y
						ON y.fingerprint_hash = x.fingerprint_hash WHERE x.workspace_id = ANY ($3::text[]) AND y.workspace_id = m.workspace_id)
				ORDER BY random() LIMIT 1 RETURNING workspace_id`, roomID, id, apart).Scan(&seated)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: the room needs %d members linked neither to you, the author nor each other to hear an objection",
					ErrConflict, PanelSize)
			}
			if err != nil {
				return err
			}
			apart = append(apart, seated)
		}
		out, err = readObjection(ctx, tx, roomID, id)
		return err
	})
	return out, err
}

// VoteObjection records ws's vote on an objection it was drawn to decide, replacing any it gave before; once two votes
// agree the objection is decided, and upheld it changes the holder's points.
func (s *Store) VoteObjection(ctx context.Context, ws, roomID, id string, uphold bool) (out Objection, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if err := shut(r); err != nil {
			return err
		}
		o, err := readObjection(ctx, tx, roomID, id)
		if err != nil {
			return err
		}
		if o.Status != ObjectionOpen {
			return fmt.Errorf("%w: the objection is %s", ErrConflict, o.Status)
		}
		tag, err := tx.Exec(ctx, `UPDATE room_objection_panel SET uphold = $3, voted_at = now() WHERE objection_id = $1
			AND workspace_id = $2 AND EXISTS (SELECT 1 FROM room_members m WHERE m.room_id = $4 AND m.workspace_id = $2
				AND m.muted_at IS NULL AND m.role = 'member')`, id, ws, uphold, roomID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return forbidden("only the members Lens drew to decide the objection, while they neither decide contributions nor are muted, vote on it")
		}
		var upholds, dismisses int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE uphold), count(*) FILTER (WHERE NOT uphold)
			FROM room_objection_panel WHERE objection_id = $1`, id).Scan(&upholds, &dismisses); err != nil {
			return err
		}
		majority := PanelSize/2 + 1
		switch {
		case upholds >= majority:
			if err := upholdObjection(ctx, tx, roomID, o); err != nil {
				return err
			}
		case dismisses >= majority:
			if _, err := tx.Exec(ctx, `UPDATE room_objections SET status = 'dismissed', decided_at = now() WHERE id = $1`, id); err != nil {
				return err
			}
		}
		out, err = readObjection(ctx, tx, roomID, id)
		return err
	})
	return out, err
}

// upholdObjection writes o's change to the holder's points, within what the rules allow it to hold now: an earlier
// objection may have moved them since o was raised.
func upholdObjection(ctx context.Context, tx pgx.Tx, roomID string, o Objection) error {
	var held, size int64
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT sum(points) FROM room_points WHERE contribution_id = c.id
		AND workspace_id = $2), 0)::bigint, coalesce(c.points, 0)::bigint FROM room_contributions c WHERE c.id = $1`,
		o.ContributionID, o.HolderWorkspaceID).Scan(&held, &size); err != nil {
		return err
	}
	change := min(max(held+o.Points, 0), size*15_000/10_000) - held
	if change != 0 {
		if err := insertPoints(ctx, tx, roomID, PointsEntry{ContributionID: o.ContributionID, WorkspaceID: o.HolderWorkspaceID,
			Reason: "objection", QualityBPS: 10_000, SurvivalBPS: 10_000, Points: change}); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE room_objections SET status = 'upheld', decided_at = now() WHERE id = $1`, o.ID)
	return err
}

const objectionCols = `o.id, o.contribution_id, o.holder_workspace_id, o.objector_workspace_id, o.points, o.reason,
	CASE WHEN o.status = 'open' AND o.decide_by <= now() THEN 'lapsed' ELSE o.status END, o.raised_at, o.decide_by, o.decided_at`

func scanObjection(r pgx.CollectableRow) (o Objection, err error) {
	o.Panel = []Panelist{}
	err = r.Scan(&o.ID, &o.ContributionID, &o.HolderWorkspaceID, &o.ObjectorWorkspaceID, &o.Points, &o.Reason, &o.Status,
		&o.RaisedAt, &o.DecideBy, &o.DecidedAt)
	return o, err
}

// readPanels fills each objection's panel.
func readPanels(ctx context.Context, tx pgx.Tx, objections []Objection) error {
	at := map[string]int{}
	ids := make([]string, len(objections))
	for i, o := range objections {
		at[o.ID], ids[i] = i, o.ID
	}
	rows, err := tx.Query(ctx, `SELECT p.objection_id, p.workspace_id, `+workspaceNameOf+`p.workspace_id`+workspaceNameEnd+`,
		p.uphold, p.voted_at FROM room_objection_panel p WHERE p.objection_id = ANY ($1::text[]) ORDER BY p.drawn_at, 2`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p Panelist
		if err := rows.Scan(&id, &p.WorkspaceID, &p.WorkspaceName, &p.Uphold, &p.VotedAt); err != nil {
			return err
		}
		objections[at[id]].Panel = append(objections[at[id]].Panel, p)
	}
	return rows.Err()
}

func readObjection(ctx context.Context, tx pgx.Tx, roomID, id string) (Objection, error) {
	rows, err := tx.Query(ctx, `SELECT `+objectionCols+` FROM room_objections o WHERE o.room_id = $1 AND o.id = $2`, roomID, id)
	if err != nil {
		return Objection{}, err
	}
	o, err := pgx.CollectExactlyOneRow(rows, scanObjection)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, fmt.Errorf("%w: no such objection", ErrNotFound)
	}
	if err != nil {
		return o, err
	}
	one := []Objection{o}
	err = readPanels(ctx, tx, one)
	return one[0], err
}

func readObjections(ctx context.Context, tx pgx.Tx, roomID string) (out Objections, err error) {
	out = Objections{RoomID: roomID, Rules: ObjectionRules}
	if out.FrozenAt, out.ObjectionsCloseAt, err = readFreeze(ctx, tx, roomID); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT `+objectionCols+` FROM room_objections o WHERE o.room_id = $1 ORDER BY o.raised_at, o.id`, roomID)
	if err != nil {
		return out, err
	}
	if out.Objections, err = pgx.CollectRows(rows, scanObjection); err != nil {
		return out, err
	}
	if out.Objections == nil {
		out.Objections = []Objection{}
	}
	return out, readPanels(ctx, tx, out.Objections)
}
