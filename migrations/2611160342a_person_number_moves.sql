-- 2611160342a_person_number_moves.sql — B61.30: the way back in for a person who has neither their number nor a key
-- (internal/phonesignin/move.go, internal/economy/number_move.go).
--
-- person_number_moves: one row each time an operator moved a person's number, behind step-up, after the person's
--                      identity check passed again against the name and date of birth on record. It names the
--                      operator and the check, and keeps the old and new numbers. emails_claimed_at says the BFF has
--                      taken the move to tell the person's email addresses (POST /v1/auth/phone/number-moves/claim).
-- number_move_holds:   one row an account of the moved person: until held_until the account can look and cannot move
--                      money. Every money capability that honours a compliance freeze honours this hold too.
CREATE TABLE IF NOT EXISTS person_number_moves (
    id                 TEXT PRIMARY KEY,
    person_id          TEXT NOT NULL REFERENCES people (id),
    old_phone          TEXT NOT NULL CHECK (old_phone <> ''),
    new_phone          TEXT NOT NULL CHECK (new_phone <> '' AND new_phone <> old_phone),
    operator           TEXT NOT NULL CHECK (operator <> ''),
    identity_check_ref TEXT NOT NULL DEFAULT '',
    moved_at           TIMESTAMPTZ NOT NULL,
    held_until         TIMESTAMPTZ NOT NULL,
    emails_claimed_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS person_number_moves_person ON person_number_moves (person_id, moved_at DESC);
CREATE INDEX IF NOT EXISTS person_number_moves_unclaimed ON person_number_moves (moved_at) WHERE emails_claimed_at IS NULL;

CREATE TABLE IF NOT EXISTS number_move_holds (
    workspace_id TEXT PRIMARY KEY CHECK (workspace_id <> ''),
    move_id      TEXT NOT NULL REFERENCES person_number_moves (id),
    held_until   TIMESTAMPTZ NOT NULL
);

ALTER TABLE phone_signin_events DROP CONSTRAINT IF EXISTS phone_signin_events_event_check;
ALTER TABLE phone_signin_events ADD CONSTRAINT phone_signin_events_event_check CHECK (event IN ('started', 'finished',
    'wrong_code', 'ended', 'refused', 'limited', 'joined', 'signed_in', 'key_signed_in', 'key_refused', 'key_added',
    'key_removed', 'number_moved'));
