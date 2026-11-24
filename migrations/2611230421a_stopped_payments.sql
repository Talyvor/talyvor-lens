-- B62.9 — a stopped payment is kept.
--
-- When the payment about to be sent under an approval is not the one the owner approved (approvalMatches, B62.1), its
-- transaction rolls back, so whatever it held is released and nothing of it is kept. stopped_payments is the one row
-- that is: written in its own transaction after the rollback, naming the approval, what the owner was shown, what would
-- have been sent, which of payee, amount, currency and agent differed, and when. An approval reads back the newest
-- (agent_approvals' `stopped`). It is append-only, as payment_proofs is (2611162348b): every UPDATE, DELETE and
-- TRUNCATE raises.

CREATE TABLE IF NOT EXISTS stopped_payments (
    id            TEXT PRIMARY KEY,                -- stp_<uuid>
    workspace_id  TEXT NOT NULL,
    agent_id      TEXT NOT NULL,
    approval_id   TEXT NOT NULL,
    payment_ref   TEXT NOT NULL,                   -- the stopped payment's reference in Lens
    shown         JSONB NOT NULL,                  -- what the owner was shown and approved
    sent          JSONB NOT NULL,                  -- what would have been sent
    changed       TEXT[] NOT NULL,                 -- of payee, amount, currency, agent, those that differed, in that order
    created_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS stopped_payments_approval ON stopped_payments (approval_id, created_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON stopped_payments
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON stopped_payments
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
