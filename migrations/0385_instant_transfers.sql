-- B59.15 — Instant transfer to a Talyvor account: one id, three legs, all or nothing (launch pack 4.1, 4.2 (1), 4.3).
--
-- An agent's rule "Can use Instant transfer" is off until its owner turns it on: an agent without it is refused.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS may_instant_transfer BOOLEAN NOT NULL DEFAULT false;

-- Every version of an agent's rules (0192) names it as its rules hold it now, so the next save that changes nothing is
-- not a new version, and a rollback to a version taken before it keeps it as it is.
UPDATE agent_rules_versions v
   SET rules = v.rules || jsonb_build_object('may_instant_transfer', r.may_instant_transfer)
  FROM agent_rules r
 WHERE r.agent_id = v.agent_id AND NOT v.rules ? 'may_instant_transfer';

-- An Instant transfer sent at a quote (0384) from an agent's account to a Talyvor account, once under its key. Its three
-- legs are money entries in ledger order: convert (the agent's currency to USDC, the fee to Talyvor), move (the USDC on
-- chain to the beneficiary) and credit (into the beneficiary's account, converted on arrival unless it holds USDC). A
-- leg that fails reverses the legs before it at the figures they moved at, so the agent has exactly what it had.
CREATE TABLE IF NOT EXISTS instant_transfers (
    id                   TEXT PRIMARY KEY,                          -- itr_<uuid>
    workspace_id         TEXT NOT NULL,
    agent_id             TEXT NOT NULL,
    quote_id             TEXT NOT NULL UNIQUE,                      -- a quote sends once
    idempotency_key      TEXT NOT NULL,
    funding              TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    from_account_id      TEXT NOT NULL,
    to_workspace_id      TEXT NOT NULL,
    to_account_id        TEXT NOT NULL,
    beneficiary_name     TEXT NOT NULL,
    send_minor           BIGINT NOT NULL CHECK (send_minor > 0),
    send_currency        TEXT NOT NULL,
    fee_minor            BIGINT NOT NULL CHECK (fee_minor >= 0),
    onchain_minor        BIGINT NOT NULL CHECK (onchain_minor > 0),  -- the USDC the move leg carries
    receive_minor        BIGINT NOT NULL CHECK (receive_minor > 0),
    receive_currency     TEXT NOT NULL,
    rate                 TEXT NOT NULL,
    status               TEXT NOT NULL CHECK (status IN ('sent', 'converted', 'on_its_way', 'received', 'reversed')),
    convert_entry_id     TEXT NOT NULL DEFAULT '',
    move_entry_id        TEXT NOT NULL DEFAULT '',
    credit_entry_id      TEXT NOT NULL DEFAULT '',
    convert_ref          TEXT NOT NULL DEFAULT '',                  -- the conversion into USDC, at the provider
    onchain_ref          TEXT NOT NULL DEFAULT '',                  -- the on-chain move
    credit_ref           TEXT NOT NULL DEFAULT '',                  -- the conversion on arrival; empty when USDC is kept
    failed_leg           TEXT NOT NULL DEFAULT '',
    failure              TEXT NOT NULL DEFAULT '',
    reversal_entry_ids   TEXT[] NOT NULL DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_instant_transfers_agent ON instant_transfers (workspace_id, agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_instant_transfers_to ON instant_transfers (agent_id, to_account_id) WHERE status = 'received';
