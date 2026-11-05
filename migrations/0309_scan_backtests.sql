-- 0309_scan_backtests.sql — B52.24: Scan, part 5. What is unusual and how sure we are.
--
-- One row per backtest of a series' model: how the model did on the series' own history (outlooks checked, the share
-- that called the side of the normal right, its error against the normal's alone, how often its 80% range held the
-- outcome), whether it passed and why, and the parameters it uses until the next run. A series whose latest backtest
-- failed is never assessed, so its model never feeds a report. finding is the latest point read against the normal,
-- with the derived point behind every figure, for the operator's page.
CREATE TABLE IF NOT EXISTS scan_backtests (
    id            BIGSERIAL PRIMARY KEY,
    series        TEXT NOT NULL CHECK (series <> ''),
    model         TEXT NOT NULL CHECK (model <> ''),
    method        TEXT NOT NULL CHECK (method <> ''),
    ran_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    data_from     TIMESTAMPTZ,
    data_to       TIMESTAMPTZ,
    tested        INTEGER NOT NULL,
    independent   DOUBLE PRECISION NOT NULL,
    hit_rate      DOUBLE PRECISION NOT NULL,
    error         DOUBLE PRECISION NOT NULL,
    normal_error  DOUBLE PRECISION NOT NULL,
    coverage      DOUBLE PRECISION NOT NULL,
    passed        BOOLEAN NOT NULL,
    verdict       TEXT NOT NULL CHECK (verdict <> ''),
    persistence   DOUBLE PRECISION NOT NULL,
    miss_low      DOUBLE PRECISION NOT NULL,
    miss_high     DOUBLE PRECISION NOT NULL,
    finding       JSONB,
    CONSTRAINT scan_backtests_finding_only_when_passed CHECK (finding IS NULL OR passed)
);
CREATE INDEX IF NOT EXISTS scan_backtests_series_idx ON scan_backtests (series, ran_at DESC, id DESC);
