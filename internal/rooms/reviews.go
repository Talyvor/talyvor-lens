package rooms

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// reviews.go — B41.144: QUALITY SCORED BY TWO RANDOM REVIEWERS (migration 0351).
//
// Lens draws a contribution's two reviewers at random, never its author: live members — not the owner or an editor, who
// decide it, nor viewers or muted ones — under the room's current terms, not its author and not linked to it — the same person joined both, or the two share
// an owner key or a card (as the pool royalty's ring detector links workspaces). Any member may ask for the draw; it
// only fills empty seats and never replaces a reviewer. Each reviewer scores the contribution from 0.5 to 1.5 while it
// is proposed, and once both have, its quality_bps — what the points ledger multiplies its size by — is the median of
// their counted scores. A review ring is dampened: two members that scored each other's work in the room count as 1.0
// and earn nothing for it, both ways, on every contribution of theirs still proposed. When a contribution with a size
// is first decided, each other reviewer whose score agreed with the outcome — at least 1.0 for accepted, under 1.0 for
// rejected — earns LENS_ROOM_REVIEW_POINTS (a setting Nicolai fills; 0 until he does) in the points ledger, which a
// later withdrawal takes back with the rest.

// ReviewPointsSetting names the setting for the points a reviewer earns when its score agreed with the outcome.
const ReviewPointsSetting = "LENS_ROOM_REVIEW_POINTS"

// Reviewers is how many members score each contribution.
const Reviewers = 2

// Review is one reviewer's seat on a contribution.
type Review struct {
	ReviewerWorkspaceID   string     `json:"reviewer_workspace_id"`
	ReviewerWorkspaceName string     `json:"reviewer_workspace_name"`
	ScoreBPS              *int       `json:"score_bps"` // 5000–15000; null until it scores
	Ring                  bool       `json:"ring"`      // it and the author scored each other's work: its score counts as 1.0
	DrawnAt               time.Time  `json:"drawn_at"`
	ScoredAt              *time.Time `json:"scored_at,omitempty"`
}

// Reviews is a contribution's reviewers and the quality their scores set.
type Reviews struct {
	ContributionID string   `json:"contribution_id"`
	QualityBPS     int      `json:"quality_bps"` // the median of both counted scores; 10000 until both are in
	Reviews        []Review `json:"reviews"`
}

// SetReviewPoints sets LENS_ROOM_REVIEW_POINTS.
func (s *Store) SetReviewPoints(n int64) { s.reviewPoints = n }

// setQuality sets contribution id's quality_bps to the median of its reviewers' counted scores, once all of them scored.
func setQuality(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE room_contributions SET quality_bps = (SELECT floor(percentile_cont(0.5) WITHIN GROUP
		(ORDER BY CASE WHEN ring THEN 10000 ELSE score_bps END))::int FROM room_reviews WHERE contribution_id = $1)
		WHERE id = $1 AND (SELECT count(score_bps) FROM room_reviews WHERE contribution_id = $1) >= $2`, id, Reviewers)
	return err
}

// reviewable reads ws's membership and one of the room's contributions, under the room's lock, while it is proposed.
func reviewable(ctx context.Context, tx pgx.Tx, ws, roomID, id string) (Contribution, error) {
	if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
		return Contribution{}, err
	}
	if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
		return Contribution{}, err
	} else if err := shut(r); err != nil {
		return Contribution{}, err
	}
	c, err := readContribution(ctx, tx, ws, roomID, id)
	if err == nil && c.Status != ContributionProposed {
		err = fmt.Errorf("%w: the contribution is %s: it is scored while it is proposed", ErrConflict, c.Status)
	}
	return c, err
}

func readReviews(ctx context.Context, tx pgx.Tx, c Contribution) (Reviews, error) {
	out := Reviews{ContributionID: c.ID}
	if err := tx.QueryRow(ctx, `SELECT quality_bps FROM room_contributions WHERE id = $1`, c.ID).Scan(&out.QualityBPS); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT v.reviewer_workspace_id, `+workspaceNameOf+`v.reviewer_workspace_id`+workspaceNameEnd+`,
		v.score_bps, v.ring, v.drawn_at, v.scored_at FROM room_reviews v WHERE v.contribution_id = $1 ORDER BY v.drawn_at, 1`, c.ID)
	if err != nil {
		return out, err
	}
	out.Reviews, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (v Review, err error) {
		err = r.Scan(&v.ReviewerWorkspaceID, &v.ReviewerWorkspaceName, &v.ScoreBPS, &v.Ring, &v.DrawnAt, &v.ScoredAt)
		return v, err
	})
	if out.Reviews == nil {
		out.Reviews = []Review{}
	}
	return out, err
}

// ContributionReviews is one of the room's contributions' reviewers, for ws, a member that accepted its current terms.
func (s *Store) ContributionReviews(ctx context.Context, ws, roomID, id string) (out Reviews, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		c, err := readContribution(ctx, tx, ws, roomID, id)
		if err != nil {
			return err
		}
		out, err = readReviews(ctx, tx, c)
		return err
	})
	return out, err
}

// DrawReviewers fills a proposed contribution's empty reviewer seats with members drawn at random from those not
// linked to its author. Whoever asks, Lens picks; the seats it cannot fill yet stay empty until more members join.
func (s *Store) DrawReviewers(ctx context.Context, ws, roomID, id string) (out Reviews, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := reviewable(ctx, tx, ws, roomID, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_reviews (contribution_id, room_id, reviewer_workspace_id)
			SELECT $3, $1, m.workspace_id FROM room_members m JOIN rooms r ON r.id = m.room_id
			WHERE m.room_id = $1 AND m.removed_at IS NULL AND m.muted_at IS NULL AND m.role = 'member'
				AND m.terms_version = r.terms_version AND m.workspace_id <> $2
				AND NOT EXISTS (SELECT 1 FROM room_reviews v WHERE v.contribution_id = $3 AND v.reviewer_workspace_id = m.workspace_id)
				AND NOT EXISTS (SELECT 1 FROM room_members a WHERE a.room_id = $1 AND a.workspace_id = $2 AND a.user_id <> ''
					AND a.user_id = m.user_id)
				AND NOT EXISTS (SELECT 1 FROM workspace_owner_links x JOIN workspace_owner_links y ON y.owner_key = x.owner_key
					WHERE x.workspace_id = $2 AND y.workspace_id = m.workspace_id)
				AND NOT EXISTS (SELECT 1 FROM workspace_card_fingerprints x JOIN workspace_card_fingerprints y
					ON y.fingerprint_hash = x.fingerprint_hash WHERE x.workspace_id = $2 AND y.workspace_id = m.workspace_id)
			ORDER BY random() LIMIT greatest($4 - (SELECT count(*) FROM room_reviews WHERE contribution_id = $3), 0)`,
			roomID, c.AuthorWorkspaceID, c.ID, Reviewers); err != nil {
			return err
		}
		out, err = readReviews(ctx, tx, c)
		return err
	})
	return out, err
}

// Review records ws's score for a proposed contribution it was drawn to review, replacing any it gave before; once both
// reviewers have scored, the contribution's quality is the median of their counted scores.
func (s *Store) Review(ctx context.Context, ws, roomID, id string, scoreBPS int) (out Reviews, err error) {
	if scoreBPS < 5000 || scoreBPS > 15000 {
		return out, invalid("score_bps is between 5000 (0.5) and 15000 (1.5)")
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := reviewable(ctx, tx, ws, roomID, id)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE room_reviews SET score_bps = $3, scored_at = now(), ring = EXISTS (SELECT 1 FROM room_reviews v
			JOIN room_contributions o ON o.id = v.contribution_id
			WHERE v.room_id = $4 AND v.reviewer_workspace_id = $5 AND o.author_workspace_id = $2 AND v.score_bps IS NOT NULL)
			WHERE contribution_id = $1 AND reviewer_workspace_id = $2 AND EXISTS (SELECT 1 FROM room_members m WHERE m.room_id = $4
				AND m.workspace_id = $2 AND m.muted_at IS NULL AND m.role = 'member')`, c.ID, ws, scoreBPS, roomID, c.AuthorWorkspaceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return forbidden("only the members Lens drew to review the contribution, while they neither decide it nor are muted, score it")
		}
		// The author scored ws's work too: that pair is a ring both ways, on its contributions still proposed.
		rows, err := tx.Query(ctx, `UPDATE room_reviews v SET ring = true FROM room_contributions o WHERE o.id = v.contribution_id
			AND o.status = 'proposed' AND v.room_id = $1 AND v.reviewer_workspace_id = $2 AND o.author_workspace_id = $3
			AND v.score_bps IS NOT NULL AND NOT v.ring RETURNING v.contribution_id`, roomID, c.AuthorWorkspaceID, ws)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range append(ids, c.ID) {
			if err := setQuality(ctx, tx, id); err != nil {
				return err
			}
		}
		out, err = readReviews(ctx, tx, c)
		return err
	})
	return out, err
}

// rewardReviewers writes, when c is first decided with a size at stake, LENS_ROOM_REVIEW_POINTS to each reviewer that is not a ring and
// whose score agreed with the decision, inside Decide's transaction.
func (s *Store) rewardReviewers(ctx context.Context, tx pgx.Tx, c Contribution, was, status string) error {
	if was != ContributionProposed || s.reviewPoints <= 0 || c.Points <= 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT reviewer_workspace_id, score_bps FROM room_reviews WHERE contribution_id = $1
		AND score_bps IS NOT NULL AND NOT ring AND (score_bps >= 10000) = $2 ORDER BY reviewer_workspace_id`,
		c.ID, status == ContributionAccepted)
	if err != nil {
		return err
	}
	agreed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PointsEntry, error) {
		e := PointsEntry{ContributionID: c.ID, Reason: "review", SurvivalBPS: 10_000, Points: s.reviewPoints}
		return e, r.Scan(&e.WorkspaceID, &e.QualityBPS)
	})
	if err != nil {
		return err
	}
	for _, e := range agreed {
		if err := insertPoints(ctx, tx, c.RoomID, e); err != nil {
			return err
		}
	}
	return nil
}
