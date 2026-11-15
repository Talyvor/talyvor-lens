-- 0394_two_plan_ladders.sql — B61.57: Talyvor's own plans, two ladders, chosen by the account's type (internal/plans).
--
-- workspaces.plan is the plan the account chose, or this migration moved it to: Solo, Crew or Fleet for a personal
-- account, Studio, Company or Group for a business (Custom, business_custom, is a contract's and the testers'). NULL is
-- the first step of the account's ladder, which is where a new account starts. A plan of the other type's ladder is
-- refused here as well as by the route.
--
-- Every existing workspace moves by its type and its 5 Oct plan (a paying subscription's, else a synthetic one's, else
-- free): free to the first step; team to the second, and with it byok, which took team's gates, and the chat plans
-- plus, pro and max, which took team's fees, so none pays more or loses an agent; business to the third; enterprise to
-- Custom, a personal one to Fleet. A business on the operator's contract moves to what it would be on without one, and
-- the contract keeps it on Custom while it lasts; a personal account on a contract moves to Fleet, which it keeps once
-- the contract no longer counts for it. No agent is archived: one past its new plan's count keeps what it has and
-- cannot add.
--
-- workspace_contracts.plan names Custom. workspaces.synthetic_plan is no longer read: the testers' route sets plan.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS plan TEXT;

ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_plan_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_plan_check CHECK (
    plan IS NULL
    OR (account_type = 'personal' AND plan IN ('personal_solo', 'personal_crew', 'personal_fleet'))
    OR (account_type = 'business' AND plan IN ('business_studio', 'business_company', 'business_group', 'business_custom')));

ALTER TABLE workspace_contracts DROP CONSTRAINT IF EXISTS workspace_contracts_plan_check;
UPDATE workspace_contracts SET plan = 'business_custom' WHERE plan = 'enterprise';
ALTER TABLE workspace_contracts ADD CONSTRAINT workspace_contracts_plan_check CHECK (plan = 'business_custom');

WITH old AS (
    SELECT w.id, w.account_type,
           EXISTS (SELECT 1 FROM workspace_contracts c WHERE c.workspace_id = w.id) AS contracted,
           COALESCE((SELECT s.plan FROM subscriptions s
                     WHERE s.workspace_id = w.id AND s.status IN ('trialing', 'active', 'past_due')),
                    w.synthetic_plan, 'free') AS plan
    FROM workspaces w
    WHERE w.plan IS NULL
)
UPDATE workspaces w SET plan = CASE
    WHEN old.account_type = 'personal' AND old.contracted THEN 'personal_fleet'
    WHEN old.account_type = 'personal' THEN CASE
        WHEN old.plan IN ('team', 'byok', 'plus', 'pro', 'max') THEN 'personal_crew'
        WHEN old.plan IN ('business', 'enterprise') THEN 'personal_fleet'
        ELSE 'personal_solo' END
    ELSE CASE
        WHEN old.plan IN ('team', 'byok', 'plus', 'pro', 'max') THEN 'business_company'
        WHEN old.plan = 'business' THEN 'business_group'
        WHEN old.plan = 'enterprise' THEN 'business_custom'
        ELSE 'business_studio' END
    END
FROM old
WHERE w.id = old.id;
