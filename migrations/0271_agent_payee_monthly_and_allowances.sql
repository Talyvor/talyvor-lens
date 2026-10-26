-- B41.104 — "Approve and always allow this": a payee's monthly cap, and payees the agent may pay without approval.
--
-- payee_monthly_limits_ulxc is payee_daily_limits_ulxc (0191) over the month, in the agent's timezone: a payment past
-- it is refused. payee_allowances maps a payee's id to {"amount_ulxc", "per": "day" | "month"}: a payment above the
-- approval amount that keeps what the agent paid that payee in the day or the month within it needs no approval.
-- Both count agent_payee_payments, as the daily caps do.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS payee_monthly_limits_ulxc JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(payee_monthly_limits_ulxc) = 'object');
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS payee_allowances JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(payee_allowances) = 'object');
