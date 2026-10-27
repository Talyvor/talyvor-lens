-- B48.1 — an agent is fiat, crypto or both, and owns one wallet of each kind it is.
--
-- wallet_kind is chosen at creation and changed by the owner; an agent made before kinds is crypto, the USDC wallet
-- B46.4 gave it. wallet_fiat_currency is the currency its fiat wallet is opened in.

ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS wallet_kind TEXT NOT NULL DEFAULT 'crypto'
    CHECK (wallet_kind IN ('fiat', 'crypto', 'both'));
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS wallet_fiat_currency TEXT NOT NULL DEFAULT 'EUR'
    CHECK (wallet_fiat_currency IN ('GBP', 'EUR', 'USD'));
