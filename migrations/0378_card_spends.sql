-- 0378: B59.21 — approval before the card. On a card programme in preapproved mode the issuer decides a payment on
-- the card's limit alone and never asks Lens, so the agent asks first: it names the merchant and the amount, and
-- either a one-time card for it or the standing card whose limit it raises (a subscription). Lens checks the agent's
-- rules (asked); anything outside them waits for the owner's approval (agent_approvals, under the request's fingerprint).
-- Only then is a one-time card issued for that amount (a batch of one, money_card_batches) or the standing card's
-- limit raised by it at the issuer, until expires_at: past it, an approval not used in time gives the limit back.
CREATE TABLE IF NOT EXISTS money_card_spends (
    id               TEXT        PRIMARY KEY,                       -- mcs_<uuid>
    workspace_id     TEXT        NOT NULL,
    agent_id         TEXT        NOT NULL,
    idempotency_key  TEXT        NOT NULL CHECK (idempotency_key <> ''),
    merchant         TEXT        NOT NULL CHECK (merchant <> ''),   -- who the agent pays
    amount_minor     BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency         TEXT        NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD')),
    funding          TEXT        NOT NULL CHECK (funding IN ('test', 'live')),
    standing_card_id TEXT        NOT NULL DEFAULT '',               -- the reusable card whose limit it raises; '' a one-time card
    status           TEXT        NOT NULL CHECK (status IN ('asked', 'awaiting_approval', 'issuing', 'active', 'expired', 'failed')),
    approval_id      TEXT        NOT NULL DEFAULT '',               -- the owner's approval it waited for, if it did
    batch_id         TEXT        NOT NULL DEFAULT '',               -- the one-time card's batch
    raised_to_minor  BIGINT      NOT NULL DEFAULT 0,                -- the standing card's limit its raise set, so a retry raises it once
    detail           TEXT        NOT NULL DEFAULT '',
    expires_at       TIMESTAMPTZ,                                   -- a standing card's raise lasts until then
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_money_card_spends_workspace ON money_card_spends (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_money_card_spends_due ON money_card_spends (expires_at) WHERE status = 'active' AND standing_card_id <> '';
