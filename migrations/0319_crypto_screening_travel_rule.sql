-- 0319_crypto_screening_travel_rule.sql — B48.7: screening and the Travel Rule on every transfer.
--
-- A deposit into an agent's wallet from a flagged address is quarantined (chain_movements.status quarantined): on the
-- chain, but posted to nothing until an operator releases its compliance case. A payment to an address a regulated
-- provider holds keeps the identity data that travelled with it (travel_rule_transfers), one row per payment. An address
-- the owner proved they hold the key of, by signing a message with it, is kept once (crypto_address_proofs): a payment
-- of more than the EU's threshold to a self-hosted address waits for one.
ALTER TABLE chain_movements DROP CONSTRAINT IF EXISTS chain_movements_status_check;
ALTER TABLE chain_movements ADD CONSTRAINT chain_movements_status_check
    CHECK (status IN ('seen', 'confirmed', 'reversed', 'final', 'quarantined'));

CREATE TABLE IF NOT EXISTS travel_rule_transfers (
    payment_id    TEXT PRIMARY KEY,               -- money_payments.id
    workspace_id  TEXT NOT NULL,
    provider      TEXT NOT NULL,                  -- the Travel Rule adapter that sent it
    provider_ref  TEXT NOT NULL,
    vasp          TEXT NOT NULL,                  -- the regulated provider that holds the beneficiary's address
    network       TEXT NOT NULL,
    asset         TEXT NOT NULL,
    amount_minor  BIGINT NOT NULL,
    originator    JSONB NOT NULL,
    beneficiary   JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_travel_rule_transfers_workspace ON travel_rule_transfers (workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS crypto_address_proofs (
    workspace_id  TEXT NOT NULL,
    network       TEXT NOT NULL,
    address       TEXT NOT NULL,                  -- lower case
    message       TEXT NOT NULL,                  -- what was signed
    signature     BYTEA NOT NULL,                 -- r‖s‖v, personal_sign
    proved_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, network, address)
);
