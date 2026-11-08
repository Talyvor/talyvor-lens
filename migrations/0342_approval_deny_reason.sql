-- 0342_approval_deny_reason.sql — B55.18: a decline can say why (internal/economy/agent_rules.go).
--
-- deny_reason: what the workspace's owner wrote when declining, up to 200 characters; the agent's identical retry is
--              refused naming it, without a fresh approval, until the declined approval's expires_at.
ALTER TABLE agent_approvals ADD COLUMN deny_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(deny_reason) <= 200);
