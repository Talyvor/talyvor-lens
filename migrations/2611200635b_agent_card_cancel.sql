-- B28.321 — the talyvor-lens side of B28.47: cancel and replace an agent's card.
--
-- A cancelled card stays a row, so a purchase Stripe still sends on it is declined as cancelled and writes no posting,
-- and its earlier purchases still capture, reverse and refund where they were held. An agent holds at most ONE card that
-- is not cancelled: the unique key on agent_id becomes a partial one. replaced_by names the card issued in its place.
ALTER TABLE agent_cards
    ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS replaced_by TEXT NOT NULL DEFAULT '';

ALTER TABLE agent_cards DROP CONSTRAINT IF EXISTS agent_cards_agent_id_key;
CREATE UNIQUE INDEX IF NOT EXISTS agent_cards_one_live_per_agent ON agent_cards (agent_id) WHERE cancelled_at IS NULL;
