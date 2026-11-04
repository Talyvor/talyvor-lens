-- B48.15 — the owners of an agent's smart account change (internal/economy/agent_recovery.go): the recovery signer the
-- person adds, the passkey it puts in place of a lost one, and the agent's key rotated.
--
-- The account adds an owner at its next index and never uses a removed index again, so each owner's index is kept. The
-- owner columns always name the owners now; the made_with_ columns keep the ones the account was made with, from the
-- first change, because its address and its first operation on a chain it is not yet on are worked out from them.
-- next_owner_index is NULL until the first change: then it is 2, or 3 when the account was made with a recovery signer.
-- rotated_from_ lists every key the agent rotated from, with its index: until a rotation is on chain the key it
-- replaced is still an owner there, so a freeze answers the call that takes out each of them too.
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS owner_index INT NOT NULL DEFAULT 0;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS agent_index INT NOT NULL DEFAULT 1;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS recovery_index INT NOT NULL DEFAULT 2;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS next_owner_index INT;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS made_with_public_key BYTEA;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS made_with_agent_signer TEXT;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS made_with_recovery_address TEXT;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS recovered_at TIMESTAMPTZ;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS rotated_at TIMESTAMPTZ;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS rotated_from_signers TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS rotated_from_indexes INT[] NOT NULL DEFAULT '{}';
