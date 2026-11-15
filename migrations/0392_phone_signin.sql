-- 0392_phone_signin.sql — B61.22: a person joins with a phone number and a texted code (internal/phonesignin).
--
-- people:              one row a person, found by their number in international form (E.164), one person a number.
--                      identity is the string POST /v1/provision is given for them, so their accounts hang off the
--                      person whichever way they signed in. phone_sandbox says the number was confirmed only by the
--                      Sandbox sender: it counts for nothing live and is asked again under a real sender.
-- phone_checks:        one row a code sent. The code is kept only as a keyed hash; a check ends when it is used,
--                      after 5 wrong tries, or after 10 minutes. The rows also count the texts for the limits.
-- phone_tickets:       what a right code gives: one use, 5 minutes, kept only as a hash. It says the number was
--                      confirmed just now and whether a person held it then.
-- phone_signin_events: every start, finish, refusal and limit reached. Never the code or the ticket.
--
-- None is keyed by a workspace: a person comes before any account.
CREATE TABLE IF NOT EXISTS people (
    id                 TEXT PRIMARY KEY,
    phone              TEXT NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{6,14}$'),
    identity           TEXT NOT NULL UNIQUE CHECK (identity <> ''),
    phone_sandbox      BOOLEAN NOT NULL,
    phone_confirmed_at TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS phone_checks (
    id             BIGSERIAL PRIMARY KEY,
    phone          TEXT NOT NULL CHECK (phone <> ''),
    caller_address TEXT NOT NULL DEFAULT '',
    code_salt      BYTEA NOT NULL,
    code_hash      BYTEA NOT NULL,
    sender         TEXT NOT NULL CHECK (sender <> ''),
    sandbox        BOOLEAN NOT NULL,
    wrong_tries    INT NOT NULL DEFAULT 0,
    ended_at       TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS phone_checks_phone_created ON phone_checks (phone, created_at DESC);
CREATE INDEX IF NOT EXISTS phone_checks_address_created ON phone_checks (caller_address, created_at DESC);

CREATE TABLE IF NOT EXISTS phone_tickets (
    ticket_hash  BYTEA PRIMARY KEY,
    phone        TEXT NOT NULL CHECK (phone <> ''),
    sandbox      BOOLEAN NOT NULL,
    had_person   BOOLEAN NOT NULL,
    used_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS phone_signin_events (
    id             BIGSERIAL PRIMARY KEY,
    phone          TEXT NOT NULL DEFAULT '',
    caller_address TEXT NOT NULL DEFAULT '',
    event          TEXT NOT NULL CHECK (event IN ('started', 'finished', 'wrong_code', 'ended', 'refused', 'limited', 'joined', 'signed_in')),
    detail         TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS phone_signin_events_phone ON phone_signin_events (phone, created_at DESC);
