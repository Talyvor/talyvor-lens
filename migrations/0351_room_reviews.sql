-- B41.144 — a contribution's quality is scored by two of the room's members drawn at random, not linked to its author.
--
-- Lens draws the reviewers, never the author: two live members (not the owner or an editor, who decide it, nor viewers
-- or muted ones) under the room's current terms, not the author and not linked to it (the same person joined both, or
-- the two share an owner key or a card). Each scores the contribution from 0.5 to 1.5 while it is proposed, and once
-- both have, its quality_bps is the median of their counted scores. Two members that scored each other's work in the
-- room are a review ring: their scores count as 1.0 and earn nothing. When a contribution with a size is first decided,
-- a reviewer whose score agreed with the outcome — at least 1.0 for accepted, under 1.0 for rejected — earns
-- LENS_ROOM_REVIEW_POINTS in the points ledger (reason review).
CREATE TABLE IF NOT EXISTS room_reviews (
    contribution_id       TEXT NOT NULL REFERENCES room_contributions (id) ON DELETE CASCADE,
    room_id               TEXT NOT NULL,
    reviewer_workspace_id TEXT NOT NULL,
    ring                  BOOLEAN NOT NULL DEFAULT false,  -- it and the author scored each other's work in the room
    score_bps             INTEGER CHECK (score_bps BETWEEN 5000 AND 15000),
    drawn_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    scored_at             TIMESTAMPTZ,
    PRIMARY KEY (contribution_id, reviewer_workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_room_reviews_reviewer ON room_reviews (reviewer_workspace_id);

ALTER TABLE room_points DROP CONSTRAINT IF EXISTS room_points_reason_check;
ALTER TABLE room_points ADD CONSTRAINT room_points_reason_check
    CHECK (reason IN ('accepted', 'remix_share', 'withdrawn', 'review'));
