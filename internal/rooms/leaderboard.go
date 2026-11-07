package rooms

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
)

// leaderboard.go — B41.145: THE PROVISIONAL LEADERBOARD.
//
// Live, for the room's members only: each holder's points from the ledger (B41.143) and their estimated share of the
// contributor pool (B41.141). Only identity-checked holders share the pool, as only they count in total_points; their
// shares are whole basis points that add up to exactly 100% of the pool, the rounding going to the largest remainders.
// A holder waiting for their owner's L2 check is listed with no share until it passes. Every answer carries the note
// that nothing is final until the Gate.

// LeaderboardNote is the note every leaderboard answer carries.
const LeaderboardNote = "Nothing is final until the Gate."

// LeaderboardRow is a holder on a room's leaderboard.
type LeaderboardRow struct {
	PointsHolder
	ShareBPS int `json:"share_bps"` // their estimated share of the contributor pool; 0 while not identity-checked
}

// Leaderboard is a room's provisional leaderboard as its members see it.
type Leaderboard struct {
	RoomID      string           `json:"room_id"`
	Enabled     bool             `json:"enabled"`  // shared ownership is switched on
	PoolBPS     int              `json:"pool_bps"` // the contributor pool
	TotalPoints int64            `json:"total_points"`
	Rows        []LeaderboardRow `json:"rows"` // most points first
	Note        string           `json:"note"`
	Notice      string           `json:"notice"`
}

// Leaderboard is roomID's provisional leaderboard, for ws, a member that accepted the room's current terms.
func (s *Store) Leaderboard(ctx context.Context, ws, roomID string) (l Leaderboard, err error) {
	l = Leaderboard{RoomID: roomID, Rows: []LeaderboardRow{}, Note: LeaderboardNote, Notice: OwnershipNotice}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		o, err := readOwnership(ctx, tx, roomID)
		if err != nil {
			return err
		}
		l.Enabled, l.PoolBPS = o.Enabled, o.PoolBPS
		holders, total, err := pointsHolders(ctx, tx, roomID)
		if err != nil {
			return err
		}
		l.TotalPoints = total
		for i, share := range poolShares(holders) {
			l.Rows = append(l.Rows, LeaderboardRow{PointsHolder: holders[i], ShareBPS: share})
		}
		return nil
	})
	return l, err
}

// poolShares splits 10,000 basis points between the identity-checked holders with points, in proportion to their
// points, by largest remainder so the shares add up to exactly 10,000: the floors leave fewer than one per holder over.
func poolShares(holders []PointsHolder) []int {
	shares := make([]int, len(holders))
	var total int64
	var rest []int
	for i, h := range holders {
		if h.IdentityChecked && h.Points > 0 {
			total += h.Points
			rest = append(rest, i)
		}
	}
	if total == 0 {
		return shares
	}
	left := 10_000
	rem := make([]int64, len(holders))
	for _, i := range rest {
		h := holders[i]
		shares[i], rem[i] = int(h.Points*10_000/total), h.Points*10_000%total
		left -= shares[i]
	}
	sort.SliceStable(rest, func(a, b int) bool { return rem[rest[a]] > rem[rest[b]] })
	for _, i := range rest[:left] {
		shares[i]++
	}
	return shares
}
