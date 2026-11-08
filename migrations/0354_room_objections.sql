-- B41.147 — objections to a room's frozen leaderboard, decided by a panel of three members drawn at random.
--
-- The room's owner freezes the leaderboard once; that opens a 14-day window in which any member may object to the
-- points a contribution earned its author, proposing a change. Lens draws a panel of three live members (not the
-- owner or an editor, who decided the contributions) under the room's current terms, linked neither to the objector
-- nor to the holder (the same person joined both, or the two share an owner key or a card). Each panelist votes to
-- uphold or dismiss within 14 days of the objection; two votes the same way decide it, and an upheld objection writes
-- its change to the points ledger (reason objection). Undecided by then, it has lapsed and the points stand.
CREATE TABLE IF NOT EXISTS room_leaderboard_freezes (
    room_id             TEXT PRIMARY KEY REFERENCES rooms (id) ON DELETE CASCADE,
    frozen_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    objections_close_at TIMESTAMPTZ NOT NULL,
    frozen_by_user_id   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS room_objections (
    id                    TEXT PRIMARY KEY,   -- robj_…
    room_id               TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    contribution_id       TEXT NOT NULL REFERENCES room_contributions (id) ON DELETE CASCADE,
    holder_workspace_id   TEXT NOT NULL,      -- whose points it would change: the contribution's author
    objector_workspace_id TEXT NOT NULL,
    points                BIGINT NOT NULL CHECK (points <> 0),   -- the change proposed, signed
    reason                TEXT NOT NULL,
    status                TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'upheld', 'dismissed')),
    raised_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    decide_by             TIMESTAMPTZ NOT NULL,
    decided_at            TIMESTAMPTZ,
    UNIQUE (contribution_id, objector_workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_room_objections_room ON room_objections (room_id, raised_at);
CREATE INDEX IF NOT EXISTS idx_room_objections_objector ON room_objections (objector_workspace_id);
CREATE INDEX IF NOT EXISTS idx_room_objections_holder ON room_objections (holder_workspace_id);

CREATE TABLE IF NOT EXISTS room_objection_panel (
    objection_id TEXT NOT NULL REFERENCES room_objections (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    uphold       BOOLEAN,                     -- null until it votes
    drawn_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    voted_at     TIMESTAMPTZ,
    PRIMARY KEY (objection_id, workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_room_objection_panel_workspace ON room_objection_panel (workspace_id);

ALTER TABLE room_points DROP CONSTRAINT IF EXISTS room_points_reason_check;
ALTER TABLE room_points ADD CONSTRAINT room_points_reason_check
    CHECK (reason IN ('accepted', 'remix_share', 'withdrawn', 'review', 'objection'));
