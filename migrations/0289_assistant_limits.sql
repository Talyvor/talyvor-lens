-- B41.114 — limits per assistant: the most each client ("chat", "voice", "mcp:<client id>", "key:<api key id>") may
-- prepare in payment requests in a UTC day, in each currency's minor units. No row: no limit of its own.
CREATE TABLE IF NOT EXISTS assistant_limits (
    workspace_id  TEXT        NOT NULL,
    client        TEXT        NOT NULL,
    currency      TEXT        NOT NULL,
    per_day_minor BIGINT      NOT NULL CHECK (per_day_minor > 0),
    updated_by    TEXT        NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, client, currency)
);
