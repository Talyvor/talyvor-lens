-- B62.1 — a proof record for every payment an owner approved.
--
-- An approval the owner signs is signed over exactly what the sheet shows them: the challenge is the SHA-256 of a
-- statement naming the approval, the agent, the payee, the amount, the currency, the rule that asked and the rules
-- version (webauthn_challenges.statement). The decision keeps that statement, the signature and the key on the
-- approval, with the rule and rules version it was filed under.
--
-- payment_proofs is one row per payment the rules judged and let through, written in the payment's own transaction:
-- what the owner was shown and signed (none for a payment inside the rules, which names the rule that passed it), and
-- what was sent. It is append-only, as the audit trail is (0055): every UPDATE, DELETE and TRUNCATE raises.

ALTER TABLE webauthn_challenges ADD COLUMN IF NOT EXISTS statement TEXT NOT NULL DEFAULT '';

ALTER TABLE agent_approvals
    ADD COLUMN IF NOT EXISTS rule TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS rules_version INT,
    ADD COLUMN IF NOT EXISTS owner_statement TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS owner_signature JSONB,
    ADD COLUMN IF NOT EXISTS owner_key_kind TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS owner_key_id TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS payment_proofs (
    id            TEXT PRIMARY KEY,                -- prf_<uuid>
    workspace_id  TEXT NOT NULL,
    agent_id      TEXT NOT NULL,
    payment_ref   TEXT NOT NULL,                   -- the payment's reference in Lens: its idempotency key or request id
    approval_id   TEXT NOT NULL DEFAULT '',        -- '' when no approval was needed
    rule          TEXT NOT NULL,                   -- the rule that sent it to the owner, or passed it
    rules_version INT,                             -- the agent's rules version then; null with no rules
    shown         JSONB NOT NULL,                  -- what the owner was shown, or the payment as the rules judged it
    statement     TEXT NOT NULL DEFAULT '',        -- the statement signed, exactly as its hash was signed
    signature     JSONB,                           -- the assertion over it; null when nothing was signed
    key_kind      TEXT NOT NULL DEFAULT '',        -- passkey | device | approver_agent | ''
    key_id        TEXT NOT NULL DEFAULT '',
    signed_at     TIMESTAMPTZ,
    sent          JSONB NOT NULL,                  -- the payee, amount, currency, rail and reference sent
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, payment_ref)
);
CREATE INDEX IF NOT EXISTS payment_proofs_workspace ON payment_proofs (workspace_id, created_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON payment_proofs
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON payment_proofs
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
