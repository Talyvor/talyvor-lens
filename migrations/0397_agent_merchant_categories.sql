-- B61.43 — an agent's rules hold merchants and categories.
--
-- allowed_merchant_categories and blocked_merchant_categories hold the merchant categories an agent may pay in by card
-- and those it may never pay in: each a category's id (software, hosting, freight, …) or one four-digit merchant
-- category code (ISO 18245). A card payment is judged by the category code its issuer reports, inside the payment's
-- movement under the agent's row lock, with its other rules. Once allowed_merchant_categories names any, a card payment
-- in another category, or in one the issuer did not report, is refused. Empty is no rule.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS allowed_merchant_categories TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS blocked_merchant_categories TEXT[] NOT NULL DEFAULT '{}';

-- Every version of an agent's rules (0192) names them as its rules hold them now, so the next save that changes nothing
-- is not a new version, and a rollback to a version taken before them keeps them as they are.
UPDATE agent_rules_versions v
   SET rules = v.rules || jsonb_build_object('allowed_merchant_categories', r.allowed_merchant_categories,
                                             'blocked_merchant_categories', r.blocked_merchant_categories)
  FROM agent_rules r
 WHERE r.agent_id = v.agent_id AND NOT v.rules ? 'allowed_merchant_categories';
