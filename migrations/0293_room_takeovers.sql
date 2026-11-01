-- B41.148 — a room's owner can change: the owner hands the room to a member, or its members take it over.
--
-- owner_since is when the room's current owner became its owner; NULL is the creator, owner since created_at. A
-- member gives notice when the owner has done nothing in the room for 60 days; an owner who does anything in the
-- room within 14 days answers it. Unanswered, the room's voting members elect a new lead — the first candidate whose
-- voters hold most of the room's points wins — or any of them forks the room, and the fork names its original in
-- forked_from_room_id (not a foreign key: a deleted original leaves its forks as they are).
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS owner_since TIMESTAMPTZ;
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS forked_from_room_id TEXT;
CREATE INDEX IF NOT EXISTS idx_rooms_forked_from ON rooms (forked_from_room_id) WHERE forked_from_room_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS room_takeovers (
    id                      TEXT PRIMARY KEY,                -- rtko_…
    room_id                 TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    owner_workspace_id      TEXT NOT NULL,                   -- the owner the notice was given to
    noticed_by_workspace_id TEXT NOT NULL,
    noticed_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    status                  TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'answered', 'elected')),
    elected_workspace_id    TEXT NOT NULL DEFAULT '',
    closed_at               TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_room_takeovers_open ON room_takeovers (room_id) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_room_takeovers_noticed_by ON room_takeovers (noticed_by_workspace_id);

-- Each member's points as the notice found them (the owner's too): the vote is tallied against these, so nobody who
-- joins, votes or is removed or made a viewer afterwards changes how many points make a majority.
CREATE TABLE IF NOT EXISTS room_takeover_points (
    takeover_id  TEXT NOT NULL REFERENCES room_takeovers (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    points       BIGINT NOT NULL CHECK (points >= 0),
    PRIMARY KEY (takeover_id, workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_room_takeover_points_workspace ON room_takeover_points (workspace_id);

-- A voting member's choice of lead, one per member per notice, replaced by its next.
CREATE TABLE IF NOT EXISTS room_takeover_votes (
    takeover_id            TEXT NOT NULL REFERENCES room_takeovers (id) ON DELETE CASCADE,
    workspace_id           TEXT NOT NULL,
    candidate_workspace_id TEXT NOT NULL,
    voted_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (takeover_id, workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_room_takeover_votes_workspace ON room_takeover_votes (workspace_id);
