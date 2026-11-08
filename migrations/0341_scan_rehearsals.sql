-- 0341_scan_rehearsals.sql — B52.40: Scan's report rehearsed on a test (synthetic) workspace, for the nightly.
--
-- One row per report the writer drafted for a test workspace from Lens's recorded data (internal/scan/rehearsal.go):
-- the draft as scan_reports holds it, the finding it was written from, the test reviewer's decision, and once approved
-- the planted outcome and whether it fell inside the report's range. Kept apart from scan_reports and
-- scan_track_record, so nothing here is ever in the public library or the public track record.
CREATE TABLE IF NOT EXISTS scan_rehearsals (
    id            BIGSERIAL PRIMARY KEY,
    workspace_id  TEXT NOT NULL CHECK (workspace_id <> ''),
    series        TEXT NOT NULL CHECK (series <> ''),
    finding       JSONB NOT NULL CHECK (jsonb_typeof(finding) = 'object'),
    draft         JSONB NOT NULL CHECK (jsonb_typeof(draft -> 'box') = 'object'),
    writer        TEXT NOT NULL CHECK (writer IN ('model', 'template')),
    model_check   TEXT NOT NULL CHECK (model_check <> ''),
    status        TEXT NOT NULL CHECK (status IN ('refused', 'pending', 'approved')),
    refusal       TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_by   TEXT,
    reviewed_at   TIMESTAMPTZ,
    review_note   TEXT NOT NULL DEFAULT '',
    outcome_value DOUBLE PRECISION,
    outcome_at    TIMESTAMPTZ,
    hit           BOOLEAN,
    scored_at     TIMESTAMPTZ,
    CONSTRAINT scan_rehearsals_refused_says_why CHECK (status <> 'refused' OR (refusal IS NOT NULL AND refusal <> '')),
    CONSTRAINT scan_rehearsals_approved_by_a_reviewer CHECK (status <> 'approved' OR (
        reviewed_by IS NOT NULL AND reviewed_by <> '' AND reviewed_at IS NOT NULL)),
    CONSTRAINT scan_rehearsals_scored_once_approved CHECK (scored_at IS NULL OR (status = 'approved' AND hit IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS scan_rehearsals_workspace_idx ON scan_rehearsals (workspace_id, id);
