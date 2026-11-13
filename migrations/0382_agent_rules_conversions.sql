-- B30.29 — convert between currencies: an agent's rules gain the currencies it may convert between (empty allows any)
-- and its daily conversion maximum (NULL is no limit, as for the other limits), judged on its money_conversions (0358).

ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS allowed_currencies TEXT[] NOT NULL DEFAULT '{}'
    CHECK (allowed_currencies <@ ARRAY['GBP', 'EUR', 'USD', 'USDC']::text[]);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS fx_daily_limit_ulxc BIGINT CHECK (fx_daily_limit_ulxc > 0);

-- Every version of an agent's rules (0192) names the two as its rules hold them now, so the next save that changes
-- nothing is not a new version, and a rollback to a version taken before them keeps them as they are.
UPDATE agent_rules_versions v
   SET rules = v.rules || jsonb_build_object('allowed_currencies', to_jsonb(r.allowed_currencies),
                                             'fx_daily_limit_ulxc', r.fx_daily_limit_ulxc)
  FROM agent_rules r
 WHERE r.agent_id = v.agent_id AND NOT v.rules ? 'allowed_currencies';

CREATE INDEX IF NOT EXISTS idx_money_conversions_agent_converted ON money_conversions (agent_id, converted_at)
    WHERE status = 'converted';
