-- 0314_scan_track_record.sql — B52.26: Scan, part 7. The track record, complete and public.
--
-- One row per published report, written in the same statement that approves it: what the report observed (the
-- reading, its normal) and the range it gave for a later date, with the report's title and sources. When the series'
-- reading for that date arrives it is scored once: the outcome, and whether it fell inside the range. The row is
-- self-contained (no foreign keys), so nothing elsewhere can change what it says, and the database refuses every
-- other change: no published figure is ever edited, a scored row is never rescored, and no row is ever deleted.
CREATE TABLE IF NOT EXISTS scan_track_record (
    id            BIGSERIAL PRIMARY KEY,
    report_id     BIGINT NOT NULL UNIQUE,
    series        TEXT NOT NULL CHECK (series <> ''),
    title         TEXT NOT NULL,
    sources       JSONB NOT NULL CHECK (jsonb_typeof(sources) = 'array'),
    published_at  TIMESTAMPTZ NOT NULL,
    observed_at   TIMESTAMPTZ NOT NULL,
    value         DOUBLE PRECISION NOT NULL,
    unit          TEXT NOT NULL,
    normal_low    DOUBLE PRECISION NOT NULL,
    normal_high   DOUBLE PRECISION NOT NULL,
    outlook_at    TIMESTAMPTZ NOT NULL,
    outlook_low   DOUBLE PRECISION NOT NULL,
    outlook_high  DOUBLE PRECISION NOT NULL,
    outcome_point BIGINT,
    outcome_at    TIMESTAMPTZ,
    outcome_value DOUBLE PRECISION,
    hit           BOOLEAN,
    scored_at     TIMESTAMPTZ,
    CONSTRAINT scan_track_record_scored_whole CHECK (
        (outcome_point IS NULL AND outcome_at IS NULL AND outcome_value IS NULL AND hit IS NULL AND scored_at IS NULL) OR
        (outcome_point IS NOT NULL AND outcome_at IS NOT NULL AND outcome_value IS NOT NULL AND hit IS NOT NULL AND scored_at IS NOT NULL)),
    CONSTRAINT scan_track_record_hit_is_the_range CHECK (hit IS NULL OR hit = (outcome_value BETWEEN outlook_low AND outlook_high))
);
CREATE INDEX IF NOT EXISTS scan_track_record_unscored_idx ON scan_track_record (outlook_at) WHERE scored_at IS NULL;

CREATE OR REPLACE FUNCTION scan_track_record_never_edited() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    outcome CONSTANT text[] := ARRAY['outcome_point', 'outcome_at', 'outcome_value', 'hit', 'scored_at'];
BEGIN
    -- The one change allowed: scoring an unscored row, every published column as it was.
    IF TG_OP = 'UPDATE' THEN
        IF OLD.scored_at IS NULL AND (to_jsonb(OLD) - outcome) = (to_jsonb(NEW) - outcome) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'scan_track_record: a published record is never edited (% refused)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

DROP TRIGGER IF EXISTS scan_track_record_never_edited ON scan_track_record;
CREATE TRIGGER scan_track_record_never_edited BEFORE UPDATE OR DELETE ON scan_track_record
    FOR EACH ROW EXECUTE FUNCTION scan_track_record_never_edited();
DROP TRIGGER IF EXISTS scan_track_record_never_truncated ON scan_track_record;
CREATE TRIGGER scan_track_record_never_truncated BEFORE TRUNCATE ON scan_track_record
    FOR EACH STATEMENT EXECUTE FUNCTION scan_track_record_never_edited();
