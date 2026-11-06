-- 0335_key_enrolments.sql — B55.1: a new key joins from another device (internal/economy/key_enrolments.go).
--
-- key_enrolments: a passkey or phone key registered with no signature in a workspace that already has a key. It waits
--                 here, signing nothing, until an existing key of the workspace approves it over a challenge naming it
--                 (it is then copied into workspace_passkeys or workspace_device_keys) or the owner declines it. It
--                 can be approved for 10 minutes; code is the 6 digits both devices show, from the SHA-256 of
--                 public_key. credential_id and sign_count are a passkey's; person and attestation a phone key's.
CREATE TABLE IF NOT EXISTS key_enrolments (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('passkey', 'device')),
    name          TEXT NOT NULL DEFAULT '',
    platform      TEXT NOT NULL DEFAULT '',
    person        TEXT NOT NULL DEFAULT '',
    credential_id TEXT NOT NULL DEFAULT '',
    public_key    BYTEA NOT NULL,
    sign_count    BIGINT NOT NULL DEFAULT 0,
    attestation   BYTEA NOT NULL DEFAULT '',
    code          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'approved', 'declined')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    decided_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_key_enrolments_waiting ON key_enrolments (workspace_id, expires_at) WHERE status = 'waiting';
