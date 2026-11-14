-- 0367_agent_card_draws_on.sql — B30.30: an agent card can spend from the agent's currency account in the card's
-- currency instead of its credits.
--
-- agent_cards.draws_on is what the card spends: 'credits' (B19.12, every card until now) or 'account'. An authorisation
-- the card approved from the account names that account on its row (money_account_id): its hold moved there on the
-- money ledger (card_hold, agent's account → hold), and every capture, release and refund of it settles there too,
-- whatever the card draws on by then. NULL is a purchase on credits.

ALTER TABLE agent_cards
    ADD COLUMN IF NOT EXISTS draws_on TEXT NOT NULL DEFAULT 'credits' CHECK (draws_on IN ('credits', 'account'));

ALTER TABLE agent_card_authorizations
    ADD COLUMN IF NOT EXISTS money_account_id TEXT;
