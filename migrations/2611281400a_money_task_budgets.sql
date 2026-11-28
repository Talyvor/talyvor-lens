-- 2611281400a_money_task_budgets.sql — B28.310: a budget for one task that ends at a time (economy/money_task_budgets.go).
--
-- The owner gives an agent an amount in one currency for a task, until expires_at. Setting it pays the amount from the
-- company's account in the currency into the agent's (fund_entry_id). At expires_at, or when the owner ends it first,
-- the part not spent goes back in one entry (return_entry_id): the amount less what left the agent's account since it
-- was funded, and never more than the account then holds. A budget is returned once; one whose return was refused says
-- why in detail, and ending it again tries again.

CREATE TABLE IF NOT EXISTS money_task_budgets (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT        NOT NULL,
    agent_id        TEXT        NOT NULL,
    task            TEXT        NOT NULL CHECK (task <> ''),
    currency        TEXT        NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD')),
    amount_minor    BIGINT      NOT NULL CHECK (amount_minor > 0),
    funding         TEXT        NOT NULL DEFAULT 'test' CHECK (funding IN ('test', 'live')),
    idempotency_key TEXT        NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    fund_entry_id   TEXT        NOT NULL UNIQUE REFERENCES money_entries (id),
    status          TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'returned', 'refused')),
    spent_minor     BIGINT      NOT NULL DEFAULT 0,
    returned_minor  BIGINT      NOT NULL DEFAULT 0,
    return_entry_id TEXT REFERENCES money_entries (id),
    detail          TEXT        NOT NULL DEFAULT '',
    ended_at        TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_money_task_budgets_due ON money_task_budgets (expires_at) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_money_task_budgets_agent ON money_task_budgets (workspace_id, agent_id);
