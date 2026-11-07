package rooms

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// gate.go — B41.146: THE GATE TRACKER: SHIPPED, USED, PAID (migration 0352).
//
// A room's members read its progress towards distribution from Talyvor's own records. Used: people not linked to the
// room's owner who were active in the room — wrote a message, a contribution or a run themselves, not through an agent
// — in at least used_active_weeks of the last 8 weeks (the weeks are 7-day spans back from now). Linked is as for the
// room's reviewers: the same person joined both, or they share an owner key or a card; and linked people count once
// between them. Shipped and Paid read the room's published build — its public beta and its listing revenue — so until
// a room can publish one (B32.34) both read "needs a published build". Talyvor's thresholds are the floor; the owner
// may raise them and never lower them. Gated on room_equity like the rest of shared ownership.

// Talyvor's Gate thresholds: the floor every room's own must meet or exceed.
const (
	GateShippedWeeks    = 8
	GateUsedPeople      = 100
	GateUsedActiveWeeks = 6
	GateUsedWindowWeeks = 8
	GatePaidUSDMicros   = int64(1_000_000_000) // $1,000
	GatePaidPayers      = 10
)

// The state of one of the Gate's tests.
const (
	GateMet                 = "met"
	GateInProgress          = "in_progress"
	GateNeedsPublishedBuild = "needs_published_build"
)

// GateNeedsBuildNote is what Shipped and Paid say until the room can publish a build.
const GateNeedsBuildNote = "needs a published build"

// GateThresholds are a room's thresholds for the three tests.
type GateThresholds struct {
	ShippedWeeks    int        `json:"shipped_weeks"`
	UsedPeople      int        `json:"used_people"`
	UsedActiveWeeks int        `json:"used_active_weeks"`
	UsedWindowWeeks int        `json:"used_window_weeks"`
	PaidUSDMicros   int64      `json:"paid_usd_micros"`
	PaidPayers      int        `json:"paid_payers"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"` // nil: the room uses Talyvor's thresholds
}

// GateThresholdsChange is PUT /v1/rooms/{id}/gate-thresholds: the thresholds to raise; one left out keeps its value.
type GateThresholdsChange struct {
	ShippedWeeks    *int   `json:"shipped_weeks"`
	UsedPeople      *int   `json:"used_people"`
	UsedActiveWeeks *int   `json:"used_active_weeks"`
	PaidUSDMicros   *int64 `json:"paid_usd_micros"`
	PaidPayers      *int   `json:"paid_payers"`
}

// GateShipped is the Shipped test: the room's public beta live ShippedWeeks weeks in a row.
type GateShipped struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	Weeks  int    `json:"weeks"`
}

// GateUsed is the Used test: People is how many unlinked people were active in UsedActiveWeeks of the last 8 weeks.
type GateUsed struct {
	Status string `json:"status"`
	People int    `json:"people"`
}

// GatePaid is the Paid test: PaidUSDMicros from PaidPayers unlinked payers through Talyvor billing.
type GatePaid struct {
	Status    string `json:"status"`
	Note      string `json:"note,omitempty"`
	USDMicros int64  `json:"usd_micros"`
	Payers    int    `json:"payers"`
}

// GateTracker is a room's progress towards the Gate as its members see it.
type GateTracker struct {
	RoomID     string         `json:"room_id"`
	Thresholds GateThresholds `json:"thresholds"`
	Shipped    GateShipped    `json:"shipped"`
	Used       GateUsed       `json:"used"`
	Paid       GatePaid       `json:"paid"`
	Met        bool           `json:"met"` // all three tests are met
	Notice     string         `json:"notice"`
}

func readGateThresholds(ctx context.Context, tx pgx.Tx, roomID string) (GateThresholds, error) {
	t := GateThresholds{ShippedWeeks: GateShippedWeeks, UsedPeople: GateUsedPeople, UsedActiveWeeks: GateUsedActiveWeeks,
		UsedWindowWeeks: GateUsedWindowWeeks, PaidUSDMicros: GatePaidUSDMicros, PaidPayers: GatePaidPayers}
	err := tx.QueryRow(ctx, `SELECT shipped_weeks, used_people, used_active_weeks, paid_usd_micros, paid_payers, updated_at
		FROM room_gate_thresholds WHERE room_id = $1`, roomID).
		Scan(&t.ShippedWeeks, &t.UsedPeople, &t.UsedActiveWeeks, &t.PaidUSDMicros, &t.PaidPayers, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return t, err
}

// linkedSQL is true when workspaces a and b in room $1 are linked: the same person joined both, or they share an owner
// key or a card.
func linkedSQL(a, b string) string {
	return fmt.Sprintf(`(EXISTS (SELECT 1 FROM room_members x JOIN room_members y ON y.room_id = x.room_id AND y.user_id = x.user_id
			WHERE x.room_id = $1 AND x.workspace_id = %[1]s AND y.workspace_id = %[2]s AND x.user_id <> '')
		OR EXISTS (SELECT 1 FROM workspace_owner_links x JOIN workspace_owner_links y ON y.owner_key = x.owner_key
			WHERE x.workspace_id = %[1]s AND y.workspace_id = %[2]s)
		OR EXISTS (SELECT 1 FROM workspace_card_fingerprints x JOIN workspace_card_fingerprints y ON y.fingerprint_hash = x.fingerprint_hash
			WHERE x.workspace_id = %[1]s AND y.workspace_id = %[2]s))`, a, b)
}

// gateUsedPeople counts the people not linked to roomID's owner active in the room in at least weeks of the last 8
// weeks; of active people linked to each other, only the first by workspace id counts.
func gateUsedPeople(ctx context.Context, tx pgx.Tx, roomID string, weeks int) (n int, err error) {
	err = tx.QueryRow(ctx, `WITH active AS (
			SELECT m.author_workspace_id AS ws FROM room_messages m JOIN rooms r ON r.id = m.room_id
			WHERE m.room_id = $1 AND m.kind IN ('text', 'contribution', 'run') AND m.author_agent_id = ''
				AND m.created_at > now() - make_interval(weeks => $3) AND m.author_workspace_id <> r.owner_workspace_id
			GROUP BY m.author_workspace_id
			HAVING count(DISTINCT floor(extract(epoch FROM now() - m.created_at) / 604800)) >= $2),
		unlinked AS (
			SELECT a.ws FROM active a JOIN rooms r ON r.id = $1 WHERE NOT `+linkedSQL("a.ws", "r.owner_workspace_id")+`)
		SELECT count(*) FROM unlinked e WHERE NOT EXISTS (SELECT 1 FROM unlinked f WHERE f.ws < e.ws AND `+linkedSQL("f.ws", "e.ws")+`)`,
		roomID, weeks, GateUsedWindowWeeks).Scan(&n)
	return n, err
}

// GateTracker is roomID's progress towards the Gate, for ws, a member that accepted the room's current terms.
func (s *Store) GateTracker(ctx context.Context, ws, roomID string) (g GateTracker, err error) {
	g = GateTracker{RoomID: roomID, Notice: OwnershipNotice,
		Shipped: GateShipped{Status: GateNeedsPublishedBuild, Note: GateNeedsBuildNote},
		Paid:    GatePaid{Status: GateNeedsPublishedBuild, Note: GateNeedsBuildNote}}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		if g.Thresholds, err = readGateThresholds(ctx, tx, roomID); err != nil {
			return err
		}
		if g.Used.People, err = gateUsedPeople(ctx, tx, roomID, g.Thresholds.UsedActiveWeeks); err != nil {
			return err
		}
		g.Used.Status = GateInProgress
		if g.Used.People >= g.Thresholds.UsedPeople {
			g.Used.Status = GateMet
		}
		g.Met = g.Shipped.Status == GateMet && g.Used.Status == GateMet && g.Paid.Status == GateMet
		return nil
	})
	return g, err
}

// SetGateThresholds raises roomID's Gate thresholds; only the room's owner does, and a value below Talyvor's or the
// room's current one is refused.
func (s *Store) SetGateThresholds(ctx context.Context, ws, user, roomID string, c GateThresholdsChange) (t GateThresholds, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		m, err := ownershipMember(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		if m.Role != RoleOwner {
			return forbidden("only the room's owner sets its Gate thresholds")
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if r.Status != Open {
			return fmt.Errorf("%w: the room is %s", ErrConflict, r.Status)
		}
		if t, err = readGateThresholds(ctx, tx, roomID); err != nil {
			return err
		}
		if err := errors.Join(raiseGate("shipped_weeks", &t.ShippedWeeks, c.ShippedWeeks), raiseGate("used_people", &t.UsedPeople, c.UsedPeople),
			raiseGate("used_active_weeks", &t.UsedActiveWeeks, c.UsedActiveWeeks),
			raiseGate("paid_usd_micros", &t.PaidUSDMicros, c.PaidUSDMicros), raiseGate("paid_payers", &t.PaidPayers, c.PaidPayers)); err != nil {
			return err
		}
		if t.UsedActiveWeeks > GateUsedWindowWeeks {
			return invalid("used_active_weeks is at most %d, the weeks the test looks back", GateUsedWindowWeeks)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_gate_thresholds (room_id, shipped_weeks, used_people, used_active_weeks,
				paid_usd_micros, paid_payers, set_by_user_id) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (room_id) DO UPDATE SET shipped_weeks = EXCLUDED.shipped_weeks, used_people = EXCLUDED.used_people,
				used_active_weeks = EXCLUDED.used_active_weeks, paid_usd_micros = EXCLUDED.paid_usd_micros,
				paid_payers = EXCLUDED.paid_payers, set_by_user_id = EXCLUDED.set_by_user_id, updated_at = now()`,
			roomID, t.ShippedWeeks, t.UsedPeople, t.UsedActiveWeeks, t.PaidUSDMicros, t.PaidPayers, user); err != nil {
			return err
		}
		t, err = readGateThresholds(ctx, tx, roomID)
		return err
	})
	return t, err
}

// raiseGate sets *cur to *to unless that would lower it; nil keeps it.
func raiseGate[T int | int64](name string, cur, to *T) error {
	if to == nil {
		return nil
	}
	if *to < *cur {
		return fmt.Errorf("%w: a Gate threshold may be raised, never lowered: %s is %d", ErrConflict, name, *cur)
	}
	*cur = *to
	return nil
}
