-- 0328_chain_movement_amounts.sql — B48.28: the chain indexer keeps every verified token, not only USDC.
--
-- An 18-decimal token's smallest unit overflows BIGINT above about 9.2 whole tokens (9.2 ETH), so a movement's amount
-- is kept whole as NUMERIC: the chain's uint256 fits in 78 digits. Only USDC's movements post to the ledger (its chain
-- accounts are in USDC); every other token's movement is kept, confirmed and valued here for the accountant's export.
ALTER TABLE chain_movements ALTER COLUMN amount_minor TYPE NUMERIC(78, 0);
