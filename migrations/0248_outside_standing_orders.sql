-- B30.18 — standing orders: a schedule that pays a saved outside payee (B30.16) through the account partner.
--
-- A schedule pays another agent (B19.8), a marketplace listing (B19.17) or now an outside payee: amount_minor of currency
-- to to_payee_id, each tick one payment out (B30.17) judged by the agent's rules when it runs, under the key
-- "schedule:<id>:<tick>" so a tick run twice pays once; its run row names the payment. end_at ends the schedule after
-- its last tick on or before it. A tick the owner skips is a run with outcome 'skipped'.
ALTER TABLE agent_payment_schedules ADD COLUMN IF NOT EXISTS to_payee_id TEXT;
ALTER TABLE agent_payment_schedules ADD COLUMN IF NOT EXISTS amount_minor BIGINT NOT NULL DEFAULT 0 CHECK (amount_minor >= 0);
ALTER TABLE agent_payment_schedules ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_payment_schedules ADD COLUMN IF NOT EXISTS end_at TIMESTAMPTZ;
ALTER TABLE agent_payment_schedules DROP CONSTRAINT IF EXISTS agent_payment_schedules_one_payee;
ALTER TABLE agent_payment_schedules ADD CONSTRAINT agent_payment_schedules_one_payee
    CHECK ((to_agent_id IS NOT NULL)::int + (to_listing_id IS NOT NULL)::int + (to_payee_id IS NOT NULL)::int = 1);
ALTER TABLE agent_payment_schedules DROP CONSTRAINT IF EXISTS agent_payment_schedules_amount_ulxc_check;
ALTER TABLE agent_payment_schedules ADD CONSTRAINT agent_payment_schedules_amount_ulxc_check
    CHECK (amount_ulxc > 0 OR (to_payee_id IS NOT NULL AND amount_minor > 0));
ALTER TABLE agent_schedule_runs ADD COLUMN IF NOT EXISTS payment_id TEXT;
ALTER TABLE agent_schedule_runs DROP CONSTRAINT IF EXISTS agent_schedule_runs_outcome_check;
ALTER TABLE agent_schedule_runs ADD CONSTRAINT agent_schedule_runs_outcome_check CHECK (outcome IN ('paid', 'refused', 'skipped'));
