-- B41.141 — shared ownership in a room (intent only): the owner switches it on and sets the contributor pool.
--
-- One row per room, written by its owner. It records intent and has no legal effect: no shares, options or rights
-- arise from it. Capability room_equity is RED, so only synthetic workspaces see it until an operator records the
-- lawyer's clearance of the wording.
CREATE TABLE IF NOT EXISTS room_shared_ownership (
    room_id         TEXT PRIMARY KEY REFERENCES rooms (id) ON DELETE CASCADE,
    enabled         BOOLEAN NOT NULL DEFAULT false,
    pool_bps        INTEGER NOT NULL DEFAULT 2500 CHECK (pool_bps BETWEEN 1000 AND 5000),
    set_by_user_id  TEXT NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
