-- B28.125 — prompts scheduled from Chat, asked at the time set on an agent's own wallet.
--
-- A schedule asks provider/model the prompt once, or every day or week from its first time. The minute tick claims a
-- due schedule by advancing next_run_at (or ending it) in one transaction that holds its row, and only then asks the
-- model as the agent's key: neither a restart nor a second Lens asks a time twice. Each time asked is a run: answered,
-- with what the agent's wallet paid and the statement line it paid on, or refused with Lens's sentence.
CREATE TABLE IF NOT EXISTS prompt_schedules (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    prompt       TEXT NOT NULL CHECK (prompt <> ''),
    provider     TEXT NOT NULL,
    model        TEXT NOT NULL,
    every        TEXT NOT NULL CHECK (every IN ('once', 'day', 'week')),
    next_run_at  TIMESTAMPTZ,            -- NULL once it will not run again
    active       BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_prompt_schedules_due ON prompt_schedules (next_run_at) WHERE active;
CREATE INDEX IF NOT EXISTS idx_prompt_schedules_workspace ON prompt_schedules (workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS prompt_schedule_runs (
    schedule_id  TEXT NOT NULL REFERENCES prompt_schedules (id) ON DELETE CASCADE,
    ran_at       TIMESTAMPTZ NOT NULL,
    outcome      TEXT NOT NULL CHECK (outcome IN ('answered', 'refused')),
    answer       TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '',
    request_id   TEXT NOT NULL DEFAULT '',
    charged_ulxc BIGINT NOT NULL DEFAULT 0,
    entry_id     UUID,                   -- the agent's statement line the answer was charged on
    PRIMARY KEY (schedule_id, ran_at)
);
