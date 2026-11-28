-- 2611281357d_wallet_webhooks.sql — B28.344: signed outbound webhooks for wallet events.
--
-- wallet_webhooks:          an owner's endpoint: its https url, the events it takes (none = every one) and its signing
--                           secret. After a rotation the previous secret still signs until previous_until, so the
--                           owner's server can move to the new one without missing an event.
-- wallet_webhook_events:    one event queued to one endpoint — a transfer, an approval asked for or decided, a limit or
--                           unusual-spend alert, a low balance. The unique key queues each once, however often the
--                           sweep runs; once answered 2xx it is delivered and never sent again.
-- wallet_webhook_attempts:  one row for each time an event was POSTed, with what the endpoint answered.

CREATE TABLE IF NOT EXISTS wallet_webhooks (
    id              TEXT PRIMARY KEY,                                -- wwh_<uuid>
    workspace_id    TEXT NOT NULL,
    url             TEXT NOT NULL,
    events          TEXT[] NOT NULL DEFAULT '{}',
    secret          TEXT NOT NULL,                                   -- whsec_<base64>
    previous_secret TEXT NOT NULL DEFAULT '',
    previous_until  TIMESTAMPTZ,
    added_by        TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at      TIMESTAMPTZ,
    UNIQUE (workspace_id, url)
);

CREATE TABLE IF NOT EXISTS wallet_webhook_events (
    id              TEXT PRIMARY KEY,                                -- msg_<uuid>: the webhook-id header
    workspace_id    TEXT NOT NULL,
    webhook_id      TEXT NOT NULL REFERENCES wallet_webhooks (id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    ref             TEXT NOT NULL,                                   -- the transfer, approval or alert it is about
    occurred_at     TIMESTAMPTZ NOT NULL,
    data            JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'delivered', 'failed')),
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT NOT NULL DEFAULT '',
    delivered_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (webhook_id, event_type, ref)
);
CREATE INDEX IF NOT EXISTS idx_wallet_webhook_events_due ON wallet_webhook_events (next_attempt_at) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS idx_wallet_webhook_events_workspace ON wallet_webhook_events (workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS wallet_webhook_attempts (
    id           BIGSERIAL PRIMARY KEY,
    event_id     TEXT NOT NULL REFERENCES wallet_webhook_events (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    status_code  INT NOT NULL DEFAULT 0,                             -- 0: no answer
    error        TEXT NOT NULL DEFAULT '',
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_wallet_webhook_attempts_event ON wallet_webhook_attempts (event_id, attempted_at);
