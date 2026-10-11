-- B30.129 — pay out to an outside business what it pulled under a mandate.
--
-- A mandate granted to an outside business names the saved payee (money_payees, B30.16) its pulls are paid out to:
-- each pull moves into the mandate's hold account (0246) and leaves it as a payment out (money_payments, 0247) through
-- the account partner. A mandate granted before this column names no payee, and pulls nothing until a new one does.

ALTER TABLE money_mandates ADD COLUMN IF NOT EXISTS payee_id TEXT; -- an outside business's saved payee (money_payees)
