-- 0277_approval_expiry.sql — B50.18: an approval lasts 24 hours.
--
-- An agent's request waits for its tap until expires_at — the setting's life (24 hours) after it was filed — and once
-- approved may be used until expires_at — 24 hours after the tap. Past it the row reads 'expired': it no longer holds
-- the one open approval per request, so the agent's retry files a fresh one.

ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
UPDATE agent_approvals SET expires_at = COALESCE(decided_at, created_at) + interval '24 hours' WHERE expires_at IS NULL;
ALTER TABLE agent_approvals ALTER COLUMN expires_at SET DEFAULT now() + interval '24 hours';
ALTER TABLE agent_approvals ALTER COLUMN expires_at SET NOT NULL;

ALTER TABLE agent_approvals DROP CONSTRAINT IF EXISTS agent_approvals_status_check;
ALTER TABLE agent_approvals ADD CONSTRAINT agent_approvals_status_check
    CHECK (status IN ('pending', 'approved', 'denied', 'used', 'expired'));
