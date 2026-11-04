-- 0303_spend_plan.sql — B53.15: the Coach's month plan (internal/economy/spend_plan.go).
--
-- spend_plan_income: the owner's expected income — about amount_minor of currency on day payday of each month, from
--                    payer (empty: any money in) — set by them or confirmed from Lens's proposal; set_at is when, so
--                    only a payday after it can be late.
-- spend_notices gains the kind 'plan': each way the month plan is off, once.

CREATE TABLE IF NOT EXISTS spend_plan_income (
    workspace_id TEXT PRIMARY KEY,
    payday       INT NOT NULL CHECK (payday BETWEEN 1 AND 31),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency     TEXT NOT NULL,
    payer        TEXT NOT NULL DEFAULT '',
    set_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE spend_notices DROP CONSTRAINT IF EXISTS spend_notices_kind_check;
ALTER TABLE spend_notices ADD CONSTRAINT spend_notices_kind_check CHECK (kind IN ('payment', 'budget', 'unusual', 'tip', 'digest', 'plan'));
