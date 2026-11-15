-- 0395_person_passcodes.sql — B61.23: the passcode (internal/phonesignin/passcode.go).
--
-- person_passcodes: one row a person who has set one. Kept only as a slow salted hash (argon2id) of the passcode
--                   mixed with LENS_PASSCODE_SECRET, which lives outside the database, so these rows alone cannot be
--                   tried offline. off_at says ten wrong tries one way turned it off: it stays off until the person
--                   signs in with a key and sets a new one.
-- passcode_events:  every set, change, right and wrong try, wait and refusal. Never a digit of the passcode. way is the
--                   way in a try came: 'lock' (the phone app's lock, a change) or 'ticket' (signing in with a phone
--                   ticket); empty for a set or a change. The wrong rows are the count, per way and with no time
--                   window, on every replica: since the last right try that way, or the last set or change.
CREATE TABLE IF NOT EXISTS person_passcodes (
    person_id   TEXT PRIMARY KEY REFERENCES people (id) ON DELETE CASCADE,
    salt        BYTEA NOT NULL,
    hash        BYTEA NOT NULL,
    hash_params TEXT NOT NULL CHECK (hash_params <> ''),
    set_at      TIMESTAMPTZ NOT NULL,
    off_at      TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS passcode_events (
    id             BIGSERIAL PRIMARY KEY,
    person_id      TEXT NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    caller_address TEXT NOT NULL DEFAULT '',
    event          TEXT NOT NULL CHECK (event IN ('set', 'changed', 'set_after_key', 'right', 'wrong', 'waited', 'off', 'refused_off')),
    way            TEXT NOT NULL DEFAULT '' CHECK (way IN ('', 'lock', 'ticket')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS passcode_events_person_created ON passcode_events (person_id, created_at DESC);
