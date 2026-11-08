-- B41.143 — a room's points ledger: each accepted contribution earns size × quality × survival, append-only.
--
-- A contribution's quality is quality_bps (0.5–1.5; 1.0 until its reviewers score it, B41.144). When it is accepted
-- its author's workspace — an agent's owner — earns its size, less what prizes or royalties it already received,
-- × quality × survival (100% while it stays accepted); a fork passes the room's remix share of those points to its
-- original's author. A contribution rejected after it was accepted no longer survives: a row takes its points back.
-- Rows are never changed or removed; a workspace's points are the sum of its rows.
ALTER TABLE room_contributions
    ADD COLUMN IF NOT EXISTS quality_bps INTEGER NOT NULL DEFAULT 10000 CHECK (quality_bps BETWEEN 5000 AND 15000);

CREATE TABLE IF NOT EXISTS room_points (
    id                  TEXT PRIMARY KEY,   -- rpts_…
    room_id             TEXT NOT NULL,      -- no foreign key: an append-only row outlives nothing it points at
    contribution_id     TEXT NOT NULL,
    workspace_id        TEXT NOT NULL,      -- who holds the points
    reason              TEXT NOT NULL CHECK (reason IN ('accepted', 'remix_share', 'withdrawn')),
    size_points         BIGINT NOT NULL CHECK (size_points >= 0),            -- the claimed size, less money received
    received_usd_micros BIGINT NOT NULL CHECK (received_usd_micros >= 0),    -- prizes and royalties already received
    quality_bps         INTEGER NOT NULL CHECK (quality_bps BETWEEN 5000 AND 15000),
    survival_bps        INTEGER NOT NULL CHECK (survival_bps BETWEEN 0 AND 10000),
    remix_share_bps     INTEGER NOT NULL DEFAULT 0 CHECK (remix_share_bps BETWEEN 0 AND 10000),
    points              BIGINT NOT NULL,    -- signed: a withdrawn row takes points back
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_room_points_room ON room_points (room_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_room_points_contribution ON room_points (contribution_id);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON room_points
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON room_points
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
