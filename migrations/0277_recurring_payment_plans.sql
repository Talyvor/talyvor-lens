-- B45.13 — recurring payments from a pasted list: a plan of standing orders and the one mandate the owner signs for them.
--
-- An assistant prepares a plan (prepare_recurring_payments): each payment becomes a standing order (B30.18) to a saved
-- payee, inactive and awaiting_signature, and the plan keeps the permission mandate draft (B40.2) drawn up for them. When
-- the owner signs exactly that draft, its standing orders start. Each tick is then a payment request (B40.3) prepared by
-- the client "schedule:<plan id>", which only that mandate names: inside it the tick pays on its own, outside it — and on
-- a standing order's first payment — it waits for the owner's approval (B40.5). A plan's schedule may be the person's
-- own, with no agent paying.
CREATE TABLE IF NOT EXISTS recurring_payment_plans (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    args_hash       TEXT NOT NULL,
    mandate_id      TEXT NOT NULL UNIQUE,
    canonical       TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);

ALTER TABLE agent_payment_schedules ALTER COLUMN from_agent_id DROP NOT NULL;
ALTER TABLE agent_payment_schedules ADD COLUMN IF NOT EXISTS recurring_plan_id TEXT REFERENCES recurring_payment_plans (id);
ALTER TABLE agent_payment_schedules ADD COLUMN IF NOT EXISTS awaiting_signature BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_payment_schedules DROP CONSTRAINT IF EXISTS agent_payment_schedules_payer;
ALTER TABLE agent_payment_schedules ADD CONSTRAINT agent_payment_schedules_payer
    CHECK (from_agent_id IS NOT NULL OR (recurring_plan_id IS NOT NULL AND to_payee_id IS NOT NULL));
CREATE INDEX IF NOT EXISTS idx_agent_payment_schedules_plan ON agent_payment_schedules (recurring_plan_id) WHERE recurring_plan_id IS NOT NULL;

ALTER TABLE agent_schedule_runs ADD COLUMN IF NOT EXISTS request_id TEXT;
ALTER TABLE agent_schedule_runs DROP CONSTRAINT IF EXISTS agent_schedule_runs_outcome_check;
ALTER TABLE agent_schedule_runs ADD CONSTRAINT agent_schedule_runs_outcome_check
    CHECK (outcome IN ('paid', 'refused', 'skipped', 'awaiting_approval'));
