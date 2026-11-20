-- B59.16 — Instant transfer to an outside wallet (launch pack 4.2 (2)).
--
-- An Instant transfer (0385) pays a Talyvor account or one of the owner's saved payees at a USDC address: the payee,
-- and the network and address it was paid at, as they were — a payee's address can change, what was paid to it cannot.
-- A transfer to a wallet has no Talyvor account on the other side, so to_workspace_id and to_account_id stay empty.
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS to_payee_id TEXT NOT NULL DEFAULT '';
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS to_network TEXT NOT NULL DEFAULT '';
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS to_address TEXT NOT NULL DEFAULT '';
ALTER TABLE instant_transfers ADD COLUMN IF NOT EXISTS beneficiary_country TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_instant_transfers_to_address ON instant_transfers (agent_id, to_address) WHERE status = 'received' AND to_address <> '';
