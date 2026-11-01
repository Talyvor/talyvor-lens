-- B41.116 — lost-phone lockdown. A passkey can now be revoked as a device key can (0258): its signatures are refused
-- from `revoked_at` on and the row is kept, so a workspace that ever had a signer never falls back to approving
-- without one. While a workspace has a row in workspace_lockdowns no agent resumes and no card unfreezes; lifting it
-- takes an identity check passed after `started_at`.
ALTER TABLE workspace_passkeys ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
ALTER TABLE workspace_passkeys ADD COLUMN IF NOT EXISTS revoked_by TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS workspace_lockdowns (
    workspace_id TEXT PRIMARY KEY,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_by   TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT ''
);
