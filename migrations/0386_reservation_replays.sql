-- B50.47 — a question retried under the same Idempotency-Key after its hold was given back is charged.
--
-- replays counts the requests an Idempotency-Key retry served on this hold. A replayed hold may pay for an answer
-- another request is giving, so only the sweeper gives it back, never a single request's release — as 0375 does
-- for the pre-serve debit's claim. A release moves the hold off its reservation_id, so a retry takes a new one.
ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS replays INTEGER NOT NULL DEFAULT 0;
