-- 0326_chain_movement_values.sql — B48.14: what each movement of a non-stable token was worth when it happened.
--
-- The accountant's export (economy.WalletTaxExport) works out the gain or loss on each disposal of a non-stable token
-- first in, first out, in the owner's currency. That needs each movement's value at the time it moved, which a price
-- read later cannot give, so it is kept on the movement: value_minor in value_currency's minor units. A movement of a
-- non-stable token without one is listed in the export as unpriced, never valued at zero.
ALTER TABLE chain_movements ADD COLUMN IF NOT EXISTS value_minor BIGINT;
ALTER TABLE chain_movements ADD COLUMN IF NOT EXISTS value_currency TEXT NOT NULL DEFAULT '';
