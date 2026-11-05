-- 0323_reusable_card_agent_spend.sql — B54.6: an agent's reusable card follows the agent's own spending rules.
--
-- A charge on an agent's reusable card is judged by the agent's rules as well as the card's controls, and counts
-- toward what the agent has spent at what it was worth in LXC when it was approved, as a payment out does
-- (money_payments.value_ulxc). The owner's own card has no agent, and its charges count toward none.
ALTER TABLE money_card_authorizations ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE money_card_authorizations ADD COLUMN IF NOT EXISTS value_ulxc BIGINT NOT NULL DEFAULT 0 CHECK (value_ulxc >= 0);
CREATE INDEX IF NOT EXISTS idx_money_card_authorizations_agent_spend ON money_card_authorizations (agent_id, created_at)
    WHERE approved AND agent_id <> '';
