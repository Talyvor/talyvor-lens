-- 0284_spend_categories_and_tags.sql — B53.3: every payment gets a category; one correction teaches it; spend tags.
--
-- A payment's category is worked out when it is read (economy/spending.go): the owner's correction for its merchant
-- first, then the card's merchant category code, then the merchant's name, then the payee. So a correction here moves
-- that merchant's past payments and its future ones at once, and nothing is written back onto the ledger.
--
-- spend_category_corrections: "always put this merchant in X", one per merchant (lower-cased, trimmed) per workspace.
-- money_entry_tags:           the owner's own tags on a movement of money (money_entries), to total spend by tag.

CREATE TABLE IF NOT EXISTS spend_category_corrections (
    workspace_id TEXT NOT NULL,
    merchant_key TEXT NOT NULL CHECK (merchant_key <> ''),
    category     TEXT NOT NULL CHECK (category ~ '^[a-z][a-z_]*$'),
    set_by       TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, merchant_key)
);

CREATE TABLE IF NOT EXISTS money_entry_tags (
    workspace_id TEXT NOT NULL,
    entry_id     TEXT NOT NULL REFERENCES money_entries (id),
    tag          TEXT NOT NULL CHECK (tag ~ '^[a-z0-9][a-z0-9 _-]{0,39}$'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, entry_id, tag)
);
