-- 2611160622b_payee_caps_in_rules_currency.sql — B61.40: payee caps, "always allow" and the daily conversion maximum
-- in the rules' currency (economy/agent_rules_currency.go).
--
-- agent_rules gains each payee's daily and monthly cap and the daily conversion maximum in the minor units of the
-- rules' currency, and each "always allow" in payee_allowances gains its amount_minor beside its amount_ulxc. Every
-- payment is judged by these from now on; the µLXC forms stay for the parked gateway to read.
--
-- Rules in dollars move once at the peg, as 0393 moved their other limits (LXC 1 = $0.10, so 100,000 µLXC is one
-- cent): exact in whole cents, and a part of a cent keeps its whole cents, never fewer than one, so no cap becomes no
-- cap and none is raised. Rules saved in pounds or euros since 0393 keep their µLXC caps, judged in µLXC as before
-- until the owner sets them in the currency: there is no rate here to move them by. Run on an empty database it
-- changes nothing.
--
-- agent_payee_payments gains what each payment was in its own currency, so what an agent has paid a payee is counted
-- in the rules' currency exactly. Rows from before keep only amount_ulxc and are valued from it.

ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS payee_daily_limits_minor JSONB NOT NULL DEFAULT '{}';
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS payee_monthly_limits_minor JSONB NOT NULL DEFAULT '{}';
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS fx_daily_limit_minor BIGINT CHECK (fx_daily_limit_minor > 0);

ALTER TABLE agent_payee_payments ADD COLUMN IF NOT EXISTS currency TEXT CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC'));
ALTER TABLE agent_payee_payments ADD COLUMN IF NOT EXISTS amount_minor BIGINT CHECK (amount_minor > 0);
ALTER TABLE agent_payee_payments DROP CONSTRAINT IF EXISTS agent_payee_payments_own_amount;
ALTER TABLE agent_payee_payments ADD CONSTRAINT agent_payee_payments_own_amount CHECK ((currency IS NULL) = (amount_minor IS NULL));

-- payee caps in µLXC at the peg, in cents: {"id": ulxc} to {"id": cents}, the zeros (no cap) left out.
CREATE OR REPLACE FUNCTION pg_temp.b6140_caps(caps JSONB) RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
    SELECT COALESCE(jsonb_object_agg(key, GREATEST(value::bigint / 100000, 1)), '{}')
      FROM jsonb_each_text(COALESCE(caps, '{}')) WHERE value::bigint > 0
$$;

-- each "always allow" with its amount at the peg in cents beside its µLXC amount.
CREATE OR REPLACE FUNCTION pg_temp.b6140_allowances(allowances JSONB) RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
    SELECT COALESCE(jsonb_object_agg(key, value || jsonb_build_object('amount_minor',
               CASE WHEN (value->>'amount_ulxc')::bigint > 0 THEN GREATEST((value->>'amount_ulxc')::bigint / 100000, 1) ELSE 0 END)), '{}')
      FROM jsonb_each(COALESCE(allowances, '{}'))
$$;

UPDATE agent_rules
   SET payee_daily_limits_minor = pg_temp.b6140_caps(payee_daily_limits_ulxc),
       payee_monthly_limits_minor = pg_temp.b6140_caps(payee_monthly_limits_ulxc),
       payee_allowances = pg_temp.b6140_allowances(payee_allowances),
       fx_daily_limit_minor = CASE WHEN fx_daily_limit_ulxc > 0 THEN GREATEST(fx_daily_limit_ulxc / 100000, 1) END
 WHERE currency = 'USD';

-- Every version of an agent's rules (0192) is moved the same way, so a rollback to it puts back the caps it had, and
-- the next save that changes nothing is not a new version. A version in pounds or euros gains the empty forms its row
-- gained.
UPDATE agent_rules_versions v
   SET rules = v.rules || CASE WHEN COALESCE(v.rules->>'currency', 'USD') = 'USD' THEN jsonb_build_object(
           'payee_daily_limits_minor', pg_temp.b6140_caps(v.rules->'payee_daily_limits_ulxc'),
           'payee_monthly_limits_minor', pg_temp.b6140_caps(v.rules->'payee_monthly_limits_ulxc'),
           'payee_allowances', pg_temp.b6140_allowances(v.rules->'payee_allowances'),
           'fx_daily_limit_minor', CASE WHEN (v.rules->>'fx_daily_limit_ulxc')::bigint > 0
               THEN GREATEST((v.rules->>'fx_daily_limit_ulxc')::bigint / 100000, 1) END)
       ELSE jsonb_build_object('payee_daily_limits_minor', '{}'::jsonb, 'payee_monthly_limits_minor', '{}'::jsonb,
           'fx_daily_limit_minor', NULL) END
 WHERE NOT v.rules ? 'payee_daily_limits_minor';
