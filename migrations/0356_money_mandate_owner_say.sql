-- B55.24 — a mandate needs its owner's signed say.
--
-- An agent's own key only asks for a mandate: it pulls nothing until the owner approves it (approved_at), signed with a
-- passkey or the phone's device key once the workspace has one, or declines it (declined_at). The owner's own grant is
-- approved as it is made. A mandate granted before this was never signed, so it waits for its owner like a request.

ALTER TABLE money_mandates ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE money_mandates ADD COLUMN IF NOT EXISTS declined_at TIMESTAMPTZ;
ALTER TABLE money_mandates ADD COLUMN IF NOT EXISTS decided_by TEXT NOT NULL DEFAULT '';
