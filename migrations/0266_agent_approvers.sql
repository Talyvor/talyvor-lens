-- B45.28 — an agent approves another agent's requests within its own limits (internal/economy/agent_approvers.go,
-- docs/ap2-mandates.md).
--
-- agent_approvers: the owner let approver_agent_id approve agent_id's requests up to limit_ulxc, signing on the
-- Talyvor app (a passkey or a device key, signed_by). mandate is that permission as an AP2 v0.2 open Payment Mandate,
-- an SD-JWT signed by Talyvor; id is its jti. An agent has at most one approver in force; removing it sets revoked_at.
--
-- agent_approvals gains who approved and the proof: owner_mandate, the owner's signed approval as an AP2 v0.2 closed
-- Payment Mandate; or approved_by_agent_id with agent_evidence, Talyvor-signed evidence naming both agents and
-- approver_mandate_id, the open mandate it ran under.

CREATE TABLE IF NOT EXISTS agent_approvers (
    id                TEXT PRIMARY KEY CHECK (id <> ''),
    workspace_id      TEXT NOT NULL CHECK (workspace_id <> ''),
    agent_id          TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    approver_agent_id TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    limit_ulxc        BIGINT NOT NULL CHECK (limit_ulxc > 0),
    mandate           TEXT NOT NULL CHECK (mandate <> ''),
    signed_by         TEXT NOT NULL CHECK (signed_by <> ''),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at        TIMESTAMPTZ,
    CHECK (agent_id <> approver_agent_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_approvers_in_force ON agent_approvers (agent_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_agent_approvers_workspace ON agent_approvers (workspace_id);

ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS owner_mandate TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS approved_by_agent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS approver_mandate_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS agent_evidence TEXT NOT NULL DEFAULT '';
