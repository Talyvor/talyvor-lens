-- B45.23 — every agent's A2A Agent Card is signed with a key of its own (internal/a2acard, docs/agent-card.md).
--
-- Each row is the PUBLIC half of one agent's signing key. The private half is never stored: Lens derives it when it
-- signs, with HKDF from its own signing secret, the agent's id and the key's generation. kid is the key's RFC 7638
-- thumbprint. An agent has at most one key in force; rotating revokes it and the next generation takes its place.
-- revoked_at and revoked_reason are why a card signed with the key no longer verifies; a revoked key stays here so a
-- verifier is told it was revoked, not that it was never Talyvor's.

CREATE TABLE IF NOT EXISTS agent_card_keys (
    kid            TEXT PRIMARY KEY CHECK (kid <> ''),
    workspace_id   TEXT NOT NULL CHECK (workspace_id <> ''),
    agent_id       TEXT NOT NULL CHECK (agent_id <> ''),
    generation     INTEGER NOT NULL CHECK (generation > 0),
    public_key     TEXT NOT NULL CHECK (public_key <> ''),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at     TIMESTAMPTZ,
    revoked_reason TEXT NOT NULL DEFAULT '',
    UNIQUE (agent_id, generation)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_card_keys_in_force ON agent_card_keys (agent_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_agent_card_keys_workspace ON agent_card_keys (workspace_id);
