-- B52.44: deleting a person's last account ends their sign-in record (workspace.endPersonSignIn). Until Nicolai says
-- whether a sign-in or fraud record must be kept, the people row is kept anonymised rather than deleted: deleted_at
-- says when, and its number and identity become 'deleted:' and its id, so the number and the identity are free to
-- sign up again as a new person and nothing found by either finds this row. Its keys, passcode and tickets are gone.
ALTER TABLE people ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE people DROP CONSTRAINT IF EXISTS people_phone_check;
ALTER TABLE people ADD CONSTRAINT people_phone_check CHECK (phone ~ '^\+[1-9][0-9]{6,14}$'
    OR (deleted_at IS NOT NULL AND phone = 'deleted:' || id));
