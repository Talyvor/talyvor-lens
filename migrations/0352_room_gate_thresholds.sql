-- B41.146 — the Gate tracker's thresholds for a room: shipped, used, paid.
--
-- Talyvor's thresholds are the floor (internal/rooms/gate.go): a public beta live 8 weeks in a row; 100 people not
-- linked to the owner active in 6 of the last 8 weeks; $1,000 from 10 payers not linked to the owner. A room's owner
-- may set stricter values, never looser, and once set a value is never lowered: the room's contributors read their
-- progress against it. No row: the room uses the floor.
CREATE TABLE IF NOT EXISTS room_gate_thresholds (
    room_id           TEXT PRIMARY KEY REFERENCES rooms (id) ON DELETE CASCADE,
    shipped_weeks     INTEGER NOT NULL CHECK (shipped_weeks >= 8),
    used_people       INTEGER NOT NULL CHECK (used_people >= 100),
    used_active_weeks INTEGER NOT NULL CHECK (used_active_weeks BETWEEN 6 AND 8),
    paid_usd_micros   BIGINT NOT NULL CHECK (paid_usd_micros >= 1000000000),
    paid_payers       INTEGER NOT NULL CHECK (paid_payers >= 10),
    set_by_user_id    TEXT NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
