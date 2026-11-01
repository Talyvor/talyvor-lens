-- 0283_card_payment_details.sql — B53.1: what every card payment says about itself, for the spending Coach.
--
-- An issuer reports, with each charge it asks about, the merchant category code (ISO 18245), the merchant's country
-- (ISO 3166 alpha-2), the channel (in_person, online, contactless) and the phone wallet it was paid from (apple_pay,
-- google_pay, samsung_pay, or none for the card itself). Lens stores them on the authorisation and on the card_charge
-- entry it posts, and each card's form factor (virtual or physical). Whatever an issuer did not say, and every row
-- from before this migration, reads 'unknown'. Entries that are not card payments leave the card_ columns NULL.

ALTER TABLE money_card_authorizations
    ADD COLUMN IF NOT EXISTS mcc              TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS merchant_country TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS channel          TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS wallet           TEXT NOT NULL DEFAULT 'unknown';

ALTER TABLE money_entries
    ADD COLUMN IF NOT EXISTS card_mcc     TEXT,
    ADD COLUMN IF NOT EXISTS card_country TEXT,
    ADD COLUMN IF NOT EXISTS card_channel TEXT,
    ADD COLUMN IF NOT EXISTS card_wallet  TEXT;

ALTER TABLE money_cards
    ADD COLUMN IF NOT EXISTS form_factor TEXT NOT NULL DEFAULT 'unknown';
