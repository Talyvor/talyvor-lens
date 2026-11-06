-- 0334_send_started.sql — B54.17: a resend after an unknown send never fails a payment that may have paid.
--
-- send_started_at is when a USDC payment out (money_payments) or an earnings payout (money_earnings_payouts) was first
-- handed to its rail. From then on it may have gone: a resend skips the Travel Rule step and screening, never fails on
-- them, and asks the rail what happened under the same key. An earnings payout's Travel Rule send that keeps failing
-- ends it as failed after a bounded number of tries, as a payment's does (travel_rule_attempts, 0319).
ALTER TABLE money_payments ADD COLUMN IF NOT EXISTS send_started_at TIMESTAMPTZ;
ALTER TABLE money_earnings_payouts ADD COLUMN IF NOT EXISTS send_started_at TIMESTAMPTZ;
ALTER TABLE money_earnings_payouts ADD COLUMN IF NOT EXISTS travel_rule_attempts INT NOT NULL DEFAULT 0;

-- Every one pending before this column was handed to its rail, or may have been: safer counted as gone.
UPDATE money_payments SET send_started_at = created_at WHERE status = 'pending' AND send_started_at IS NULL;
UPDATE money_earnings_payouts SET send_started_at = created_at WHERE status = 'pending' AND send_started_at IS NULL;
