-- B45.24 — outside agents' HTTP message signatures (internal/agentsig, docs/agent-signatures.md).
--
-- agent_signature_nonces: each nonce a valid signature carried, kept until that signature expires, so the same nonce
-- under the same key is refused a second time. Rows past expires_at are deleted as new ones arrive.
--
-- checked_agent_operators: the signing domains Talyvor has checked belong to a business. A signature from one of
-- these domains is "Verified agent of <business>"; from any other, only "Signed by <domain>". An operator adds or
-- removes a row behind step-up; checked_by names who.
--
-- Neither table names a workspace: they describe outside agents, not customers.

CREATE TABLE IF NOT EXISTS agent_signature_nonces (
    key_id     TEXT NOT NULL CHECK (key_id <> ''),
    nonce      TEXT NOT NULL CHECK (nonce <> ''),
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (key_id, nonce)
);
CREATE INDEX IF NOT EXISTS idx_agent_signature_nonces_expires ON agent_signature_nonces (expires_at);

CREATE TABLE IF NOT EXISTS checked_agent_operators (
    domain     TEXT PRIMARY KEY CHECK (domain <> '' AND domain = lower(domain)),
    business   TEXT NOT NULL CHECK (business <> ''),
    checked_by TEXT NOT NULL CHECK (checked_by <> ''),
    checked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
