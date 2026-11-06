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

-- One pending before these columns was handed to its rail, or may have been: safer counted as gone. A payout's
-- destination is its recipient's verified details now: the best that is known of it.
UPDATE money_payments SET send_started_at = created_at WHERE status = 'pending' AND send_started_at IS NULL;
UPDATE money_earnings_payouts e SET sent_to = d.holder || '|' || d.iban || '|' || d.bic || '|' || d.network || '|' || lower(d.address)
    FROM money_payout_details d WHERE d.workspace_id = e.recipient_workspace_id AND d.asset = e.currency AND d.status = 'verified'
    AND e.status = 'pending' AND e.partner_ref = '' AND e.sent_to IS NULL;
