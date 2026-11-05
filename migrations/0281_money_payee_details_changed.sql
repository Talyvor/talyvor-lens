-- B41.112 — a saved payee's bank details can be changed, and the change is locked.
--
-- details_changed_at is when the payee's account details last changed (by passkey, with a fresh check of the name at
-- its bank). For 24 hours after it every payment to the payee is held.
ALTER TABLE money_payees ADD COLUMN IF NOT EXISTS details_changed_at TIMESTAMPTZ;
