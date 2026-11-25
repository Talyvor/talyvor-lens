-- B48.37 — the owner's spend permission held on chain too: the agent's own account that spends under it, the spend
-- permissions it approves in the SpendPermissionManager (JSON, as smartaccount.SpendPermission), and the account's call
-- the person's passkey signs to put them there. NULL on_chain: a permission held at Talyvor's gate only.
ALTER TABLE agent_spend_permissions ADD COLUMN IF NOT EXISTS spender TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_spend_permissions ADD COLUMN IF NOT EXISTS on_chain TEXT;
ALTER TABLE agent_spend_permissions ADD COLUMN IF NOT EXISTS grant_call TEXT NOT NULL DEFAULT '';
