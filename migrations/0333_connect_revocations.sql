-- 0333_connect_revocations.sql — B54.13: a Connect token stops the moment its connection ends.
--
-- connect_revocations: the BFF records here when a person disconnects an assistant (client_id) or signs out everywhere
--                      (client_id ''). A Connect token issued at or before revoked_at no longer pays; one minted after
--                      it, when the person connects again, does.
-- agent_approvals.connect_client_id: the assistant whose payment filed the approval, so the approvals it leaves waiting
--                      can be capped; '' for every other caller.
CREATE TABLE IF NOT EXISTS connect_revocations (
    user_id    TEXT NOT NULL,
    client_id  TEXT NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, client_id)
);

ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS connect_client_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_agent_approvals_connect ON agent_approvals (agent_id, connect_client_id)
    WHERE status = 'pending' AND connect_client_id <> '';
