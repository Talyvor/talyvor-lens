-- 0341_agent_spend_permission_settle.sql — B54.26: an agent's payment settles from its operation's receipt.
--
-- chain_id and paymaster are where a use was paid and, for a fee an ERC-20 paymaster takes in the token, who takes it:
-- what is left approved to a paymaster on a chain is counted once per paymaster and chain, so another paymaster's
-- approval never replaces it. op is the operation the agent's key signed for the use (smartaccount.UserOp's JSON),
-- op_hash its hash and sent_block the chain's head when it was handed to the bundler. outcome is how it ended, read from
-- the chain by economy.SettleAgentCryptoPayments: paid; reverted (the payment did not move, a fee may have); or dropped
-- (its account used its nonce for another operation, so it never lands, and the use counts nothing). NULL until known.
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS chain_id BIGINT;
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS paymaster TEXT;
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS op JSONB;
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS op_hash TEXT;
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS sent_block BIGINT;
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS outcome TEXT CHECK (outcome IN ('paid', 'reverted', 'dropped'));
-- A use whose payment did not move may count nothing.
ALTER TABLE agent_spend_permission_uses DROP CONSTRAINT IF EXISTS agent_spend_permission_uses_amount_check;
ALTER TABLE agent_spend_permission_uses ADD CONSTRAINT agent_spend_permission_uses_amount_check
	CHECK (amount > 0 OR outcome IS NOT NULL AND amount >= 0);
CREATE INDEX IF NOT EXISTS agent_spend_permission_uses_unsettled ON agent_spend_permission_uses (chain_id, id)
	WHERE op IS NOT NULL AND outcome IS NULL;
