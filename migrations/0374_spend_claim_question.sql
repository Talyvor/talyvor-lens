-- B58.3 — a question the agent never got an answer to is never charged.
--
-- With reservations off an agent's question is debited its estimate before the serve (lxc_spend_claims) and
-- settled after it (agent_debit_settlements). question_id marks a claim as such a pre-serve debit and names its
-- question (the token_events request_id). A debit with no settlement and no served usage row after a set time
-- is given back by the sweeper (economy.ReleaseStrandedAgentDebits). Every other agent charge leaves it NULL
-- and is never swept; so does every claim booked before this migration.
ALTER TABLE lxc_spend_claims ADD COLUMN IF NOT EXISTS question_id TEXT;

CREATE INDEX IF NOT EXISTS lxc_spend_claims_questions
    ON lxc_spend_claims (created_at) WHERE question_id IS NOT NULL;
