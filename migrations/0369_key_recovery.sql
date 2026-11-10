-- 0369: B55.48 — a workspace left with no live key gets one back, by its recovery code or after 72 hours.
--
-- The code itself is never kept: only sha256(workspace_id || ':' || the normalised code). A workspace has at most one
-- code not yet used; making a new one deletes it, and a used one stays as the record of its use.
CREATE TABLE IF NOT EXISTS workspace_recovery_codes (
    workspace_id TEXT        NOT NULL,
    hash         TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    used_at      TIMESTAMPTZ,
    PRIMARY KEY (workspace_id, hash)
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_recovery_codes_live ON workspace_recovery_codes (workspace_id) WHERE used_at IS NULL;

-- Each wrong code, so five an hour per workspace and per IP are the most anyone can try.
CREATE TABLE IF NOT EXISTS key_recovery_failures (
    workspace_id TEXT        NOT NULL,
    ip           TEXT        NOT NULL DEFAULT '',
    failed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS key_recovery_failures_workspace ON key_recovery_failures (workspace_id, failed_at);
CREATE INDEX IF NOT EXISTS key_recovery_failures_ip ON key_recovery_failures (ip, failed_at) WHERE ip <> '';

-- A recovery without the code: a key can be added from ready_at, by user_id alone, for 24 hours, unless it is cancelled.
CREATE TABLE IF NOT EXISTS recovery_requests (
    id           TEXT        PRIMARY KEY,
    workspace_id TEXT        NOT NULL,
    user_id      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at     TIMESTAMPTZ NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'cancelled', 'completed', 'expired')),
    cancelled_by TEXT        NOT NULL DEFAULT '',
    completed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS recovery_requests_one_waiting ON recovery_requests (workspace_id) WHERE status = 'waiting';
