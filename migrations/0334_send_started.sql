-- 0334_send_started.sql — B54.17: a resend after an unknown send never fails a payment that may have paid.
--
-- money_payments.send_started_at is when a payment out was first handed to its rail; money_earnings_payouts.sent_to is
-- the destination an earnings payout was first handed to its partner for. From then on it may have gone: a resend
-- skips the Travel Rule step and screening, never fails on them, and asks the rail what happened under the same key —
-- a payout only while its details are still the ones it was sent to. An earnings payout's Travel Rule send that keeps
-- failing ends it as failed after a bounded number of tries, as a payment's does (travel_rule_attempts, 0319).
ALTER TABLE money_payments ADD COLUMN IF NOT EXISTS send_started_at TIMESTAMPTZ;
ALTER TABLE money_earnings_payouts ADD COLUMN IF NOT EXISTS sent_to TEXT;
ALTER TABLE money_earnings_payouts ADD COLUMN IF NOT EXISTS travel_rule_attempts INT NOT NULL DEFAULT 0;

-- A payment pending before this column, its Travel Rule never tried, was handed to its rail or may have been: safer
-- counted as gone. Earnings payouts are not backfilled: what they were sent to is not known, so they are screened as
-- before, never paid to details nobody screened.
UPDATE money_payments SET send_started_at = created_at WHERE status = 'pending' AND travel_rule_attempts = 0 AND send_started_at IS NULL;
