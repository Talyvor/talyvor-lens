-- 0311_scan_watches.sql — B52.27: Scan agents, watches that run on their own.
--
-- A watch is a Scan agent's: the series its owner picked (each a dataset in a region, e.g. eia.stocks.cushing) and the
-- owner's own idea of unusual — outside the five-year range for the same weeks ('range'), or at least z standard
-- deviations from the five-year normal ('z'). An alert is sent once per crossing: the first unusual point after a usual
-- one, the unique key keeping it once however often the watch runs. Each alert records what the agent's wallet paid
-- for it and the request it paid under, so a run is charged exactly once. last_refusal says why the watch did not run
-- last time (the agent paused, or out of credit), until it runs again.
CREATE TABLE IF NOT EXISTS scan_watches (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    agent_id      TEXT NOT NULL,
    series        TEXT[] NOT NULL CHECK (cardinality(series) BETWEEN 1 AND 50),
    rule          TEXT NOT NULL CHECK (rule IN ('range', 'z')),
    z             DOUBLE PRECISION CHECK ((rule = 'z') = (z IS NOT NULL) AND (z IS NULL OR z > 0)),
    active        BOOLEAN NOT NULL DEFAULT TRUE,
    last_run_at   TIMESTAMPTZ,
    last_refusal  TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS scan_watches_workspace_idx ON scan_watches (workspace_id, created_at);
CREATE INDEX IF NOT EXISTS scan_watches_active_idx ON scan_watches (active) WHERE active;

CREATE TABLE IF NOT EXISTS scan_watch_alerts (
    id            BIGSERIAL PRIMARY KEY,
    watch_id      TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    agent_id      TEXT NOT NULL,
    series        TEXT NOT NULL,
    observed_at   TIMESTAMPTZ NOT NULL,
    point_id      BIGINT NOT NULL,
    body          TEXT NOT NULL CHECK (body <> ''),
    finding       JSONB NOT NULL,
    attribution   TEXT[] NOT NULL,
    charged_ulxc  BIGINT NOT NULL CHECK (charged_ulxc >= 0),
    request_id    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (watch_id, series, observed_at)
);
CREATE INDEX IF NOT EXISTS scan_watch_alerts_workspace_idx ON scan_watch_alerts (workspace_id, created_at DESC);
