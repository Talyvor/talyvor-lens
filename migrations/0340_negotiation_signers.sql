-- 0340_negotiation_signers.sql — B54.16: the agents that proved a workspace's negotiation domain are its own
-- (internal/economy/negotiation_mandates.go).
--
-- negotiation_signers: each agent that verified one of the workspace's domains, by its signing domain (the exact host)
--                      and its key's RFC 7638 thumbprint. A deal confirmation signed as one of these domains, or with
--                      one of these keys under any domain, is the workspace confirming its own deal, and holds nothing.
CREATE TABLE IF NOT EXISTS negotiation_signers (
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    domain       TEXT NOT NULL CHECK (domain <> '' AND domain = lower(domain)),
    key_id       TEXT NOT NULL CHECK (key_id <> ''),
    seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, domain, key_id)
);
