-- B40.5 — one approval for a whole batch, then each item paid exactly once, with a hash-chained record.
-- A prepared request (B40.3) is approved once: the person signs, with a passkey or the phone's device key, a statement
-- naming the request's frozen payload hash, the items approved (payee, amount, reference), their totals and an expiry.
-- `payment_request_approvals` keeps that statement and signature; its primary key makes a second approval impossible.
-- Each item then records the payment that paid it, where it stands, and the authority it was paid under ("approval"
-- or "mandate:<id>"). `audit_chain` is the append-only, hash-chained record of every step, per subject (a request id):
-- each row's hash is the SHA-256 of the previous row's hash, its sequence number, its kind and its body.
ALTER TABLE payment_requests DROP CONSTRAINT IF EXISTS payment_requests_status_check;
ALTER TABLE payment_requests ADD CONSTRAINT payment_requests_status_check
    CHECK (status IN ('pending_approval', 'auto', 'blocked', 'cancelled', 'approved'));

ALTER TABLE payment_request_items
    ADD COLUMN IF NOT EXISTS payment_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pay_status TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pay_detail TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS authority  TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS payment_request_approvals (
    request_id         TEXT PRIMARY KEY REFERENCES payment_requests (id),
    workspace_id       TEXT NOT NULL,
    statement          TEXT NOT NULL,
    credential_id      TEXT NOT NULL DEFAULT '',
    device_id          TEXT NOT NULL DEFAULT '',
    client_data_json   TEXT NOT NULL DEFAULT '',
    authenticator_data TEXT NOT NULL DEFAULT '',
    signature          TEXT NOT NULL,
    approved_by        TEXT NOT NULL DEFAULT '',
    approved_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON payment_request_approvals
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON payment_request_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

CREATE TABLE IF NOT EXISTS audit_chain (
    workspace_id TEXT NOT NULL,
    subject      TEXT NOT NULL,
    seq          BIGINT NOT NULL CHECK (seq > 0),
    kind         TEXT NOT NULL,
    body         TEXT NOT NULL,                                     -- JSON, kept byte for byte as hashed
    prev_hash    TEXT NOT NULL,
    hash         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, subject, seq)
);
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON audit_chain
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON audit_chain
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
