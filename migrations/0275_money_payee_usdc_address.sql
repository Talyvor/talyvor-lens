-- B46.18 — a saved payee can be a USDC address on Base.
--
-- network and address are the payee's chain and address (lower case), instead of bank details. Nobody can check a
-- name against an address, so its check_result is 'address': it is paid only once the owner has approved it with a
-- passkey (confirmed_at), which puts it on the owner's allowlist of addresses an agent's wallet may pay.
ALTER TABLE money_payees ADD COLUMN IF NOT EXISTS network TEXT NOT NULL DEFAULT '';
ALTER TABLE money_payees ADD COLUMN IF NOT EXISTS address TEXT NOT NULL DEFAULT '';
ALTER TABLE money_payees DROP CONSTRAINT IF EXISTS money_payees_check_result_check;
ALTER TABLE money_payees ADD CONSTRAINT money_payees_check_result_check
    CHECK (check_result IN ('exact_match', 'close_match', 'no_match', 'address'));
