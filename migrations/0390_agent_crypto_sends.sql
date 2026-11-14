-- 0390_agent_crypto_sends.sql — B48.9: an agent's smart account sends USDC to another Talyvor agent or to any address
-- (internal/economy/agent_crypto_sends.go).
--
-- agent_crypto_sends: one row per send, keyed by the caller's idempotency key, so a retry answers the same send and
--                     never pays twice. A first send to an address the workspace never paid waits for the owner
--                     (awaiting_owner) when the agent asked for it. Once the agent's key has signed, the row holds the
--                     signed user operation and its hash (kept before it is handed to the bundler, so a retry sends
--                     that same operation again and its nonce lets it land once), then the transaction's hash from its
--                     receipt. use_id is the spend permission's use the gate kept for it, deleted again when the
--                     operation is refused or fails on the chain.
CREATE TABLE IF NOT EXISTS agent_crypto_sends (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL CHECK (workspace_id <> ''),
    agent_id         TEXT NOT NULL CHECK (agent_id <> ''),
    idempotency_key  TEXT NOT NULL CHECK (idempotency_key <> ''),
    network          TEXT NOT NULL CHECK (network <> ''),
    token            TEXT NOT NULL CHECK (token <> ''),
    to_address       TEXT NOT NULL CHECK (to_address <> ''),
    to_kind          TEXT NOT NULL CHECK (to_kind IN ('agent', 'address')),
    to_label         TEXT NOT NULL DEFAULT '',
    to_agent_id      TEXT NOT NULL DEFAULT '',
    to_workspace_id  TEXT NOT NULL DEFAULT '',
    amount_minor     BIGINT NOT NULL CHECK (amount_minor > 0),
    max_fee_wei      TEXT NOT NULL DEFAULT '',
    fee_paid_by      TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL CHECK (status IN ('awaiting_owner', 'sending', 'sent', 'landed', 'failed', 'refused')),
    reason           TEXT NOT NULL DEFAULT '',
    requested_by     TEXT NOT NULL DEFAULT '',
    approved_by      TEXT NOT NULL DEFAULT '',
    use_id           BIGINT,
    signed_op        TEXT NOT NULL DEFAULT '',
    user_op_hash     TEXT NOT NULL DEFAULT '',
    tx_hash          TEXT NOT NULL DEFAULT '',
    carried_until    TIMESTAMPTZ, -- one caller at a time takes a send further, under this lease,
    carried_by       TEXT NOT NULL DEFAULT '', -- and only the caller holding it gives it up
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, agent_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS agent_crypto_sends_agent ON agent_crypto_sends (workspace_id, agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS agent_crypto_sends_to ON agent_crypto_sends (workspace_id, lower(to_address)) WHERE status = 'landed';
CREATE INDEX IF NOT EXISTS agent_crypto_sends_to_agent ON agent_crypto_sends (to_workspace_id, to_agent_id, created_at DESC) WHERE to_agent_id <> '';
