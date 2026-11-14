-- B59.51 — the owner's own bank: a payee saved as the owner's own account, the one a company account is withdrawn to.
--
-- Wallets' withdraw pays only a payee marked own; it is set when the payee is saved and passes every check a payee
-- does (the bank's name check, screening, the cooling-off and details holds), as any payment out does.
ALTER TABLE money_payees ADD COLUMN IF NOT EXISTS own BOOLEAN NOT NULL DEFAULT false;
