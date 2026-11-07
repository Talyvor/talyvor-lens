-- B41.116 — lost-phone lockdown. A passkey can now be revoked as a device key can (0258): its signatures are refused
-- from `revoked_at` on and the row is kept, so a workspace that ever had a signer never falls back to approving
-- without one. While a workspace has a row in workspace_lockdowns with no `lifted_at`, no agent resumes and no card
-- unfreezes; lifting it takes an identity check passed after `started_at` and sets `lifted_at`. When that check matched
-- the person checked before the lockdown, the lift also returns `reenrol_challenge`: one new key registered over it
-- enrols with no other key's signature, if the lockdown left none live, and spends it. A lockdown ends every session
-- of the workspace but the one that ran it: a JWT issued before `sessions_ended_at` is refused unless it is
-- `kept_session`. The lift goes only to the session that started the identity check (`requested_session`), and the
-- re-check matches the earlier one's date of birth as well (`date_of_birth_digest`).
ALTER TABLE workspace_passkeys ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
ALTER TABLE workspace_passkeys ADD COLUMN IF NOT EXISTS revoked_by TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS workspace_lockdowns (
    workspace_id TEXT PRIMARY KEY,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_by   TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    lifted_at    TIMESTAMPTZ,
    reenrol_challenge TEXT,
    sessions_ended_at TIMESTAMPTZ,
    kept_session TEXT NOT NULL DEFAULT ''
);

ALTER TABLE workspace_verifications ADD COLUMN IF NOT EXISTS requested_session TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_verifications ADD COLUMN IF NOT EXISTS date_of_birth_digest TEXT NOT NULL DEFAULT '';
