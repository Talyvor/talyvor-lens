-- 0308_scan_collectors.sql — B52.21: Scan, part 2. The energy collectors re-run safely.
--
-- A collector re-reads overlapping windows of its source (and backfills five years on its first run), so one source's
-- figure for one series at one time is stored once: a re-run stores nothing new.
CREATE UNIQUE INDEX IF NOT EXISTS scan_points_observed_once ON scan_points (source_id, series, observed_at)
    WHERE kind = 'observed';
