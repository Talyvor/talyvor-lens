-- 2611190203a_low_balance_alerts.sql — B28.334: alert when an agent's balance falls below a threshold.
--
-- balance_thresholds: the owner's threshold for one agent's balance in one currency.
-- low_balance_alerts: one row for each hour an agent's balance is below its threshold. The unique key makes a balance
--                     checked on every request, or every minute, raise one alert an hour, never one each time.

CREATE TABLE IF NOT EXISTS balance_thresholds (
    id              TEXT PRIMARY KEY,                                -- lbt_<uuid>
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL,
    currency        TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$' OR currency = 'USDC'),
    threshold_minor BIGINT NOT NULL CHECK (threshold_minor > 0),
    set_by          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, agent_id, currency)
);

CREATE TABLE IF NOT EXISTS low_balance_alerts (
    id              TEXT PRIMARY KEY,                                -- lba_<uuid>
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL,
    currency        TEXT NOT NULL,
    hour_start      TIMESTAMPTZ NOT NULL,                            -- the UTC hour it was below
    balance_minor   BIGINT NOT NULL,                                 -- what the agent held when it was raised
    threshold_minor BIGINT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, agent_id, currency, hour_start)
);
CREATE INDEX IF NOT EXISTS idx_low_balance_alerts_workspace ON low_balance_alerts (workspace_id, created_at DESC);

-- A low-balance alert goes to the owner's alert channels (B28.338) like a limit alert does.
ALTER TABLE alert_deliveries DROP CONSTRAINT IF EXISTS alert_deliveries_alert_kind_check;
ALTER TABLE alert_deliveries ADD CONSTRAINT alert_deliveries_alert_kind_check
    CHECK (alert_kind IN ('limit', 'unusual_spend', 'low_balance'));
