-- 0286_money_reusable_cards.sql — B53.2: the owner's own card, and an agent's, reusable, with limits and categories.
--
-- Beside an agent's single-use cards (B46.10), the owner issues a reusable card on their own wallet (the company's
-- account in a currency) or on one of their agents' wallets. It pays any merchant, as often as its controls let it:
-- a limit per payment and a limit a day, both set when it is issued; the merchant categories (ISO 18245 MCCs) it pays
-- only, or never; and frozen, which declines everything until it is unfrozen. The issuer asks Lens as each charge is
-- made, and the answer is recorded in money_card_authorizations as a single-use card's is.

CREATE TABLE IF NOT EXISTS money_reusable_cards (
    id                      TEXT PRIMARY KEY,                          -- rcard_<uuid>: the key it is issued under
    workspace_id            TEXT NOT NULL,
    agent_id                TEXT NOT NULL DEFAULT '',                  -- '' for the owner's own card
    idempotency_key         TEXT NOT NULL CHECK (idempotency_key <> ''),
    currency                TEXT NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD')),
    funding                 TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    account_id              TEXT NOT NULL,                             -- the account it spends from
    wallet_ref              TEXT NOT NULL,                             -- the partner's reference for that money
    status                  TEXT NOT NULL CHECK (status IN ('issuing', 'active', 'frozen', 'failed')),
    per_payment_limit_minor BIGINT NOT NULL CHECK (per_payment_limit_minor > 0),
    daily_limit_minor       BIGINT NOT NULL CHECK (daily_limit_minor >= per_payment_limit_minor),
    allowed_mccs            TEXT[] NOT NULL DEFAULT '{}',              -- empty: every category not blocked
    blocked_mccs            TEXT[] NOT NULL DEFAULT '{}',
    partner                 TEXT NOT NULL DEFAULT '',
    partner_ref             TEXT NOT NULL DEFAULT '',
    last4                   TEXT NOT NULL DEFAULT '',
    form_factor             TEXT NOT NULL DEFAULT 'unknown',
    detail                  TEXT NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_reusable_cards_partner_ref ON money_reusable_cards (partner, partner_ref) WHERE partner_ref <> '';
CREATE INDEX IF NOT EXISTS idx_money_reusable_cards_workspace ON money_reusable_cards (workspace_id, created_at);

-- A single-use card is still approved once; a reusable card is approved as often as its controls let it.
DROP INDEX IF EXISTS idx_money_card_authorizations_one_approved;
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_card_authorizations_one_approved ON money_card_authorizations (card_id)
    WHERE approved AND card_id LIKE 'mcard\_%';
CREATE INDEX IF NOT EXISTS idx_money_card_authorizations_card_day ON money_card_authorizations (card_id, created_at) WHERE approved;
