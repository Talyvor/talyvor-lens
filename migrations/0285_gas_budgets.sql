-- 0285_gas_budgets.sql — B48.5: each workspace's budget for the network fees a paymaster pays for its agents.
--
-- Per workspace and chain, in wei of the chain's coin: the budget is LENS_GAS_BUDGET_WEI plus every top-up the owner
-- made; each sponsored operation holds its most possible fee when it is sent and is charged what the chain took once
-- it is on the chain. gas_budgets is the row a hold locks, and remembers when the owner was asked to top up, so they
-- are asked once until they do.

CREATE TABLE IF NOT EXISTS gas_budgets (
    workspace_id    TEXT        NOT NULL,
    chain_id        BIGINT      NOT NULL CHECK (chain_id > 0),
    top_up_asked_at TIMESTAMPTZ,
    PRIMARY KEY (workspace_id, chain_id)
);

CREATE TABLE IF NOT EXISTS gas_budget_top_ups (
    id           BIGSERIAL   PRIMARY KEY,
    workspace_id TEXT        NOT NULL,
    chain_id     BIGINT      NOT NULL,
    wei          BIGINT      NOT NULL CHECK (wei > 0),
    user_id      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, chain_id) REFERENCES gas_budgets (workspace_id, chain_id)
);

CREATE TABLE IF NOT EXISTS gas_sponsorships (
    chain_id     BIGINT      NOT NULL,
    user_op_hash TEXT        NOT NULL,
    workspace_id TEXT        NOT NULL,
    held_wei     BIGINT      NOT NULL CHECK (held_wei > 0),
    charged_wei  BIGINT      CHECK (charged_wei >= 0),
    released_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at   TIMESTAMPTZ,
    PRIMARY KEY (chain_id, user_op_hash),
    FOREIGN KEY (workspace_id, chain_id) REFERENCES gas_budgets (workspace_id, chain_id),
    CHECK (charged_wei IS NULL OR released_at IS NULL)
);

CREATE INDEX IF NOT EXISTS gas_sponsorships_workspace ON gas_sponsorships (workspace_id, chain_id);
