-- 0399_person_key_tightenings.sql — B61.81: five tightenings to a person's sign-in keys (internal/phonesignin, keys.go).
--
-- person_keys_public_key:          one key is kept once, for one person: two adds of the same key at the same instant,
--                                  for two people, both passed the "kept already" read, which runs under each person's
--                                  own lock. A removed key keeps its row, so it is never kept again either.
-- person_key_challenges_expires,
-- person_key_tickets_expires:      a challenge or ticket a day past its time is deleted when a new one is issued.
CREATE UNIQUE INDEX IF NOT EXISTS person_keys_public_key ON person_keys (public_key);
CREATE INDEX IF NOT EXISTS person_key_challenges_expires ON person_key_challenges (expires_at);
CREATE INDEX IF NOT EXISTS person_key_tickets_expires ON person_key_tickets (expires_at);
