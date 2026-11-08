-- 0332_agent_spend_permission_fees.sql — B54.12: a network fee paid in the token counts what it cost.
--
-- A payment whose network fee an ERC-20 paymaster takes in the same token is counted against the owner's allowance
-- with the most that fee can be (smartaccount.SponsorInToken's quote), which the operation approves the paymaster.
-- fee is that most, kept with the use. Once the operation is on the chain, economy.SettleAgentCryptoFee counts instead
-- fee_charged, what the paymaster took, and fee_left, what the account still approves it, which the paymaster could
-- still take until the next such payment's approval replaces it: only the latest settled use of an agent's token keeps
-- a fee_left, the others' is 0. All three are NULL for a payment with no such fee.
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS fee BIGINT CHECK (fee >= 0);
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS fee_charged BIGINT CHECK (fee_charged >= 0 AND fee_charged <= fee);
ALTER TABLE agent_spend_permission_uses ADD COLUMN IF NOT EXISTS fee_left BIGINT CHECK (fee_left >= 0);
