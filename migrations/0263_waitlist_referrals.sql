-- B45.35 — every waitlist signup gets its own random referral code, and remembers whose code brought it in.
-- New codes come from crypto/rand in cmd/lens/waitlist_handler.go; rows that predate this get a random one here.
-- Neither is ever derived from the email. referred_by is set once, on the row's first insert, and never changes.
ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS ref_code TEXT;
UPDATE waitlist_signups SET ref_code = substr(replace(gen_random_uuid()::text, '-', ''), 1, 10) WHERE ref_code IS NULL;
ALTER TABLE waitlist_signups ALTER COLUMN ref_code SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS waitlist_signups_ref_code_key ON waitlist_signups (ref_code);
ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS referred_by BIGINT REFERENCES waitlist_signups (id);
