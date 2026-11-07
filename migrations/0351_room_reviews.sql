-- B41.144 — a contribution's quality is scored by two of the room's members drawn at random, not linked to its author.
--
-- Lens draws the reviewers, never the author: two live members that are not viewers, under the room's current terms,
-- not the author and not linked to it (the same person joined both, or the two share an owner key or a card). Each
-- scores the contribution from 0.5 to 1.5 while it is proposed, and once both have, its quality_bps is the median of
-- their counted scores. A reviewer whose own work the author scored in the room is a review ring: its score counts as
-- 1.0 and earns it nothing. When the contribution is first decided, a reviewer whose score agreed with the outcome — at
-- least 1.0 for accepted, under 1.0 for rejected — earns LENS_ROOM_REVIEW_POINTS in the points ledger (reason review).
CREATE TABLE IF NOT EXISTS room_reviews (
    contribution_id       TEXT NOT NULL REFERENCES room_contributions (id) ON DELETE CASCADE,
    room_id               TEXT NOT NULL,
    reviewer_workspace_id TEXT NOT NULL,
    ring                  BOOLEAN NOT NULL DEFAULT false,  -- the author scored the reviewer's work in this room
    score_bps             INTEGER CHECK (score_bps BETWEEN 5000 AND 15000),
    drawn_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    scored_at             TIMESTAMPTZ,
    PRIMARY KEY (contribution_id, reviewer_workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_room_reviews_reviewer ON room_reviews (reviewer_workspace_id);

ALTER TABLE room_points DROP CONSTRAINT IF EXISTS room_points_reason_check;
ALTER TABLE room_points ADD CONSTRAINT room_points_reason_check
    CHECK (reason IN ('accepted', 'remix_share', 'withdrawn', 'review'));
