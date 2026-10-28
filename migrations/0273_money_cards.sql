-- B46.10 — cards on the money layer: single-use, locked to one merchant, capped.
--
-- A card batch is what an agent needs to pay for one job — "book a trip": a card for the airline, one for the hotel,
-- one for the car — asked for together and held, moving nothing, for one approval by the owner or an admin. On
-- approval each card's cap is held out of the agent's account in the batch's currency and the card is issued through
-- the money layer's Cards (B46.3), locked to its merchant and capped, under its own id, so approving twice issues
-- nothing twice. Each card takes one charge: the issuer asks Lens as it is made (money_card_authorizations), and Lens
-- approves only the card's merchant, up to its cap, once, within 24 hours of issue. The first approved charge closes
-- the card and gives back what it did not spend; a card unused after 24 hours is closed and its whole cap given back.

CREATE TABLE IF NOT EXISTS money_card_batches (
    id              TEXT PRIMARY KEY,                                  -- mcb_<uuid>
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL,                                     -- the agent the cards are for
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    purpose         TEXT NOT NULL DEFAULT '',                          -- what the cards are for: "Book a trip to Lisbon"
    currency        TEXT NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD')),
    funding         TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    total_minor     BIGINT NOT NULL CHECK (total_minor > 0),           -- the caps' sum: what one approval lets leave at most
    status          TEXT NOT NULL CHECK (status IN ('awaiting_approval', 'issuing', 'issued')),
    approved_by     TEXT NOT NULL DEFAULT '',
    approved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_money_card_batches_workspace ON money_card_batches (workspace_id, created_at);

CREATE TABLE IF NOT EXISTS money_cards (
    id              TEXT PRIMARY KEY,                                  -- mcard_<uuid>: the key it is issued under
    batch_id        TEXT NOT NULL REFERENCES money_card_batches (id),
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL,
    line            INTEGER NOT NULL CHECK (line >= 1),
    merchant        TEXT NOT NULL CHECK (merchant <> ''),              -- the one merchant it pays
    cap_minor       BIGINT NOT NULL CHECK (cap_minor > 0),
    currency        TEXT NOT NULL,
    account_id      TEXT NOT NULL,                                     -- the agent's account its cap is held from
    wallet_ref      TEXT NOT NULL,                                     -- the partner's reference for the money it spends
    hold_account_id TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('awaiting_approval', 'issuing', 'active', 'used', 'expired', 'failed')),
    partner         TEXT NOT NULL DEFAULT '',
    partner_ref     TEXT NOT NULL DEFAULT '',
    last4           TEXT NOT NULL DEFAULT '',
    spent_minor     BIGINT NOT NULL DEFAULT 0 CHECK (spent_minor >= 0 AND spent_minor <= cap_minor),
    detail          TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ,                                       -- 24 hours after issue
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, line),
    UNIQUE (batch_id, merchant)                                        -- one card per merchant
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_cards_partner_ref ON money_cards (partner, partner_ref) WHERE partner_ref <> '';
CREATE INDEX IF NOT EXISTS idx_money_cards_active ON money_cards (expires_at) WHERE status = 'active';

-- Every charge the issuer asked about, approved or declined; append-only. Asked again, the answer is the first one.
CREATE TABLE IF NOT EXISTS money_card_authorizations (
    partner      TEXT NOT NULL,
    id           TEXT NOT NULL,                                        -- the issuer's
    card_id      TEXT NOT NULL DEFAULT '',                             -- '' for a card Lens does not know
    workspace_id TEXT NOT NULL DEFAULT '',
    merchant     TEXT NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency     TEXT NOT NULL,
    approved     BOOLEAN NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',                             -- why it was declined
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (partner, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_card_authorizations_one_approved ON money_card_authorizations (card_id) WHERE approved AND card_id <> '';
