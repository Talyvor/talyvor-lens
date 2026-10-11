-- B30.22 — international payments, with the fee and the rate shown first.
--
-- A quote prices a payment to a payee whose account is in another currency than the account it is paid from: what is
-- sent, the partner's cost and Talyvor's fee (both in the sending currency), the rate, and what arrives. A payment is
-- made at a quote within its life, and keeps the quote's figures.

CREATE TABLE IF NOT EXISTS money_payment_quotes (
    id                  TEXT PRIMARY KEY,                             -- mpq_<uuid>
    workspace_id        TEXT NOT NULL,
    account_id          TEXT NOT NULL REFERENCES money_accounts (id), -- the account it would be paid from
    agent_id            TEXT NOT NULL DEFAULT '',
    payee_id            TEXT NOT NULL,
    payee_name          TEXT NOT NULL,
    destination_country TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL CHECK (amount_minor > 0),     -- what is sent, before the fee
    currency            TEXT NOT NULL,
    partner_cost_minor  BIGINT NOT NULL CHECK (partner_cost_minor >= 0),
    fee_minor           BIGINT NOT NULL CHECK (fee_minor >= 0),       -- Talyvor's
    arrives_minor       BIGINT NOT NULL CHECK (arrives_minor > 0),
    arrives_currency    TEXT NOT NULL,
    rate                TEXT NOT NULL,
    funding             TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    partner             TEXT NOT NULL,
    partner_ref         TEXT NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_money_payment_quotes_workspace ON money_payment_quotes (workspace_id, created_at);

ALTER TABLE money_payments
    ADD COLUMN IF NOT EXISTS quote_id           TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS quote_ref          TEXT   NOT NULL DEFAULT '',  -- the partner's reference for the quote
    ADD COLUMN IF NOT EXISTS partner_cost_minor BIGINT NOT NULL DEFAULT 0 CHECK (partner_cost_minor >= 0),
    ADD COLUMN IF NOT EXISTS fee_minor          BIGINT NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
    ADD COLUMN IF NOT EXISTS arrives_minor      BIGINT NOT NULL DEFAULT 0 CHECK (arrives_minor >= 0),
    ADD COLUMN IF NOT EXISTS arrives_currency   TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS rate               TEXT   NOT NULL DEFAULT '';
