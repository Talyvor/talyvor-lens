-- 0394_person_signin_keys.sql — B61.24: a passkey signs its person in (internal/phonesignin, keys.go).
--
-- person_keys:            a person's sign-in keys: a web passkey (WebAuthn, ES256, its signature counter) or the phone
--                         app's device key (internal/devicekey). A removed key keeps its row, so it signs nobody in and
--                         is never registered again. These are the person's, for signing in only: a workspace's
--                         approval keys stay in workspace_passkeys and workspace_device_keys, and signing in adds none.
-- person_key_challenges:  one use, 5 minutes. A sign-in challenge is bound to the origin it was asked from (empty for
--                         the phone's key) and, when the person was named, to that person; a registration challenge to
--                         the person registering.
-- person_key_tickets:     what a sign-in gives: one use, 5 minutes, kept only as a hash, naming the person and the key.
--
-- Sign-ins, refusals, keys added and removed are rows in phone_signin_events (0392), with no number when the person
-- has none to hand: that table is the door's one audit trail.
CREATE TABLE IF NOT EXISTS person_keys (
    id            TEXT PRIMARY KEY,
    person_id     TEXT NOT NULL REFERENCES people (id),
    kind          TEXT NOT NULL CHECK (kind IN ('passkey', 'device')),
    credential_id TEXT UNIQUE,                 -- base64url, a passkey's; NULL for a device key
    platform      TEXT NOT NULL DEFAULT '',    -- a device key's: ios, android or test
    name          TEXT NOT NULL DEFAULT '',
    public_key    BYTEA NOT NULL,              -- SubjectPublicKeyInfo DER, P-256
    sign_count    BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    removed_at    TIMESTAMPTZ,
    CHECK ((kind = 'passkey') = (credential_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS person_keys_person ON person_keys (person_id, created_at);

CREATE TABLE IF NOT EXISTS person_key_challenges (
    challenge  TEXT PRIMARY KEY,               -- base64url of 32 random bytes
    purpose    TEXT NOT NULL CHECK (purpose IN ('signin', 'register')),
    person_id  TEXT REFERENCES people (id),    -- NULL: a sign-in any person's key may answer
    origin     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    CHECK (purpose = 'signin' OR person_id IS NOT NULL)
);

CREATE TABLE IF NOT EXISTS person_key_tickets (
    ticket_hash BYTEA PRIMARY KEY,
    person_id   TEXT NOT NULL REFERENCES people (id),
    key_id      TEXT NOT NULL REFERENCES person_keys (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ
);

ALTER TABLE phone_signin_events DROP CONSTRAINT IF EXISTS phone_signin_events_event_check;
ALTER TABLE phone_signin_events ADD CONSTRAINT phone_signin_events_event_check CHECK (event IN ('started', 'finished',
    'wrong_code', 'ended', 'refused', 'limited', 'joined', 'signed_in', 'key_signed_in', 'key_refused', 'key_added',
    'key_removed'));
