package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ratecard.go — B41.142: THE RATE CARD: POINTS FOR EACH SIZE OF WORK, FIXED BEFORE THE WORK STARTS (migration 0349).
//
// The room's owner publishes the pool's points and, for each size of work, its estimated hours and the points per hour
// of its market band: the size earns hours × points per hour, and no size may be worth more than 5% of the pool. A
// contribution claims a size — its author or the room's owner or an editor sets it — and the size's points are copied
// onto it then, so a rate card published later does not change them. Once the contribution is accepted its points are
// never raised: that stops a huge award at the end. Like shared ownership, it is gated on room_equity (equityOpen) and
// shown only to the room's live members who accepted its current terms.

// ItemCapBPS is the most one item may be worth, in basis points of the pool's points.
const ItemCapBPS = 500

// The rate card's bounds, which keep hours × points per hour, and the cap, far inside an int64.
const (
	maxRateSizes     = 12
	maxSizeName      = 24
	maxSizeHours     = 10_000
	maxPointsPerHour = 1_000_000
	maxPoolPoints    = 1_000_000_000_000
)

// RateSize is one size of work: hours × points per hour is Points.
type RateSize struct {
	Size          string `json:"size"`
	Hours         int64  `json:"hours"`
	PointsPerHour int64  `json:"points_per_hour"`
	Points        int64  `json:"points"` // computed; ignored when the owner sends it
}

// RateCard is a room's rate card as its members see it.
type RateCard struct {
	RoomID        string     `json:"room_id"`
	PoolPoints    int64      `json:"pool_points"`     // 0: the owner has not published one
	ItemCapPoints int64      `json:"item_cap_points"` // 5% of the pool: no size is worth more
	Sizes         []RateSize `json:"sizes"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// RateCardChange is PUT /v1/rooms/{id}/rate-card.
type RateCardChange struct {
	PoolPoints int64      `json:"pool_points"`
	Sizes      []RateSize `json:"sizes"`
}

func itemCap(pool int64) int64 { return pool * ItemCapBPS / 10_000 }

func readRateCard(ctx context.Context, tx pgx.Tx, roomID string) (RateCard, error) {
	rc := RateCard{RoomID: roomID, Sizes: []RateSize{}}
	var sizes []byte
	err := tx.QueryRow(ctx, `SELECT pool_points, sizes, updated_at FROM room_rate_cards WHERE room_id = $1`, roomID).
		Scan(&rc.PoolPoints, &sizes, &rc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return rc, nil
	}
	if err == nil {
		err = json.Unmarshal(sizes, &rc.Sizes)
	}
	rc.ItemCapPoints = itemCap(rc.PoolPoints)
	return rc, err
}

// RateCard is roomID's rate card, for ws, a member that accepted the room's current terms.
func (s *Store) RateCard(ctx context.Context, ws, roomID string) (rc RateCard, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		rc, err = readRateCard(ctx, tx, roomID)
		return err
	})
	return rc, err
}

// SetRateCard publishes roomID's rate card; only the room's owner does. Contributions already claimed keep their points.
func (s *Store) SetRateCard(ctx context.Context, ws, user, roomID string, c RateCardChange) (rc RateCard, err error) {
	if c.PoolPoints < 1 || c.PoolPoints > maxPoolPoints {
		return rc, invalid("pool_points is between 1 and %d", int64(maxPoolPoints))
	}
	if len(c.Sizes) == 0 || len(c.Sizes) > maxRateSizes {
		return rc, invalid("a rate card has 1 to %d sizes", maxRateSizes)
	}
	most := itemCap(c.PoolPoints)
	seen := map[string]bool{}
	for i := range c.Sizes {
		z := &c.Sizes[i]
		z.Size = strings.TrimSpace(z.Size)
		switch {
		case z.Size == "" || len(z.Size) > maxSizeName:
			return rc, invalid("each size has a name of 1 to %d characters", maxSizeName)
		case seen[z.Size]:
			return rc, invalid("size %q is on the rate card twice", z.Size)
		case z.Hours < 1 || z.Hours > maxSizeHours:
			return rc, invalid("size %q: hours is between 1 and %d", z.Size, maxSizeHours)
		case z.PointsPerHour < 1 || z.PointsPerHour > maxPointsPerHour:
			return rc, invalid("size %q: points_per_hour is between 1 and %d", z.Size, maxPointsPerHour)
		}
		seen[z.Size] = true
		if z.Points = z.Hours * z.PointsPerHour; z.Points > most {
			return rc, invalid("size %q is worth %d points, more than 5%% of the pool (%d)", z.Size, z.Points, most)
		}
	}
	sizes, err := json.Marshal(c.Sizes)
	if err != nil {
		return rc, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		m, err := ownershipMember(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		if m.Role != RoleOwner {
			return forbidden("only the room's owner publishes its rate card")
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if r.Status != Open {
			return fmt.Errorf("%w: the room is %s", ErrConflict, r.Status)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_rate_cards (room_id, pool_points, sizes, set_by_user_id) VALUES ($1, $2, $3, $4)
			ON CONFLICT (room_id) DO UPDATE SET pool_points = EXCLUDED.pool_points, sizes = EXCLUDED.sizes,
				set_by_user_id = EXCLUDED.set_by_user_id, updated_at = now()`, roomID, c.PoolPoints, string(sizes), user); err != nil {
			return err
		}
		rc, err = readRateCard(ctx, tx, roomID)
		return err
	})
	return rc, err
}

// ClaimSize sets one of the room's contributions to a size on the room's rate card and fixes that size's points on it:
// its author claims a size once, and the room's owner or an editor may change it. Once the contribution is decided its
// points are never raised.
func (s *Store) ClaimSize(ctx context.Context, ws, roomID, contributionID, size string) (out Contribution, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		m, err := ownershipMember(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		// Under the room's lock, as Decide is, so a decision and a claim never cross.
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if err := shut(r); err != nil {
			return err
		}
		c, err := readContribution(ctx, tx, ws, roomID, contributionID)
		if err != nil {
			return err
		}
		switch decides := m.Role == RoleOwner || m.Role == RoleEditor; {
		case c.AuthorWorkspaceID != ws && !decides:
			return forbidden("only the contribution's author or the room's owner or an editor sets its size")
		case !decides && c.Size != "":
			// The author claims once, before the work; it never re-sizes it under review.
			return fmt.Errorf("%w: the contribution is claimed at size %s: the room's owner or an editor changes it", ErrConflict, c.Size)
		}
		rc, err := readRateCard(ctx, tx, roomID)
		if err != nil {
			return err
		}
		var z *RateSize
		for i := range rc.Sizes {
			if rc.Sizes[i].Size == size {
				z = &rc.Sizes[i]
			}
		}
		switch {
		case z == nil:
			return invalid("size %q is not on the room's rate card", size)
		case c.DecidedAt != nil && z.Points > c.Points:
			// Once decided, never raised: rejecting an accepted contribution does not open a raise before accepting it again.
			return fmt.Errorf("%w: the contribution was decided at %d points, and points are never raised after delivery", ErrConflict, c.Points)
		}
		if _, err := tx.Exec(ctx, `UPDATE room_contributions SET size = $2, points = $3 WHERE id = $1`, c.ID, z.Size, z.Points); err != nil {
			return err
		}
		out, err = readContribution(ctx, tx, ws, roomID, contributionID)
		return err
	})
	return out, err
}
