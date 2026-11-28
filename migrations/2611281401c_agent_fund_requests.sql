-- B28.73 — an owner's key asks to fund an agent; a person funds it.
--
-- agent_fund_requests: a request, from the owner's SDK or an assistant over MCP, to move amount_ulxc of the
--                      workspace's unallocated LXC to an agent. Nothing moves while it is pending. A person signed in
--                      to the workspace approves it from its link, and FundAgent then posts it once, its ref the
--                      request's idempotency_key; or they decline it, and nothing moves.
CREATE TABLE IF NOT EXISTS agent_fund_requests (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    amount_ulxc     BIGINT NOT NULL CHECK (amount_ulxc > 0),
    idempotency_key TEXT NOT NULL,
    requested_by    TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'declined')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ,
    decided_by      TEXT NOT NULL DEFAULT '',
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_agent_fund_requests_agent ON agent_fund_requests (agent_id) WHERE status = 'pending';
