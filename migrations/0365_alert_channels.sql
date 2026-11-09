-- 0365_alert_channels.sql — B28.338: the owner chooses where alerts go — push, a signed webhook or Slack.
--
-- alert_channels:   each place the workspace's alerts are sent. push goes to the owner's phones and browsers; webhook
--                   POSTs the alert as JSON to url, signed with secret; slack POSTs it to a Slack incoming webhook.
--                   Email waits for an email integration, so there is no email kind yet.
-- alert_deliveries: one row per alert per channel, queued the minute the alert is raised (economy/alert_channels.go).
--                   The unique key keeps each alert to one delivery on each channel; a channel added later is sent
--                   only the alerts raised after it.

CREATE TABLE IF NOT EXISTS alert_channels (
    id           TEXT PRIMARY KEY,                                   -- ach_<uuid>
    workspace_id TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('push', 'webhook', 'slack')),
    url          TEXT NOT NULL DEFAULT '',                           -- '' for push
    secret       TEXT NOT NULL DEFAULT '',                           -- a webhook's signing secret; '' otherwise
    added_by     TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, kind, url)
);

CREATE TABLE IF NOT EXISTS alert_deliveries (
    id           TEXT PRIMARY KEY,                                   -- ald_<uuid>
    workspace_id TEXT NOT NULL,
    channel_id   TEXT NOT NULL REFERENCES alert_channels (id) ON DELETE CASCADE,
    alert_kind   TEXT NOT NULL CHECK (alert_kind IN ('limit', 'unusual_spend')),
    alert_id     TEXT NOT NULL,                                      -- a spend_alerts or agent_spend_alerts id
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'failed')),
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at      TIMESTAMPTZ,
    UNIQUE (channel_id, alert_kind, alert_id)
);
CREATE INDEX IF NOT EXISTS idx_alert_deliveries_workspace ON alert_deliveries (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_alert_deliveries_queued ON alert_deliveries (created_at) WHERE status = 'queued';
