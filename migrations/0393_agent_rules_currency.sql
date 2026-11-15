-- B61.12 — an agent's rules are held and judged in the account's own currency.
--
-- The rules gain the currency the owner typed them in, and their limits per payment, hour, day, week and month and
-- the amount above which a payment asks the owner, in that currency's minor units (NULL is no limit, as for the
-- µLXC limits). Every movement is judged by these from now on; the µLXC columns stay for the parked gateway to read.
--
-- Existing rules move once to dollars at the peg (LXC 1 = $0.10, so 100,000 µLXC is one cent): exact for every limit
-- in whole cents, and a limit with a part of a cent keeps its whole cents — never fewer than one, so no limit
-- becomes "no limit" and no limit is raised. Run on an empty database it changes nothing.

ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'USD'
    CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC'));
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS max_per_payment_minor BIGINT CHECK (max_per_payment_minor > 0);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS hourly_limit_minor BIGINT CHECK (hourly_limit_minor > 0);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS daily_limit_minor BIGINT CHECK (daily_limit_minor > 0);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS weekly_limit_minor BIGINT CHECK (weekly_limit_minor > 0);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS monthly_limit_minor BIGINT CHECK (monthly_limit_minor > 0);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS approval_above_minor BIGINT CHECK (approval_above_minor > 0);

UPDATE agent_rules
   SET max_per_payment_minor = CASE WHEN max_per_request_ulxc > 0 THEN GREATEST(max_per_request_ulxc / 100000, 1) END,
       hourly_limit_minor = CASE WHEN hourly_limit_ulxc > 0 THEN GREATEST(hourly_limit_ulxc / 100000, 1) END,
       daily_limit_minor = CASE WHEN daily_limit_ulxc > 0 THEN GREATEST(daily_limit_ulxc / 100000, 1) END,
       weekly_limit_minor = CASE WHEN weekly_limit_ulxc > 0 THEN GREATEST(weekly_limit_ulxc / 100000, 1) END,
       monthly_limit_minor = CASE WHEN monthly_limit_ulxc > 0 THEN GREATEST(monthly_limit_ulxc / 100000, 1) END,
       approval_above_minor = CASE WHEN approval_above_ulxc > 0 THEN GREATEST(approval_above_ulxc / 100000, 1) END
 WHERE max_per_payment_minor IS NULL AND hourly_limit_minor IS NULL AND daily_limit_minor IS NULL
   AND weekly_limit_minor IS NULL AND monthly_limit_minor IS NULL AND approval_above_minor IS NULL;

-- Every version of an agent's rules (0192) is moved the same way from its own µLXC limits, so a rollback to it puts
-- back the limits it had, and the next save that changes nothing is not a new version.
UPDATE agent_rules_versions v
   SET rules = v.rules || jsonb_build_object(
           'currency', 'USD',
           'max_per_payment_minor', CASE WHEN (v.rules->>'max_per_request_ulxc')::bigint > 0 THEN GREATEST((v.rules->>'max_per_request_ulxc')::bigint / 100000, 1) END,
           'hourly_limit_minor', CASE WHEN (v.rules->>'hourly_limit_ulxc')::bigint > 0 THEN GREATEST((v.rules->>'hourly_limit_ulxc')::bigint / 100000, 1) END,
           'daily_limit_minor', CASE WHEN (v.rules->>'daily_limit_ulxc')::bigint > 0 THEN GREATEST((v.rules->>'daily_limit_ulxc')::bigint / 100000, 1) END,
           'weekly_limit_minor', CASE WHEN (v.rules->>'weekly_limit_ulxc')::bigint > 0 THEN GREATEST((v.rules->>'weekly_limit_ulxc')::bigint / 100000, 1) END,
           'monthly_limit_minor', CASE WHEN (v.rules->>'monthly_limit_ulxc')::bigint > 0 THEN GREATEST((v.rules->>'monthly_limit_ulxc')::bigint / 100000, 1) END,
           'approval_above_minor', CASE WHEN (v.rules->>'approval_above_ulxc')::bigint > 0 THEN GREATEST((v.rules->>'approval_above_ulxc')::bigint / 100000, 1) END)
 WHERE NOT v.rules ? 'currency';

-- A boost (0193) may raise the new limits, in the rules' currency. A boost set before this raises a µLXC limit and
-- is judged as that limit in dollars at the peg.
ALTER TABLE agent_rule_boosts DROP CONSTRAINT IF EXISTS agent_rule_boosts_rule_check;
ALTER TABLE agent_rule_boosts ADD CONSTRAINT agent_rule_boosts_rule_check CHECK (rule IN ('max_per_request_ulxc',
    'hourly_limit_ulxc', 'daily_limit_ulxc', 'weekly_limit_ulxc', 'monthly_limit_ulxc', 'approval_above_ulxc',
    'requests_per_minute', 'max_per_payment_minor', 'hourly_limit_minor', 'daily_limit_minor', 'weekly_limit_minor',
    'monthly_limit_minor', 'approval_above_minor'));
