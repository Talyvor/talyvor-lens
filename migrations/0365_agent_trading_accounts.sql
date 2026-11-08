-- 0365_agent_trading_accounts.sql — B30.48: each agent's trading account at the broker partner.
--
-- One per agent and currency, opened at the broker partner (the Test broker until trade_equities is cleared) under
-- the broker's reference for it. The cash it holds is kept in the ledger in an account of its own (purpose broker),
-- funded from the agent's account in the same currency: only a transfer to or from it, or a trade, moves it.
CREATE TABLE IF NOT EXISTS agent_trading_accounts (
    id                  TEXT PRIMARY KEY,
    workspace_id        TEXT NOT NULL,
    agent_id            TEXT NOT NULL,
    currency            TEXT NOT NULL CHECK (currency = 'USD'),
    partner             TEXT NOT NULL,
    partner_account_ref TEXT NOT NULL,
    money_account_id    TEXT NOT NULL REFERENCES money_accounts (id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, agent_id, currency)
);

ALTER TABLE money_accounts DROP CONSTRAINT IF EXISTS money_accounts_purpose_check;
ALTER TABLE money_accounts ADD CONSTRAINT money_accounts_purpose_check
    CHECK (purpose IN ('company', 'agent', 'pot', 'hold', 'suspense', 'partner', 'revenue', 'chain', 'broker'));
