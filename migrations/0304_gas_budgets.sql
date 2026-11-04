-- 0304_gas_budgets.sql — B48.5: each workspace's budget for the network fees a paymaster pays for its agents.
--
-- Per workspace and chain, in wei of the chain's coin: the budget is LENS_GAS_BUDGET_WEI plus every top-up
-- an admin made; each sponsored operation holds its most possible fee when it is sent and is charged what the chain took once
-- it is on the chain. An operation is held under op_ref, its hash or the sender's idempotency key for it; user_op_hash
-- is set once it may reach the chain, and from then on a sweep charges it from its receipt, released or not. gas_budgets is the row a hold locks, and remembers when the owner was asked to top up, so they
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
    op_ref       TEXT        NOT NULL,
    user_op_hash TEXT,
    workspace_id TEXT        NOT NULL,
    held_wei     BIGINT      NOT NULL CHECK (held_wei > 0),
    charged_wei  BIGINT      CHECK (charged_wei >= 0),
    released_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at   TIMESTAMPTZ,
    PRIMARY KEY (chain_id, op_ref),
    FOREIGN KEY (workspace_id, chain_id) REFERENCES gas_budgets (workspace_id, chain_id),
    CHECK (charged_wei IS NULL OR released_at IS NULL)
);

CREATE INDEX IF NOT EXISTS gas_sponsorships_workspace ON gas_sponsorships (workspace_id, chain_id);
CREATE INDEX IF NOT EXISTS gas_sponsorships_unsettled ON gas_sponsorships (created_at)
    WHERE charged_wei IS NULL AND user_op_hash IS NOT NULL;
