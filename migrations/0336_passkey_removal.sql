-- 0336: B55.15 — a passkey can be removed, and the workspace's last key never can.
--
-- A removed passkey signs nothing and is offered to no one, but its row stays: like a revoked phone key it still
-- counts as the workspace having a key, so removing one never leaves approvals needing no signature.
ALTER TABLE workspace_passkeys ADD COLUMN IF NOT EXISTS removed_at TIMESTAMPTZ;
ALTER TABLE workspace_passkeys ADD COLUMN IF NOT EXISTS removed_by TEXT NOT NULL DEFAULT '';
