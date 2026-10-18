-- B39.9 — an operator's request limits for one workspace, set through PUT /v1/admin/workspaces/{wsID}/rate-limits.
-- A workspace with no row keeps the default 100 a second, 1,000 a minute and 10,000 an hour.
CREATE TABLE IF NOT EXISTS workspace_rate_limits (
    workspace_id        TEXT PRIMARY KEY,
    requests_per_second INTEGER NOT NULL CHECK (requests_per_second > 0),
    requests_per_minute INTEGER NOT NULL CHECK (requests_per_minute > 0),
    requests_per_hour   INTEGER NOT NULL CHECK (requests_per_hour > 0),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
