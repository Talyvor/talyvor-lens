-- B61.42: an approval says what the rules asked above and what they leave this month in the rules' currency
-- (economy/agent_rules.go, fileApproval; agent_self_service.go).
--
-- rules_currency:          the currency the agent's rules were held in when it was filed; '' on one filed before this.
-- asks_above_minor:        the amount the rules asked above, in rules_currency's minor units.
-- budget_left_after_minor: the monthly limit less the month's spend and this amount, in rules_currency's minor units;
--                          NULL with no monthly limit.
-- The µLXC columns of 0343 stay for the parked gateway. Rows from before keep NULLs here: there is no rate to move
-- them by, and a sheet reads them as it did.
ALTER TABLE agent_approvals
    ADD COLUMN IF NOT EXISTS rules_currency TEXT NOT NULL DEFAULT '' CHECK (rules_currency IN ('', 'GBP', 'EUR', 'USD', 'USDC')),
    ADD COLUMN IF NOT EXISTS asks_above_minor BIGINT,
    ADD COLUMN IF NOT EXISTS budget_left_after_minor BIGINT;
