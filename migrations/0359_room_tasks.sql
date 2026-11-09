-- B41.152 — a room's open tasks board: tasks sized from the rate card that any member may claim.
--
-- The room's owner or an editor posts a task at a size on the room's rate card, and the size's points are copied onto
-- it then, as a contribution's are. Any member that accepted the room's current terms claims an open task for itself
-- and may let it go again. There is deliberately no column to assign a task to anyone, and none for a deadline or a
-- penalty: contributors stay volunteers, not workers (ERA 1996 s230(3)(b)). Gated on room_equity (RED), like the
-- rate card.
CREATE TABLE IF NOT EXISTS room_tasks (
    id                       TEXT PRIMARY KEY,
    room_id                  TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    poster_workspace_id      TEXT NOT NULL,
    title                    TEXT NOT NULL,
    description              TEXT NOT NULL DEFAULT '',
    size                     TEXT NOT NULL,
    points                   BIGINT NOT NULL CHECK (points > 0),
    claimant_workspace_id    TEXT,  -- NULL: open; only the claimant itself ever sets it
    claimed_at               TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((claimant_workspace_id IS NULL) = (claimed_at IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_room_tasks_room ON room_tasks (room_id, created_at);
CREATE INDEX IF NOT EXISTS idx_room_tasks_poster ON room_tasks (poster_workspace_id);
CREATE INDEX IF NOT EXISTS idx_room_tasks_claimant ON room_tasks (claimant_workspace_id);
