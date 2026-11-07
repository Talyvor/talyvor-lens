package rooms

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
)

// points.go — B41.143: THE POINTS LEDGER: SIZE × QUALITY × SURVIVAL, APPEND-ONLY (migration 0350).
//
// When a contribution is accepted its author's workspace earns its size × quality × survival. Size is the points its
// claimed size on the rate card fixed (B41.142), less the prizes and royalties its author already received for it: each
// US dollar received takes LENS_ROOM_POINTS_PER_USD points off, and until Nicolai sets that, any money received takes
// the whole size, so nobody is paid twice for the same work. Money itself earns no points: only an accepted
// contribution writes a row. Quality is the contribution's quality_bps: the median of its two reviewers' scores, 1.0
// until both score it (B41.144).
// Survival is 100% while the contribution stays accepted: rejected after it was accepted, a withdrawn row takes back
// what each holder earned from it. A fork passes the room's remix share — the share its room_fork lineage edge locked —
// of its points to its original's author. An agent contributes as its owner's workspace, so its points are its owner's;
// an owner holds them once identity-checked at L2, and until then they show as waiting for that check.

// PointsPerUSDSetting names the setting that converts money already received into points taken off a size.
const PointsPerUSDSetting = "LENS_ROOM_POINTS_PER_USD"

// maxPointsEntries bounds the ledger rows GET /v1/rooms/{id}/points returns.
const maxPointsEntries = 200

// PointsEntry is one row of a room's points ledger.
type PointsEntry struct {
	ID                string    `json:"id"`
	ContributionID    string    `json:"contribution_id"`
	WorkspaceID       string    `json:"workspace_id"`
	Reason            string    `json:"reason"` // accepted, remix_share, withdrawn, review or objection
	SizePoints        int64     `json:"size_points"`
	ReceivedUSDMicros int64     `json:"received_usd_micros"`
	QualityBPS        int       `json:"quality_bps"`
	SurvivalBPS       int       `json:"survival_bps"`
	RemixShareBPS     int       `json:"remix_share_bps"`
	Points            int64     `json:"points"`
	CreatedAt         time.Time `json:"created_at"`
}

// PointsHolder is a workspace's points in a room.
type PointsHolder struct {
	WorkspaceID     string `json:"workspace_id"`
	WorkspaceName   string `json:"workspace_name"`
	Points          int64  `json:"points"`
	IdentityChecked bool   `json:"identity_checked"` // false: its points wait for its owner's L2 check
}

// Points is a room's points ledger as its members see it.
type Points struct {
	RoomID      string         `json:"room_id"`
	TotalPoints int64          `json:"total_points"` // the points of holders that are identity-checked
	Holders     []PointsHolder `json:"holders"`
	Entries     []PointsEntry  `json:"entries"` // newest first
}

// SetPointsPerUSD sets LENS_ROOM_POINTS_PER_USD: the points each US dollar already received takes off a size. 0, the
// default, takes the whole size once any money was received.
func (s *Store) SetPointsPerUSD(n int64) { s.pointsPerUSD = n }

// earnedSize is size less what received µUSD takes off it, a whole US dollar at a time.
func (s *Store) earnedSize(size, received int64) int64 {
	dollars := received / 1_000_000
	switch {
	case received == 0:
		return size
	case s.pointsPerUSD <= 0 || dollars >= size/s.pointsPerUSD+1: // unset, or it takes it all (without overflowing the product)
		return 0
	}
	return max(size-dollars*s.pointsPerUSD, 0)
}

// recordPoints writes c's rows when Decide moves it from was to status, inside Decide's transaction.
func (s *Store) recordPoints(ctx context.Context, tx pgx.Tx, c Contribution, was, status string) error {
	if err := frozenPoints(ctx, tx, c, was); err != nil { // B41.147
		return err
	}
	if err := s.rewardReviewers(ctx, tx, c, was, status); err != nil { // B41.144
		return err
	}
	switch {
	case status == ContributionAccepted && c.Points > 0:
		return s.award(ctx, tx, c)
	case was == ContributionAccepted:
		return withdraw(ctx, tx, c)
	}
	return nil
}

func (s *Store) award(ctx context.Context, tx pgx.Tx, c Contribution) error {
	var received int64
	var quality int
	if err := tx.QueryRow(ctx, `SELECT quality_bps, coalesce((SELECT sum(e.share_usd_micros) FROM market_earnings e
		JOIN market_uses u ON u.id = e.use_id WHERE u.listing_id = c.listing_id AND e.seller_workspace_id = c.author_workspace_id), 0)::bigint
		FROM room_contributions c WHERE c.id = $1`, c.ID).Scan(&quality, &received); err != nil {
		return err
	}
	size := s.earnedSize(c.Points, received)
	points := size * int64(quality) / 10_000 // survival 100%: it stands accepted
	row := PointsEntry{ContributionID: c.ID, WorkspaceID: c.AuthorWorkspaceID, Reason: "accepted", SizePoints: size,
		ReceivedUSDMicros: received, QualityBPS: quality, SurvivalBPS: 10_000, Points: points}
	if c.ForkedFrom != "" {
		var original string
		err := tx.QueryRow(ctx, `SELECT o.author_workspace_id, coalesce(e.share_bps, 0) FROM room_contributions o
			LEFT JOIN market_lineage e ON e.child_listing_id = $2 AND e.child_version = $3 AND e.parent_listing_id = o.listing_id
				AND e.source = 'room_fork'
			WHERE o.id = $1`, c.ForkedFrom, c.ListingID, c.Version).Scan(&original, &row.RemixShareBPS)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if share := points * int64(row.RemixShareBPS) / 10_000; share > 0 {
			remix := row
			remix.WorkspaceID, remix.Reason, remix.Points = original, "remix_share", share
			if err := insertPoints(ctx, tx, c.RoomID, remix); err != nil {
				return err
			}
			row.Points -= share
		}
	}
	return insertPoints(ctx, tx, c.RoomID, row)
}

// withdraw takes back, at survival 0, what each holder still holds from c.
func withdraw(ctx context.Context, tx pgx.Tx, c Contribution) error {
	rows, err := tx.Query(ctx, `SELECT workspace_id, sum(points)::bigint FROM room_points WHERE contribution_id = $1
		GROUP BY workspace_id HAVING sum(points) <> 0 ORDER BY workspace_id`, c.ID)
	if err != nil {
		return err
	}
	held, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PointsEntry, error) {
		e := PointsEntry{ContributionID: c.ID, Reason: "withdrawn", QualityBPS: 10_000}
		err := r.Scan(&e.WorkspaceID, &e.Points)
		e.Points = -e.Points
		return e, err
	})
	if err != nil {
		return err
	}
	for _, e := range held {
		if err := insertPoints(ctx, tx, c.RoomID, e); err != nil {
			return err
		}
	}
	return nil
}

func insertPoints(ctx context.Context, tx pgx.Tx, roomID string, e PointsEntry) error {
	if e.Points > 0 { // B41.150: points count only for a workspace that signed the contributor agreement
		if signed, err := signedAgreement(ctx, tx, roomID, e.WorkspaceID); err != nil || !signed {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO room_points (id, room_id, contribution_id, workspace_id, reason, size_points,
		received_usd_micros, quality_bps, survival_bps, remix_share_bps, points) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		"rpts_"+strings.ReplaceAll(uuid.NewString(), "-", ""), roomID, e.ContributionID, e.WorkspaceID, e.Reason, e.SizePoints,
		e.ReceivedUSDMicros, e.QualityBPS, e.SurvivalBPS, e.RemixShareBPS, e.Points)
	return err
}

// pointsHolders is each holder's points in roomID, most first, and the total of those identity-checked.
func pointsHolders(ctx context.Context, tx pgx.Tx, roomID string) (holders []PointsHolder, total int64, err error) {
	rows, err := tx.Query(ctx, `SELECT p.workspace_id, `+workspaceNameOf+`p.workspace_id`+workspaceNameEnd+`, sum(p.points)::bigint
		FROM room_points p WHERE p.room_id = $1 GROUP BY p.workspace_id ORDER BY 3 DESC, 1`, roomID)
	if err != nil {
		return nil, 0, err
	}
	if holders, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (h PointsHolder, err error) {
		err = r.Scan(&h.WorkspaceID, &h.WorkspaceName, &h.Points)
		return h, err
	}); err != nil {
		return nil, 0, err
	}
	for i := range holders {
		h := &holders[i]
		if h.IdentityChecked, err = economy.IdentityChecked(ctx, tx, h.WorkspaceID); err != nil {
			return nil, 0, err
		}
		if h.IdentityChecked {
			total += h.Points
		}
	}
	return holders, total, nil
}

// Points is roomID's points ledger, for ws, a member that accepted the room's current terms.
func (s *Store) Points(ctx context.Context, ws, roomID string) (p Points, err error) {
	p = Points{RoomID: roomID, Holders: []PointsHolder{}, Entries: []PointsEntry{}}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		var err error
		if p.Holders, p.TotalPoints, err = pointsHolders(ctx, tx, roomID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, contribution_id, workspace_id, reason, size_points, received_usd_micros, quality_bps,
			survival_bps, remix_share_bps, points, created_at FROM room_points WHERE room_id = $1 ORDER BY created_at DESC, id LIMIT $2`,
			roomID, maxPointsEntries)
		if err != nil {
			return err
		}
		p.Entries, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (e PointsEntry, err error) {
			err = r.Scan(&e.ID, &e.ContributionID, &e.WorkspaceID, &e.Reason, &e.SizePoints, &e.ReceivedUSDMicros, &e.QualityBPS,
				&e.SurvivalBPS, &e.RemixShareBPS, &e.Points, &e.CreatedAt)
			return e, err
		})
		return err
	})
	return p, err
}
