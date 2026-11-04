-- 0310_scan_reports.sql — B52.25: Scan, part 6. The report writer, the compliance check and the review queue.
--
-- One row per report the writer drafted from a model's finding: the draft (its four sections, every figure with the
-- engine's point or backtest behind it, its sources and the "How this report was made" box), who wrote it (the model
-- or the fixed template), what the code rules and the second model said, and where it is in the queue. A draft the
-- check refused stays refused; one that passed waits as pending until a person approves or rejects it, and only an
-- approved report is published. The database refuses a decision that names no person.
CREATE TABLE IF NOT EXISTS scan_reports (
    id           BIGSERIAL PRIMARY KEY,
    series       TEXT NOT NULL CHECK (series <> ''),
    finding_at   TIMESTAMPTZ NOT NULL,
    backtest_id  BIGINT NOT NULL REFERENCES scan_backtests(id),
    draft        JSONB NOT NULL CHECK (jsonb_typeof(draft -> 'box') = 'object'),
    writer       TEXT NOT NULL CHECK (writer IN ('model', 'template')),
    model_check  TEXT NOT NULL CHECK (model_check <> ''),
    status       TEXT NOT NULL CHECK (status IN ('refused', 'pending', 'approved', 'rejected')),
    refusal      TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_by  TEXT,
    reviewed_at  TIMESTAMPTZ,
    review_note  TEXT NOT NULL DEFAULT '',
    UNIQUE (series, finding_at),
    CONSTRAINT scan_reports_refused_says_why CHECK (status <> 'refused' OR (refusal IS NOT NULL AND refusal <> '')),
    CONSTRAINT scan_reports_decided_by_a_person CHECK (status NOT IN ('approved', 'rejected') OR (
        reviewed_by IS NOT NULL AND reviewed_by <> '' AND reviewed_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS scan_reports_status_idx ON scan_reports (status, created_at DESC);
