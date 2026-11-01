-- 0298_money_allowances.sql — B30.27: a monthly allowance in money for each agent.
--
-- One allowance per agent and currency: on day_of_month (1–28, so every month has it) the company's account in that
-- currency pays amount_minor into the agent's, and with sweep_back the agent's unspent money goes back first. Each
-- month it ran is one money_allowance_runs row naming the entries it posted, so a month runs once. A stopped
-- allowance keeps its row and its runs; setting it again starts it.

CREATE TABLE IF NOT EXISTS money_allowances (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT        NOT NULL,
    agent_id     TEXT        NOT NULL,
    currency     TEXT        NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD')),
    amount_minor BIGINT      NOT NULL CHECK (amount_minor > 0),
    day_of_month SMALLINT    NOT NULL CHECK (day_of_month BETWEEN 1 AND 28),
    sweep_back   BOOLEAN     NOT NULL DEFAULT false,
    funding      TEXT        NOT NULL DEFAULT 'test' CHECK (funding IN ('test', 'live')),
    next_run_at  TIMESTAMPTZ NOT NULL,
    stopped_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, currency)
);

CREATE INDEX IF NOT EXISTS idx_money_allowances_due ON money_allowances (next_run_at) WHERE stopped_at IS NULL;

CREATE TABLE IF NOT EXISTS money_allowance_runs (
    allowance_id   TEXT        NOT NULL REFERENCES money_allowances (id),
    period         TEXT        NOT NULL, -- the month it ran for, YYYY-MM
    swept_minor    BIGINT      NOT NULL DEFAULT 0,
    funded_minor   BIGINT      NOT NULL DEFAULT 0,
    sweep_entry_id TEXT REFERENCES money_entries (id),
    fund_entry_id  TEXT REFERENCES money_entries (id),
    status         TEXT        NOT NULL CHECK (status IN ('funded', 'refused')),
    detail         TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (allowance_id, period)
);
