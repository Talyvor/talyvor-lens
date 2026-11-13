-- 0383_agent_rules_proposals.sql — B52.41: a raise of an agent's limits, filed for the owner's passkey.
--
-- agent_rules_proposals: rules filed for an agent (by the owner's token, for a connected assistant) that loosen what
--                        is in force. Nothing applies until POST …/rules/proposals/{id}/approve, signed with the
--                        workspace's passkey or device key over the proposal's challenge; then status is 'applied'.
CREATE TABLE IF NOT EXISTS agent_rules_proposals (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    rules        JSONB NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'applied')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_agent_rules_proposals_agent ON agent_rules_proposals (agent_id) WHERE status = 'pending';
