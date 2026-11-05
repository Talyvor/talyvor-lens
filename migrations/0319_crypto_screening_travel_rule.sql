-- 0319_crypto_screening_travel_rule.sql — B48.7: screening and the Travel Rule on every transfer.
--
-- A deposit into an agent's wallet from a flagged address is quarantined (chain_movements.status quarantined): on the
-- chain, but posted to nothing until an operator releases its compliance case. A payment to an address a regulated
-- provider holds keeps the identity data that travelled with it (travel_rule_transfers), one row per payment. An address
-- the owner proved they hold the key of, by signing a message with it, is kept once (crypto_address_proofs): a payment
-- that takes what is paid to a self-hosted address over the EU's threshold waits for one; what is paid is counted by
-- each payment's own address (money_payments.address), as it was paid, never by its payee's, which can change.
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

ALTER TABLE money_payments ADD COLUMN IF NOT EXISTS address TEXT NOT NULL DEFAULT ''; -- the USDC address paid, lower case
CREATE INDEX IF NOT EXISTS idx_money_payments_address ON money_payments (workspace_id, address) WHERE address <> '';
-- Payments made before this column count too, by their payee's address now: the best that is known of them.
UPDATE money_payments m SET address = y.address FROM money_payees y
    WHERE y.id = m.payee_id AND y.workspace_id = m.workspace_id AND m.address = '' AND y.address <> '';

-- A Travel Rule send that keeps failing ends its payment as failed after a bounded number of tries (B48.7).
ALTER TABLE money_payments ADD COLUMN IF NOT EXISTS travel_rule_attempts INT NOT NULL DEFAULT 0;

-- A payment in from a flagged payer, on any rail, is quarantined: the money is at the partner, but nothing posts until
-- an operator releases its compliance case, and then it posts once under the same key (payment_in:<partner_ref>).
CREATE TABLE IF NOT EXISTS money_quarantined_deposits (
    workspace_id        TEXT NOT NULL,
    partner_ref         TEXT NOT NULL,             -- the partner's reference for the payment
    case_id             TEXT NOT NULL,             -- compliance_cases.id
    partner             TEXT NOT NULL,
    partner_account_ref TEXT NOT NULL,
    payer               TEXT NOT NULL,
    reference           TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL,
    currency            TEXT NOT NULL,
    funding             TEXT NOT NULL,
    quarantined_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at           TIMESTAMPTZ,
    PRIMARY KEY (workspace_id, partner_ref)
);
CREATE INDEX IF NOT EXISTS idx_money_quarantined_deposits_open ON money_quarantined_deposits (case_id) WHERE posted_at IS NULL;
