-- B41.111 — new-payee cooling-off.
--
-- A payment of more than 1,000 (in its currency) to a payee saved less than 24 hours ago waits until the 24 hours have
-- passed, unless the owner has confirmed with a passkey that they called the payee on a number they already had:
-- called_at is when, called_by the passkey that signed it.
ALTER TABLE money_payees ADD COLUMN IF NOT EXISTS called_at TIMESTAMPTZ;
ALTER TABLE money_payees ADD COLUMN IF NOT EXISTS called_by TEXT NOT NULL DEFAULT '';
