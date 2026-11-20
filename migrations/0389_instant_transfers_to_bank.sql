-- B59.17 — Instant transfer to an outside bank account (launch pack 4.2 (3)).
--
-- An Instant transfer (0385, 0387) can pay one of the owner's saved payees at a bank account: to_payee_id with no
-- address. Its third leg is a payout, not a credit: the USDC converted to the currency the payee is paid in, at the
-- quote the transfer locked, and paid into the account by the payout provider. The payout's money entry, the
-- conversion's and the payout's references, and the longest the bank's rail takes — shown in the quote and in the
-- approval before the owner confirms — are kept with the transfer. A bank account paid in the currency sent is paid
-- straight (the direct route, 0384): its only leg is the payout.
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS payout_entry_id TEXT NOT NULL DEFAULT '';
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS payout_convert_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS payout_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS arrives_within_seconds BIGINT NOT NULL DEFAULT 0;
-- Paid straight, nothing goes on chain: only such a transfer, to a bank, carries no USDC.
ALTER TABLE instant_transfers DROP CONSTRAINT IF EXISTS instant_transfers_onchain_minor_check;
ALTER TABLE instant_transfers ADD CONSTRAINT instant_transfers_onchain_minor_check
    CHECK (onchain_minor > 0 OR (onchain_minor = 0 AND to_payee_id <> '' AND to_address = ''));
CREATE INDEX IF NOT EXISTS idx_instant_transfers_to_payee ON instant_transfers (agent_id, to_payee_id) WHERE status = 'received' AND to_payee_id <> '';
