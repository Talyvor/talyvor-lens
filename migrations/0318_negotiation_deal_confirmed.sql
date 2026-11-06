-- 0318_negotiation_deal_confirmed.sql — B54.7: a deal reference holds a deal only once the counterparty confirms it
-- (internal/economy/negotiation_mandates.go).
--
-- The reference an owner types blocks no one: 0306's one-live-mandate-per-reference index let anyone squat an RFQ by
-- typing it first. A deal is now held by the mandate the counterparty's own channel confirmed — deal_confirmed_by is
-- where it came from (the registrable domain of the counterparty's signing agent, so its subdomains are one
-- counterparty), deal_confirmed_via its channel — and at most one live mandate holds a confirmed deal of one
-- counterparty. The other side of a confirmed deal is refused in internal/economy, under a lock on the deal, and only
-- when the two mandates are each other's parties.
--
-- negotiation_domains: the domains a workspace trades as, so a confirmation from its own domain counts for nothing and
-- the two parties of a deal can be told apart from strangers who typed the same reference. The owner claims a domain;
-- it is verified only when that domain's own signed agent names the workspace.
ALTER TABLE negotiation_mandates
    ADD COLUMN IF NOT EXISTS deal_confirmed_by  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deal_confirmed_via TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deal_confirmed_at  TIMESTAMPTZ;
DROP INDEX IF EXISTS idx_negotiation_mandates_live_deal;
CREATE UNIQUE INDEX IF NOT EXISTS idx_negotiation_mandates_confirmed_deal ON negotiation_mandates (deal_confirmed_by, deal_ref)
    WHERE deal_confirmed_at IS NOT NULL AND ended_at IS NULL;

CREATE TABLE IF NOT EXISTS negotiation_domains (
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    domain       TEXT NOT NULL CHECK (domain <> '' AND domain = lower(domain)),
    claimed_by   TEXT NOT NULL CHECK (claimed_by <> ''),
    claimed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at  TIMESTAMPTZ,
    PRIMARY KEY (workspace_id, domain)
);
CREATE INDEX IF NOT EXISTS idx_negotiation_domains_verified ON negotiation_domains (domain) WHERE verified_at IS NOT NULL;
