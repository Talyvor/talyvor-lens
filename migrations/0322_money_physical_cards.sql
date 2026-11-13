-- 0322_money_physical_cards.sql — B53.19: the owner's physical card, ordered by post, activated on arrival, replaced.
--
-- The owner orders a physical card on their own pounds account, in the partner's standard design, to an address Lens
-- has checked. The issuer posts it (delivery: ordered, shipped, delivered) and it stays inactive until the owner
-- activates it with its last four digits. A lost, stolen or damaged card is frozen at the issuer at once and a new
-- one ordered to the same address. The card number never reaches Lens: a card is its partner_ref and last four.

CREATE TABLE IF NOT EXISTS money_physical_cards (
    id                  TEXT PRIMARY KEY,                              -- pcard_<uuid>: the key it is issued under
    workspace_id        TEXT NOT NULL,
    account_id          TEXT NOT NULL,                                 -- the owner's account it is on
    wallet_ref          TEXT NOT NULL,                                 -- the partner's reference for that money
    state               TEXT NOT NULL CHECK (state IN ('ordering', 'inactive', 'active', 'frozen', 'closed')),
    name_on_card        TEXT NOT NULL CHECK (name_on_card <> ''),
    address             JSONB NOT NULL,                                -- {line1, line2, city, postcode, country}, checked
    delivery_status     TEXT NOT NULL DEFAULT 'ordered' CHECK (delivery_status IN ('ordered', 'shipped', 'delivered')),
    delivery_updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    partner             TEXT NOT NULL DEFAULT '',
    partner_ref         TEXT NOT NULL DEFAULT '',
    last4               TEXT NOT NULL DEFAULT '',
    replaces            TEXT NOT NULL DEFAULT '',                      -- the card this one replaces
    replaced_by         TEXT NOT NULL DEFAULT '',
    replace_reason      TEXT NOT NULL DEFAULT '' CHECK (replace_reason IN ('', 'lost', 'stolen', 'damaged')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_physical_cards_partner_ref ON money_physical_cards (partner, partner_ref) WHERE partner_ref <> '';
CREATE INDEX IF NOT EXISTS idx_money_physical_cards_workspace ON money_physical_cards (workspace_id, created_at);
