-- B61.41 — what an agent has spent is counted in the rules' own currency (internal/economy/agent_rules_currency.go).
--
-- agent_rule_spends: one row for each movement from a currency account that rules held in pounds or euros let
-- through, written in the movement's own transaction, so it commits exactly when the movement does. It keeps what
-- the movement was worth in the rules' currency then — its own amount when it is in that currency, else at the
-- conversion provider's rate, rounded up — under the ref the rules judged it by. The day's, week's and month's spend
-- for such rules is the sum of these over the movements the period counts (agentSpentSince), so a limit and what was
-- spent against it no longer move with the rate. Movements before this, and any recorded in another currency, are
-- valued at today's rate as before.
CREATE TABLE IF NOT EXISTS agent_rule_spends (
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    ref          TEXT NOT NULL,                       -- the movement, as the rules judged it: its payment, entry key or authorisation
    currency     TEXT NOT NULL CHECK (currency <> ''), -- the rules' currency when it was let through
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, ref)
);

-- Append-only, like every other money record (0055).
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_rule_spends
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_rule_spends
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
