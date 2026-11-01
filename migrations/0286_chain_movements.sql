-- 0286_chain_movements.sql — B48.6: every movement of an agent's crypto wallet seen, confirmed and reconciled.
--
-- Each token transfer into or out of an agent's smart account (agent_smart_accounts) is one row, keyed by the chain's
-- own name for it — network, transaction, its place among the transaction's logs and which way it went — so the
-- provider's webhook and the node's logs telling of the same transfer, however often, make one row. It is posted to
-- the ledger only once it has its network's confirmations (status confirmed), and watched until it is deep enough not
-- to move (final); a reorg that takes it off the chain posts the reversal (reversed), and it is posted again if it
-- comes back. posted counts the times it was posted, so each posting and reversal has its own idempotency key.
CREATE TABLE IF NOT EXISTS chain_movements (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    agent_id      TEXT NOT NULL,
    network       TEXT NOT NULL,
    tx_hash       TEXT NOT NULL,                  -- 0x, lower case
    log_position  INT NOT NULL,                   -- its place among the transaction's logs, which a reorg does not move
    direction     TEXT NOT NULL CHECK (direction IN ('in', 'out')),
    wallet        TEXT NOT NULL,                  -- the agent's account, EIP-55
    counterparty  TEXT NOT NULL,                  -- who sent it or was paid, EIP-55
    token         TEXT NOT NULL,
    asset         TEXT NOT NULL,
    amount_minor  BIGINT NOT NULL CHECK (amount_minor > 0),
    funding       TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    source        TEXT NOT NULL CHECK (source IN ('webhook', 'poll')), -- who told Lens first
    status        TEXT NOT NULL DEFAULT 'seen' CHECK (status IN ('seen', 'confirmed', 'reversed', 'final')),
    block_number  BIGINT,
    block_hash    TEXT NOT NULL DEFAULT '',
    confirmations BIGINT NOT NULL DEFAULT 0,
    posted        INT NOT NULL DEFAULT 0,
    entry_id      TEXT NOT NULL DEFAULT '',       -- the money entry that last posted or reversed it
    seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (network, tx_hash, log_position, direction)
);
CREATE INDEX IF NOT EXISTS idx_chain_movements_watch ON chain_movements (network) WHERE status <> 'final';
CREATE INDEX IF NOT EXISTS idx_chain_movements_agent ON chain_movements (workspace_id, agent_id, seen_at DESC);

-- How far the node's logs were read on each network: the fallback for a webhook that never came.
CREATE TABLE IF NOT EXISTS chain_index_cursors (
    network    TEXT PRIMARY KEY,
    next_block BIGINT NOT NULL
);

-- Each wallet's nightly reconciliation: what the chain says it holds against the ledger's mirror of it and the
-- movements seen but not yet confirmed. A difference is raised to the operator.
CREATE TABLE IF NOT EXISTS chain_reconciliations (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL,
    agent_id         TEXT NOT NULL,
    network          TEXT NOT NULL,
    wallet           TEXT NOT NULL,
    asset            TEXT NOT NULL,
    block_number     BIGINT NOT NULL,
    chain_minor      BIGINT NOT NULL,
    ledger_minor     BIGINT NOT NULL,
    pending_minor    BIGINT NOT NULL,
    difference_minor BIGINT NOT NULL,             -- chain minus ledger and pending: 0 when they agree
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_chain_reconciliations_workspace ON chain_reconciliations (workspace_id, created_at DESC);
