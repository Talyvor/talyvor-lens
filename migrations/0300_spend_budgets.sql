-- 0300_spend_budgets.sql — B53.4: weekly and monthly budgets, with alerts at 50, 80 and 100 percent.
--
-- spend_budgets: what the owner means to spend in a week (from Monday) or a month, in their own time zone — overall or
--                on one category (B53.3), for their own wallet (agent_id ''), one agent's, or every wallet ('all').
--                Progress is never stored: it is worked out from the postings each time it is read.
-- spend_alerts:  one row each time spending reaches 50, 80 or 100% of a budget, a card's cap or an agent's limit.
--                The unique key makes each one fire once in its period; the next period has another period_start.

CREATE TABLE IF NOT EXISTS spend_budgets (
    id           TEXT PRIMARY KEY,                                   -- bdg_<uuid>
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL DEFAULT '',                           -- '': the owner's own wallet; 'all': every wallet
    category     TEXT NOT NULL DEFAULT '' CHECK (category ~ '^[a-z_]*$'), -- '': all spending
    period       TEXT NOT NULL CHECK (period IN ('week', 'month')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency     TEXT NOT NULL,
    timezone     TEXT NOT NULL DEFAULT 'UTC',                        -- IANA; where its week and month begin
    set_by       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, agent_id, category, period, currency)
);

CREATE TABLE IF NOT EXISTS spend_alerts (
    id           TEXT PRIMARY KEY,                                   -- sal_<uuid>
    workspace_id TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('budget', 'card', 'agent_limit')),
    subject      TEXT NOT NULL,                                      -- the budget's id, the card's, or the agent's
    agent_id     TEXT NOT NULL DEFAULT '',
    category     TEXT NOT NULL DEFAULT '',
    period       TEXT NOT NULL DEFAULT '',                           -- week, month; an agent's hourly … monthly limit
    period_start TIMESTAMPTZ NOT NULL,
    percent      INTEGER NOT NULL CHECK (percent IN (50, 80, 100)),
    spent_minor  BIGINT NOT NULL,
    limit_minor  BIGINT NOT NULL,
    currency     TEXT NOT NULL,                                      -- LXC for an agent's limit, counted in µLXC
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, kind, subject, period, period_start, percent)
);
CREATE INDEX IF NOT EXISTS idx_spend_alerts_workspace ON spend_alerts (workspace_id, created_at DESC);
