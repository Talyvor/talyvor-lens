-- B30.31: an agent card can be locked to one merchant.
--
-- agent_cards.merchant_lock is how: 'off' (every card until now), 'first' (locked to the network merchant id of the
-- first purchase the card approves from now on) or 'merchant' (locked to the merchant id the owner set).
-- agent_cards.locked_merchant_id is the merchant it is locked to: NULL while 'off', or 'first' before that purchase.
-- Every authorisation from any other merchant is declined, the lock named in its reason.

ALTER TABLE agent_cards
    ADD COLUMN IF NOT EXISTS merchant_lock TEXT NOT NULL DEFAULT 'off' CHECK (merchant_lock IN ('off', 'first', 'merchant')),
    ADD COLUMN IF NOT EXISTS locked_merchant_id TEXT;
