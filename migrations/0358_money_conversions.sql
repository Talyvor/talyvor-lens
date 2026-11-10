-- B56.23 — an agent converts between its currency accounts at its plan's FX margin.
--
-- A quote prices selling an amount from one of the agent's currency accounts for another of its currencies: the
-- partner's reference rate, the plan's margin (taken in the currency sold) and what it buys. It converts once, within
-- its life, under one idempotency key, and then keeps the key, the money entry that moved both legs and what the
-- agent's rules counted it as.

CREATE TABLE IF NOT EXISTS money_conversions (
    id              TEXT PRIMARY KEY,                             -- mfx_<uuid>
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL,
    from_account_id TEXT NOT NULL REFERENCES money_accounts (id), -- the agent's account in the currency sold
    to_account_id   TEXT NOT NULL REFERENCES money_accounts (id), -- the agent's account in the currency bought
    sell_minor      BIGINT NOT NULL CHECK (sell_minor > 0),       -- what leaves the account, the margin included
    sell_currency   TEXT NOT NULL,
    margin_bps      BIGINT NOT NULL CHECK (margin_bps BETWEEN 0 AND 10000),
    margin_minor    BIGINT NOT NULL CHECK (margin_minor >= 0),    -- Talyvor's, in the currency sold
    buy_minor       BIGINT NOT NULL CHECK (buy_minor > 0),
    buy_currency    TEXT NOT NULL,
    reference_rate  TEXT NOT NULL,
    rate            TEXT NOT NULL,
    funding         TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    partner         TEXT NOT NULL,
    partner_ref     TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'quoted' CHECK (status IN ('quoted', 'converted')),
    idempotency_key TEXT,
    entry_id        TEXT NOT NULL DEFAULT '',
    value_ulxc      BIGINT NOT NULL DEFAULT 0,
    expires_at      TIMESTAMPTZ NOT NULL,
    converted_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'converted') = (idempotency_key IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_conversions_key ON money_conversions (workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_money_conversions_agent ON money_conversions (agent_id, created_at);
