-- B41.150 — a room's contributor agreement, e-signed before a contributor's points count.
--
-- The agreement assigns a contributor's work in the room to the company formed to own it, with a licence back if no
-- company is ever formed (CDPA s90(3) needs the assignment in writing). Its text lives in internal/rooms/agreement.go
-- under a version; a signature keeps the version, the hash of the exact text signed, the name typed, the person who
-- signed and when, as evidence. Points are written only for a workspace that signed the current version. Rows are
-- never changed or removed.
CREATE TABLE IF NOT EXISTS room_agreement_signatures (
    room_id           TEXT NOT NULL,      -- no foreign key: the evidence outlives the room
    workspace_id      TEXT NOT NULL,      -- the contributor
    agreement_version TEXT NOT NULL,
    text_sha256       TEXT NOT NULL CHECK (text_sha256 ~ '^[0-9a-f]{64}$'),
    signed_name       TEXT NOT NULL CHECK (length(btrim(signed_name)) BETWEEN 1 AND 200),
    user_id           TEXT NOT NULL CHECK (user_id <> ''),
    signed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, workspace_id, agreement_version)
);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON room_agreement_signatures
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON room_agreement_signatures
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
