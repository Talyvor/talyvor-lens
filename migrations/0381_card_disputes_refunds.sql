-- 0381: B59.24 — card disputes and refunds. The owner disputes a payment on an agent's card (money_card_authorizations,
-- approved) with a reason; the dispute is opened with the card's issuer and kept here as it goes: opened, with the
-- issuer, then won or lost. A merchant's refund is kept here once under the issuer's id. A won dispute and a refund
-- each credit the card's account once, as their own money entry (credited_minor, entry_id), and together never more
-- than the payment: what is left of it is read under the payment's row lock.
CREATE TABLE IF NOT EXISTS money_card_disputes (
    id            TEXT        PRIMARY KEY,                           -- cdsp_<uuid>
    partner       TEXT        NOT NULL,                              -- the issuer's adapter name
    payment_id    TEXT        NOT NULL,                              -- money_card_authorizations.id
    workspace_id  TEXT        NOT NULL,
    agent_id      TEXT        NOT NULL DEFAULT '',                   -- '' on the owner's own card
    card_id       TEXT        NOT NULL,
    merchant      TEXT        NOT NULL,
    amount_minor  BIGINT      NOT NULL CHECK (amount_minor > 0),     -- the payment's
    currency      TEXT        NOT NULL,
    reason        TEXT        NOT NULL CHECK (reason <> ''),
    status        TEXT        NOT NULL DEFAULT 'opened' CHECK (status IN ('opened', 'with_issuer', 'won', 'lost')),
    partner_ref   TEXT        NOT NULL DEFAULT '',                   -- the issuer's, once it has the dispute
    credited_minor BIGINT     NOT NULL DEFAULT 0 CHECK (credited_minor >= 0),
    entry_id      TEXT        NOT NULL DEFAULT '',                   -- the money entry that credited it
    opened_by     TEXT        NOT NULL DEFAULT '',
    decided_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (partner, payment_id)                                     -- one dispute a payment
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_card_disputes_ref ON money_card_disputes (partner, partner_ref) WHERE partner_ref <> '';
CREATE INDEX IF NOT EXISTS idx_money_card_disputes_workspace ON money_card_disputes (workspace_id, created_at);

CREATE TABLE IF NOT EXISTS money_card_refunds (
    partner        TEXT        NOT NULL,
    id             TEXT        NOT NULL CHECK (id <> ''),           -- the issuer's
    payment_id     TEXT        NOT NULL,                            -- money_card_authorizations.id
    workspace_id   TEXT        NOT NULL,
    agent_id       TEXT        NOT NULL DEFAULT '',
    card_id        TEXT        NOT NULL,
    merchant       TEXT        NOT NULL,
    amount_minor   BIGINT      NOT NULL CHECK (amount_minor > 0),   -- what the merchant refunded
    currency       TEXT        NOT NULL,
    credited_minor BIGINT      NOT NULL CHECK (credited_minor >= 0), -- what was credited: no more than is left of the payment
    entry_id       TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (partner, id)
);
CREATE INDEX IF NOT EXISTS idx_money_card_refunds_payment ON money_card_refunds (partner, payment_id);
CREATE INDEX IF NOT EXISTS idx_money_card_refunds_workspace ON money_card_refunds (workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_money_card_authorizations_workspace ON money_card_authorizations (workspace_id, created_at) WHERE approved;
