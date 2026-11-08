-- B41.142 — a room's rate card: the points each size of work earns, fixed on a contribution before the work starts.
--
-- The owner publishes the pool's points and each size's estimated hours and points per hour of its market band; a
-- size is worth hours × points per hour and never more than 5% of the pool. A contribution's claimed size and its
-- points are copied onto it when claimed, so a later rate card does not change them, and once it is accepted they
-- are never raised. Gated on room_equity (RED), like the room's shared ownership.
CREATE TABLE IF NOT EXISTS room_rate_cards (
    room_id         TEXT PRIMARY KEY REFERENCES rooms (id) ON DELETE CASCADE,
    pool_points     BIGINT NOT NULL CHECK (pool_points > 0),
    sizes           JSONB NOT NULL,
    set_by_user_id  TEXT NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE room_contributions
    ADD COLUMN IF NOT EXISTS size   TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS points BIGINT NOT NULL DEFAULT 0 CHECK (points >= 0);
