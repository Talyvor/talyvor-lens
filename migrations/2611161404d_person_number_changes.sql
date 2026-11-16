-- B61.104: a person changes their own number (internal/phonesignin/change.go): a texted code to the new number and
-- their passcode, with a session of the person the suite BFF holds. The change is a person_number_moves row like an
-- operator's move (B61.30), so the BFF claims it the same way to tell the person's email addresses; changed_by says
-- who changed it. A person's own change names no operator and holds no account.
ALTER TABLE person_number_moves ADD COLUMN IF NOT EXISTS changed_by TEXT NOT NULL DEFAULT 'operator';
ALTER TABLE person_number_moves DROP CONSTRAINT IF EXISTS person_number_moves_changed_by_check;
ALTER TABLE person_number_moves ADD CONSTRAINT person_number_moves_changed_by_check CHECK (changed_by IN ('operator', 'person'));
ALTER TABLE person_number_moves DROP CONSTRAINT IF EXISTS person_number_moves_operator_check;
ALTER TABLE person_number_moves ADD CONSTRAINT person_number_moves_operator_check CHECK (
    (changed_by = 'operator' AND operator <> '') OR (changed_by = 'person' AND operator = ''));
