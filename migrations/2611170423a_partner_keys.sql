-- B62.5 — a read-only view for the account partner's risk team.
--
-- partner_keys is the partner's own kind of key (tlv_prk_): it reads GET /v1/partner/agent-activity, its CSV and its
-- summary, and nothing else, and never writes. An operator makes and revokes one with the admin key and a step-up code.
-- The raw key is shown once, at creation; only its sha256 is kept.
--
-- partner_key_uses is one row per read, written before the read is answered, and append-only as the audit trail is
-- (0055): every UPDATE, DELETE and TRUNCATE raises.

CREATE TABLE IF NOT EXISTS partner_keys (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL CHECK (name <> ''),
    key_hash    TEXT NOT NULL UNIQUE,
    key_prefix  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,
    revoked_by  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS partner_key_uses (
    id          BIGSERIAL PRIMARY KEY,
    key_id      BIGINT NOT NULL REFERENCES partner_keys (id),
    method      TEXT NOT NULL,
    path        TEXT NOT NULL,
    query       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS partner_key_uses_key ON partner_key_uses (key_id, id DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON partner_key_uses
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON partner_key_uses
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- The view lists an approval only until its payment has a proof record, so it looks proofs up by approval.
CREATE INDEX IF NOT EXISTS payment_proofs_approval ON payment_proofs (approval_id) WHERE approval_id <> '';
